package logbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
)

// Paging limits of sync (7.1): one pull answer or one push carries at most PageOps ops or
// PageBytes of op bytes, except that a single op larger than the byte limit still travels alone.
// 1 MiB of ops is about 1.4 MiB of base64 on the link: a slow link moves it within one page's
// time, and it stays far under the control connection's 8 MiB backlog (7.6, Pages). A caller
// may ask for less (Limits); never for more.
const (
	PageOps   = 2000
	PageBytes = 1 << 20
)

// maxSyncPages bounds the pages of one Sync in each direction, so a peer that keeps writing
// cannot hold an exchange open for ever. The next exchange carries on where this one stopped.
const maxSyncPages = 1000

// Limits is the most one page may hold: ops and bytes of ops. A zero field, or one above PageOps
// or PageBytes, means the page limit.
type Limits struct {
	Ops   int
	Bytes int
}

// clamp puts l within the page limits.
func (l Limits) clamp() Limits {
	if l.Ops <= 0 || l.Ops > PageOps {
		l.Ops = PageOps
	}
	if l.Bytes <= 0 || l.Bytes > PageBytes {
		l.Bytes = PageBytes
	}
	return l
}

// PullAnswer is the answer to a pull: the ops the caller lacks, the answering replica's version
// vector, whether more pages follow, and the origins the answering replica refuses.
type PullAnswer struct {
	Ops    [][]byte
	Vector Vector
	More   bool
	// Skip lists the origins the answering replica refuses (two hosts share them, 7.4): the
	// caller sends none of their ops.
	Skip []string
	// Served is, per origin in Ops, the last seq: once the page lands, the caller holds every
	// op up to it. Only the answering side fills it; it does not travel.
	Served Vector
}

// PushAnswer is the answer to a push: the receiving replica's version vector after it applied
// the ops, and the origins it refuses.
type PushAnswer struct {
	Vector Vector
	Skip   []string
}

// Remote is the far end of one sync link: the replica this one exchanges ops with (7.1).
// gff-hab gives the peer link's logbook.pull and logbook.push; the property tests give a link
// that drops, delays, repeats and reorders.
type Remote interface {
	// Pull asks for one page, of at most lim, of the ops have lacks, none of skip's origins.
	Pull(ctx context.Context, have Vector, skip []string, lim Limits) (PullAnswer, error)
	Push(ctx context.Context, ops [][]byte) (PushAnswer, error)
	// Page is the most the next page over this link may hold, either way. A link that turns
	// slow asks for smaller pages.
	Page() Limits
}

// SyncResult says what one Sync moved.
type SyncResult struct {
	Pulled  ApplyResult // what the pulled pages did here, summed
	Pages   int         // pull answers and pushes made
	Pushed  int         // ops sent to the remote
	Changed []string    // record ids the pulled ops may have changed, sorted
	// Stalled names a direction that stopped before the other side ran out of ops: a page that
	// moved no vector and found no new origin to pass over, or the page bound.
	Stalled string
	// Passed lists, sorted, the origins this exchange passed over: the first op of theirs a page
	// carried was refused on the receiving side, so later pages skipped them and other origins
	// still moved (7.6, Refused origins).
	Passed []string
}

// ErrPageTooLarge refuses a push of more than PageOps ops or PageBytes bytes.
var ErrPageTooLarge = errors.New("logbook: a push holds more than one page of ops")

// ServePull answers a pull (logbook.pull, 7.1): the ops the caller lacks by its vector have, at
// most one page of lim, in per-origin seq order. The caller names in skip the origins it
// refuses; none of their ops are sent. have also says how far the caller holds this replica's
// own origin (NoteOwnSeen).
func (s *Store) ServePull(ctx context.Context, have Vector, skip []string, lim Limits) (PullAnswer, error) {
	if err := s.noteOwnIn(ctx, have); err != nil {
		return PullAnswer{}, err
	}
	lim = lim.clamp()
	ops, served, more, err := s.opsAfter(ctx, skipping(have, skip), lim.Ops, lim.Bytes)
	if err != nil {
		return PullAnswer{}, err
	}
	st, err := s.syncState(ctx)
	if err != nil {
		return PullAnswer{}, err
	}
	return PullAnswer{Ops: ops, Vector: st.Vector, More: more, Skip: st.Shared, Served: served}, nil
}

// ServePush takes a push (logbook.push, 7.1): it applies the ops and answers with this
// replica's vector. A push over one page is refused whole.
func (s *Store) ServePush(ctx context.Context, ops [][]byte) (PushAnswer, ApplyResult, error) {
	if !OnePage(ops) {
		return PushAnswer{}, ApplyResult{}, ErrPageTooLarge
	}
	res, err := s.Apply(ctx, ops)
	if err != nil {
		return PushAnswer{}, res, err
	}
	if res.ownMax > 0 {
		if err := s.NoteOwnSeen(ctx, res.ownMax); err != nil {
			return PushAnswer{}, res, err
		}
	}
	st, err := s.syncState(ctx)
	if err != nil {
		return PushAnswer{}, res, err
	}
	return PushAnswer{Vector: st.Vector, Skip: st.Shared}, res, nil
}

// OnePage reports whether ops fit one page: at most PageOps ops and PageBytes bytes, or one op.
func OnePage(ops [][]byte) bool {
	if len(ops) > PageOps {
		return false
	}
	if len(ops) <= 1 {
		return true
	}
	n := 0
	for _, o := range ops {
		n += len(o)
	}
	return n <= PageBytes
}

// skipping is have with every origin of skip raised past any seq, so no op of theirs is sent.
func skipping(have Vector, skip []string) Vector {
	want := Vector{}
	maps.Copy(want, have)
	for _, o := range skip {
		want[o] = math.MaxUint64
	}
	return want
}

// syncState is the vector and the refused origins, read in one go.
func (s *Store) syncState(ctx context.Context) (Status, error) {
	var st Status
	var err error
	if st.Shared, err = queryStrings(ctx, s.db, `SELECT origin FROM origins WHERE shared = 1 ORDER BY origin`); err != nil {
		return st, err
	}
	st.Vector, err = s.Vector(ctx)
	return st, err
}

// Sync makes one exchange with r, the way the hab that holds a link drives both directions
// (7.1): it pulls page by page until the remote has nothing more, then pushes what the remote
// lacks by the vector it answered with, page by page. Ops travel from every origin, so replicas
// that never link converge through any replica both reach (7.2). Repeats are harmless: ops are
// keyed by op id.
//
// An origin whose first op in a page the receiving side refused (its file's constraints, say)
// moves no vector, and every later page would carry the same ops again and starve the origins
// sorted after it. So Sync passes over such an origin for the rest of the exchange: it joins
// skip on a pull, and is left out of later pushes. A page that moves no vector and finds no new
// origin to pass over ends that direction (Stalled); the next exchange tries again.
func (s *Store) Sync(ctx context.Context, r Remote) (res SyncResult, err error) {
	maxPages := s.maxPages
	if maxPages <= 0 {
		maxPages = maxSyncPages
	}
	changed := map[string]bool{}
	passed := map[string]bool{}
	var peer PullAnswer
	for pulls := 1; ; pulls++ {
		st, err := s.syncState(ctx)
		if err != nil {
			return res, err
		}
		a, err := r.Pull(ctx, st.Vector, union(st.Shared, passed), r.Page().clamp())
		if err != nil {
			return res, fmt.Errorf("pull: %w", err)
		}
		res.Pages++
		peer = a
		if err := s.noteOwnIn(ctx, a.Vector); err != nil {
			return res, err
		}
		if len(a.Ops) == 0 {
			break
		}
		ar, err := s.Apply(ctx, a.Ops)
		if err != nil {
			return res, err
		}
		res.Pulled.Stored += ar.Stored
		res.Pulled.Repeats += ar.Repeats
		res.Pulled.Refused += ar.Refused
		for _, id := range ar.Changed {
			changed[id] = true
		}
		if !a.More {
			break
		}
		after, err := s.Vector(ctx)
		if err != nil {
			return res, err
		}
		if !progress(st.Vector, after, pageOrigins(a.Ops), passed) {
			res.Stalled = "pull"
			break
		}
		if pulls >= maxPages {
			res.Stalled = "pull"
			break
		}
	}
	res.Changed = sortedKeys(changed)
	// Set on every return below: the named result carries it.
	defer func() { res.Passed = sortedKeys(passed) }()
	theirs, skip := peer.Vector, peer.Skip
	pushedPassed := map[string]bool{}
	for pushes := 0; pushes < maxPages; pushes++ {
		lim := r.Page().clamp()
		ops, last, _, err := s.opsAfter(ctx, skipping(theirs, union(skip, pushedPassed)), lim.Ops, lim.Bytes)
		if err != nil {
			return res, err
		}
		if len(ops) == 0 {
			return res, nil
		}
		a, err := r.Push(ctx, ops)
		if err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
		res.Pages++
		res.Pushed += len(ops)
		if err := s.noteOwnIn(ctx, a.Vector); err != nil {
			return res, err
		}
		in := map[string]bool{}
		for o := range last {
			in[o] = true
		}
		moved := progress(theirs, a.Vector, in, pushedPassed)
		for o := range pushedPassed {
			passed[o] = true
		}
		if !moved && slices.Equal(skip, a.Skip) {
			res.Stalled = "push"
			return res, nil
		}
		theirs, skip = a.Vector, a.Skip
	}
	res.Stalled = "push"
	return res, nil
}

// progress reports whether a page moved: vector after holds a seq above before's for some
// origin, or an origin of the page (in) moved no seq and is newly added to passed.
func progress(before, after Vector, in, passed map[string]bool) bool {
	moved := grew(before, after)
	for o := range in {
		if after[o] <= before[o] && !passed[o] {
			passed[o] = true
			moved = true
		}
	}
	return moved
}

// pageOrigins reads the origin of each op of a page from its envelope alone, so an op the
// receiving side could not decode whole still names its origin.
func pageOrigins(ops [][]byte) map[string]bool {
	out := map[string]bool{}
	for o := range PageVector(ops) {
		out[o] = true
	}
	return out
}

// union is a and the keys of b, sorted, without repeats.
func union(a []string, b map[string]bool) []string {
	out := slices.Clone(a)
	for o := range b {
		out = append(out, o)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// grew reports whether vector b holds a seq above a's for some origin.
func grew(a, b Vector) bool {
	for o, n := range b {
		if n > a[o] {
			return true
		}
	}
	return false
}

// PageVector is, per origin, the highest seq among ops, read from their envelopes: what a peer
// that sent them holds at least.
func PageVector(ops [][]byte) Vector {
	v := Vector{}
	for _, raw := range ops {
		var env struct {
			Origin string `json:"o"`
			Seq    uint64 `json:"s"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Origin != "" {
			v[env.Origin] = max(v[env.Origin], env.Seq)
		}
	}
	return v
}
