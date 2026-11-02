package logbook

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
)

// Paging limits of sync (7.1): one pull answer or one push carries at most PageOps ops or
// PageBytes of op bytes, except that a single op larger than PageBytes still travels alone.
const (
	PageOps   = 2000
	PageBytes = 4 << 20
)

// maxSyncPages bounds the pages of one Sync in each direction, so a peer that keeps writing
// cannot hold an exchange open for ever. The next exchange carries on where this one stopped.
const maxSyncPages = 1000

// PullAnswer is the answer to a pull: the ops the caller lacks, the answering replica's version
// vector, whether more pages follow, and the origins the answering replica refuses.
type PullAnswer struct {
	Ops    [][]byte
	Vector Vector
	More   bool
	// Skip lists the origins the answering replica refuses (two hosts share them, 7.4): the
	// caller sends none of their ops.
	Skip []string
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
	Pull(ctx context.Context, have Vector, skip []string) (PullAnswer, error)
	Push(ctx context.Context, ops [][]byte) (PushAnswer, error)
}

// SyncResult says what one Sync moved.
type SyncResult struct {
	Pulled  ApplyResult // what the pulled pages did here, summed
	Pages   int         // pull answers and pushes made
	Pushed  int         // ops sent to the remote
	Changed []string    // record ids the pulled ops may have changed, sorted
	// Stalled names a direction that stopped because a page moved no vector: a peer that keeps
	// sending ops this replica refuses, or one that refuses what this one sends.
	Stalled string
}

// ErrPageTooLarge refuses a push of more than PageOps ops or PageBytes bytes.
var ErrPageTooLarge = errors.New("logbook: a push holds more than one page of ops")

// ServePull answers a pull (logbook.pull, 7.1): the ops the caller lacks by its vector have, at
// most one page, in per-origin seq order. The caller names in skip the origins it refuses; none
// of their ops are sent.
func (s *Store) ServePull(ctx context.Context, have Vector, skip []string) (PullAnswer, error) {
	want := Vector{}
	maps.Copy(want, have)
	for _, o := range skip {
		want[o] = math.MaxUint64
	}
	ops, more, err := s.OpsAfter(ctx, want, PageOps, PageBytes)
	if err != nil {
		return PullAnswer{}, err
	}
	st, err := s.syncState(ctx)
	if err != nil {
		return PullAnswer{}, err
	}
	return PullAnswer{Ops: ops, Vector: st.Vector, More: more, Skip: st.Shared}, nil
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
// keyed by op id. A page that moves no vector ends that direction (Stalled); the next exchange
// tries again.
func (s *Store) Sync(ctx context.Context, r Remote) (SyncResult, error) {
	var res SyncResult
	changed := map[string]bool{}
	var peer PullAnswer
	for {
		st, err := s.syncState(ctx)
		if err != nil {
			return res, err
		}
		a, err := r.Pull(ctx, st.Vector, st.Shared)
		if err != nil {
			return res, fmt.Errorf("pull: %w", err)
		}
		res.Pages++
		peer = a
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
		if !grew(st.Vector, after) {
			res.Stalled = "pull"
			break
		}
		if res.Pages >= maxSyncPages {
			res.Stalled = "pull"
			break
		}
	}
	res.Changed = sortedKeys(changed)
	theirs, skip := peer.Vector, peer.Skip
	for pushes := 0; pushes < maxSyncPages; pushes++ {
		want := Vector{}
		maps.Copy(want, theirs)
		for _, o := range skip {
			want[o] = math.MaxUint64
		}
		ops, _, err := s.OpsAfter(ctx, want, PageOps, PageBytes)
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
		if !grew(theirs, a.Vector) && slices.Equal(skip, a.Skip) {
			res.Stalled = "push"
			return res, nil
		}
		theirs, skip = a.Vector, a.Skip
	}
	res.Stalled = "push"
	return res, nil
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
