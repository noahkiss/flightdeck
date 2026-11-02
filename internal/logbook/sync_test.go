package logbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Focused tests of Store.Sync, ServePull and ServePush: paging, skip, passing over a refused
// origin, stalls, the page bound, and a file restored from an older copy (logbook.md 7.6).

// direct is a Remote that serves from a replica in this process, with what each call asked.
type direct struct {
	to     *replica
	lim    Limits
	pulls  []pullCall
	pushes [][][]byte
	// beforePull runs before each pull is served, with the pull's number from 1.
	beforePull func(n int)
}

type pullCall struct {
	have Vector
	skip []string
	lim  Limits
	ans  PullAnswer
}

func (d *direct) Pull(ctx context.Context, have Vector, skip []string, lim Limits) (PullAnswer, error) {
	if d.beforePull != nil {
		d.beforePull(len(d.pulls) + 1)
	}
	a, err := d.to.ServePull(ctx, have, skip, lim)
	d.pulls = append(d.pulls, pullCall{have: have, skip: slices.Clone(skip), lim: lim, ans: a})
	return a, err
}

func (d *direct) Push(ctx context.Context, ops [][]byte) (PushAnswer, error) {
	d.pushes = append(d.pushes, ops)
	a, _, err := d.to.ServePush(ctx, ops)
	return a, err
}

func (d *direct) Page() Limits { return d.lim }

// writeN writes n tasks on r, one batch each, with a body of size bytes.
func writeN(t testing.TB, r *replica, n, size int) {
	t.Helper()
	for i := range n {
		fields := map[string]any{"title": fmt.Sprintf("%s %d", r.name, i)}
		if size > 0 {
			fields["body"] = strings.Repeat("x", size)
		}
		r.write(t, nil, create(NewID(TypeTask), TypeTask, fields))
	}
}

func pageBytes(ops [][]byte) int {
	n := 0
	for _, o := range ops {
		n += len(o)
	}
	return n
}

// One exchange pages through More by ops and by bytes, and makes exactly the pages it needs:
// a pull per page and one more only when the last said more.
func TestSyncPages(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	writeN(t, b, 10, 0)
	d := &direct{to: b, lim: Limits{Ops: 3}}
	res, err := a.Sync(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	// 3+3+3+1: the fourth answer says no more, so no fifth pull.
	if res.Pages != 4 || len(d.pulls) != 4 || res.Pulled.Stored != 10 || res.Stalled != "" {
		t.Fatalf("by ops: %+v, %d pulls", res, len(d.pulls))
	}
	for i, p := range d.pulls {
		if len(p.ans.Ops) > 3 || p.ans.More != (i < 3) || p.lim.Ops != 3 || p.lim.Bytes != PageBytes {
			t.Fatalf("pull %d: %d ops, more %v, limits %+v", i, len(p.ans.Ops), p.ans.More, p.lim)
		}
	}

	// By bytes: pages hold at most the bytes asked for, one op past it alone.
	c := newReplica(t, "C")
	writeN(t, b, 4, 3000)
	d = &direct{to: b, lim: Limits{Bytes: 7000}}
	if res, err = c.Sync(ctx, d); err != nil || res.Pulled.Stored != 14 || res.Stalled != "" {
		t.Fatalf("by bytes: %+v %v", res, err)
	}
	for i, p := range d.pulls {
		if len(p.ans.Ops) > 1 && pageBytes(p.ans.Ops) > 7000 {
			t.Fatalf("pull %d holds %d bytes", i, pageBytes(p.ans.Ops))
		}
	}
	// An op larger than the byte limit travels alone, and the exchange still ends.
	e := newReplica(t, "E")
	d = &direct{to: b, lim: Limits{Bytes: 10}}
	if res, err = e.Sync(ctx, d); err != nil || res.Pulled.Stored != 14 || res.Pages != 14 || res.Stalled != "" {
		t.Fatalf("one op a page: %+v %v", res, err)
	}
	for i, p := range d.pulls {
		if len(p.ans.Ops) > 1 {
			t.Fatalf("pull %d holds %d ops over a 10-byte limit", i, len(p.ans.Ops))
		}
	}
	// Pushes page the same way.
	f := newReplica(t, "F")
	d = &direct{to: f, lim: Limits{Ops: 4}}
	if res, err = b.Sync(ctx, d); err != nil || res.Pushed != 14 || len(d.pushes) != 4 || f.vec(t)[b.Origin()] != 14 {
		t.Fatalf("pushes: %+v %v, %d pushes", res, err, len(d.pushes))
	}
}

func (r *replica) vec(t testing.TB) Vector {
	t.Helper()
	v, err := r.Vector(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A caller never gets more than PageOps ops or PageBytes bytes in one answer, whatever it asks.
func TestServePullPageBytes(t *testing.T) {
	ctx := context.Background()
	b := newReplica(t, "B")
	writeN(t, b, 6, 200<<10) // 1.2 MiB of bodies
	a, err := b.ServePull(ctx, Vector{}, nil, Limits{Ops: 1 << 20, Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if !a.More || len(a.Ops) < 2 || pageBytes(a.Ops) > PageBytes {
		t.Fatalf("%d ops, %d bytes, more %v", len(a.Ops), pageBytes(a.Ops), a.More)
	}
	if (Limits{}).clamp() != (Limits{Ops: PageOps, Bytes: PageBytes}) {
		t.Fatal("zero limits are not the page limits")
	}
	// Served names the last seq per origin of the page.
	if a.Served[b.Origin()] != uint64(len(a.Ops)) {
		t.Fatalf("served %v for %d ops", a.Served, len(a.Ops))
	}
}

// sharedAt makes origin z shared on r: r holds z's op 1 and is then offered a different op 1.
// It returns the z replica, whose ops r now refuses.
func sharedAt(t *testing.T, r *replica) *replica {
	t.Helper()
	z := newReplica(t, "Z")
	writeN(t, z, 2, 0)
	push(t, z, r)
	clone := rawOp(t, map[string]any{"v": 1, "o": z.Origin(), "s": 1, "w": 1, "c": 0, "bl": 1, "k": "create",
		"r": "task-clonedvm", "root": "1@" + z.Origin(), "val": map[string]any{"type": "task", "fields": map[string]any{"title": "C"}}})
	if _, err := r.Apply(context.Background(), [][]byte{clone}); err != nil {
		t.Fatal(err)
	}
	st, _ := r.Status(context.Background())
	if !slices.Contains(st.Shared, z.Origin()) {
		t.Fatalf("%s does not refuse %s: %+v", r.name, z.Origin(), st)
	}
	return z
}

// ServePull sends none of the ops of an origin the caller skips.
func TestServePullSkip(t *testing.T) {
	ctx := context.Background()
	b := newReplica(t, "B")
	writeN(t, b, 2, 0)
	c := newReplica(t, "C")
	writeN(t, c, 3, 0)
	push(t, c, b)
	a, err := b.ServePull(ctx, Vector{}, []string{c.Origin()}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Ops) != 2 || a.Served[c.Origin()] != 0 || a.Served[b.Origin()] != 2 || a.Vector[c.Origin()] != 3 {
		t.Fatalf("%d ops, served %v, vector %v", len(a.Ops), a.Served, a.Vector)
	}
}

// A replica that refuses an origin names it in every pull's skip, and pushes none of the ops of
// an origin the peer refuses.
func TestSyncSkipBothWays(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	z := sharedAt(t, a) // a refuses z
	writeN(t, z, 2, 0)  // z's later ops, which b gets and a must not be sent
	push(t, z, b)
	d := &direct{to: b}
	res, err := a.Sync(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range d.pulls {
		if !slices.Contains(p.skip, z.Origin()) {
			t.Fatalf("pull %d skip %v lacks the refused origin", i, p.skip)
		}
	}
	if res.Pulled.Refused != 0 || res.Pulled.Stored != 0 {
		t.Fatalf("pulled %+v: the skipped origin's ops came", res.Pulled)
	}

	// The other way: b refuses z, a holds z's ops; a pushes only its own.
	a2, b2 := newReplica(t, "A2"), newReplica(t, "B2")
	z2 := sharedAt(t, b2)
	writeN(t, z2, 1, 0)
	push(t, z2, a2) // a2 holds z's three ops; b2 refuses z
	writeN(t, a2, 2, 0)
	d = &direct{to: b2}
	res, err = a2.Sync(ctx, d)
	if err != nil || res.Pushed != 2 || res.Stalled != "" || len(res.Passed) != 0 {
		t.Fatalf("%+v %v: want only a2's two ops pushed", res, err)
	}
}

// listRemote serves a fixed op list per origin, honouring have, skip and the op limit, and
// counts what a push moved; pushRefuses names an origin whose pushed ops it never takes.
type listRemote struct {
	ops         map[string][][]byte // by origin, seq 1 first
	lim         Limits
	ignoreSkip  bool
	pushRefuses string
	got         Vector // what pushes moved, by origin
	pulls       int
	pushes      [][][]byte
}

func (l *listRemote) vector() Vector {
	v := Vector{}
	for o, list := range l.ops {
		v[o] = uint64(len(list))
	}
	for o, n := range l.got {
		v[o] = max(v[o], n)
	}
	return v
}

func (l *listRemote) Pull(_ context.Context, have Vector, skip []string, lim Limits) (PullAnswer, error) {
	l.pulls++
	var out [][]byte
	origins := make([]string, 0, len(l.ops))
	for o := range l.ops {
		origins = append(origins, o)
	}
	slices.Sort(origins)
	for _, o := range origins {
		if !l.ignoreSkip && slices.Contains(skip, o) {
			continue
		}
		for _, raw := range l.ops[o][min(have[o], uint64(len(l.ops[o]))):] {
			if len(out) == lim.Ops {
				return PullAnswer{Ops: out, Vector: l.vector(), More: true}, nil
			}
			out = append(out, raw)
		}
	}
	return PullAnswer{Ops: out, Vector: l.vector()}, nil
}

func (l *listRemote) Push(_ context.Context, ops [][]byte) (PushAnswer, error) {
	l.pushes = append(l.pushes, ops)
	if l.got == nil {
		l.got = Vector{}
	}
	for _, raw := range ops {
		var env struct {
			O string `json:"o"`
			S uint64 `json:"s"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.O != l.pushRefuses && env.S == l.got[env.O]+1 {
			l.got[env.O] = env.S
		}
	}
	return PushAnswer{Vector: l.vector()}, nil
}

func (l *listRemote) Page() Limits { return l.lim }

// opsOf returns every op r holds of its own origin.
func opsOf(t *testing.T, r *replica) [][]byte {
	t.Helper()
	ops, _, err := r.OpsAfter(context.Background(), Vector{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// An origin whose first op the receiver refuses, followed by a full page and more, no longer
// starves the origins sorted after it: the exchange passes over it, on pulls and on pushes.
func TestSyncPassesRefusedOrigin(t *testing.T) {
	ctx := context.Background()
	first, after := newReplica(t, "A0"), newReplica(t, "B0") // "SHA256:A0" sorts before "SHA256:B0"
	writeN(t, first, 8, 0)
	writeN(t, after, 3, 0)
	bad := opsOf(t, first)
	var m map[string]any
	if err := json.Unmarshal(bad[0], &m); err != nil {
		t.Fatal(err)
	}
	m["r"] = "not an id" // a create of an id this build refuses: the receiver refuses op 1
	bad[0] = rawOp(t, m)
	r := &listRemote{ops: map[string][][]byte{first.Origin(): bad, after.Origin(): opsOf(t, after)}, lim: Limits{Ops: 3}}
	x := newReplica(t, "X")
	res, err := x.Sync(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if v := x.vec(t); v[after.Origin()] != 3 || v[first.Origin()] != 0 || res.Stalled != "" ||
		!slices.Equal(res.Passed, []string{first.Origin()}) || r.pulls != 2 {
		t.Fatalf("vector %v, %+v, %d pulls: want B0's ops past A0's refused one", v, res, r.pulls)
	}

	// Pushes: the peer takes none of A0's ops; B0's still go, in the next push.
	y := newReplica(t, "Y")
	push(t, first, y)
	push(t, after, y)
	r = &listRemote{ops: map[string][][]byte{}, lim: Limits{Ops: 3}, pushRefuses: first.Origin()}
	res, err = y.Sync(ctx, r)
	if err != nil || res.Stalled != "" || !slices.Equal(res.Passed, []string{first.Origin()}) || r.got[after.Origin()] != 3 || len(r.pushes) != 2 {
		t.Fatalf("%+v %v, got %v in %d pushes", res, err, r.got, len(r.pushes))
	}
}

// A peer that ignores skip and sends the same refused ops again stalls the pull after the
// second page; the exchange ends instead of spinning.
func TestSyncStalls(t *testing.T) {
	ctx := context.Background()
	first := newReplica(t, "A0")
	writeN(t, first, 5, 0)
	bad := opsOf(t, first)
	var m map[string]any
	_ = json.Unmarshal(bad[0], &m)
	m["r"] = "not an id"
	bad[0] = rawOp(t, m)
	r := &listRemote{ops: map[string][][]byte{first.Origin(): bad}, lim: Limits{Ops: 3}, ignoreSkip: true}
	x := newReplica(t, "X")
	writeN(t, x, 1, 0) // a vector that is not empty
	x.maxPages = 10
	res, err := x.Sync(ctx, r)
	if err != nil || res.Stalled != "pull" || r.pulls != 2 {
		t.Fatalf("%+v %v after %d pulls: want a stall on the second", res, err, r.pulls)
	}
}

// One exchange makes at most the page bound of pulls, then of pushes; the next carries on.
func TestSyncPageBound(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	writeN(t, b, 7, 0)
	writeN(t, a, 7, 0)
	a.maxPages = 3
	d := &direct{to: b, lim: Limits{Ops: 2}}
	res, err := a.Sync(ctx, d)
	if err != nil || res.Stalled != "push" || len(d.pulls) != 3 || len(d.pushes) != 3 || res.Pulled.Stored != 6 || res.Pushed != 6 {
		t.Fatalf("%+v %v, %d pulls, %d pushes", res, err, len(d.pulls), len(d.pushes))
	}
	if res, err = a.Sync(ctx, d); err != nil || a.vec(t)[b.Origin()] != 7 || b.vec(t)[a.Origin()] != 7 || res.Stalled != "" {
		t.Fatalf("the next exchange: %+v %v", res, err)
	}
}

// copyFile copies a closed store's file.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// A file restored from an older copy, with the same device id: once a peer's answer shows it
// holds newer own ops, writes are refused until sync brings them back, and then go on with the
// next seq. No replica marks the origin shared; both hold the same op bytes.
func TestRestoredFileWaitsForItsOps(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	writeN(t, a, 1, 0)
	if _, err := a.Sync(ctx, &direct{to: b}); err != nil {
		t.Fatal(err)
	}
	backup := a.path + ".backup"
	_ = a.Close()
	copyFile(t, a.path, backup)
	a = openReplica(t, "A", a.path)
	writeN(t, a, 2, 0)
	if _, err := a.Sync(ctx, &direct{to: b}); err != nil {
		t.Fatal(err)
	}
	// The restore.
	_ = a.Close()
	for _, ext := range []string{"-wal", "-shm"} {
		_ = os.Remove(a.path + ext)
	}
	copyFile(t, backup, a.path)
	a = openReplica(t, "A", a.path)
	if a.vec(t)[a.Origin()] != 1 {
		t.Fatalf("restored vector %v", a.vec(t))
	}
	var during error
	var st Status
	d := &direct{to: b, lim: Limits{Ops: 1}, beforePull: func(n int) {
		if n == 2 { // the first answer named seq 3 and brought seq 2
			st, _ = a.Status(ctx)
			_, during = a.Write(ctx, Batch{Steps: []Step{create(NewID(TypeTask), TypeTask, map[string]any{"title": "too soon"})}})
		}
	}}
	if _, err := a.Sync(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(during, ErrRestoring) || st.Restoring != 3 {
		t.Fatalf("a write while restoring: %v, status %+v", during, st)
	}
	if !slices.ContainsFunc(a.logged(), func(l string) bool { return strings.Contains(l, "RESTORED FILE") }) {
		t.Fatalf("no loud log: %q", a.logged())
	}
	if st, _ := a.Status(ctx); st.Restoring != 0 {
		t.Fatalf("still restoring after the sync: %+v", st)
	}
	res := a.write(t, nil, create(NewID(TypeTask), TypeTask, map[string]any{"title": "after"}))
	if res.Ops[0].Seq != 4 {
		t.Fatalf("the next write took seq %d, want 4", res.Ops[0].Seq)
	}
	if _, err := a.Sync(ctx, &direct{to: b}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*replica{a, b} {
		if st, _ := r.Status(ctx); len(st.Shared) != 0 || st.Vector[a.Origin()] != 4 {
			t.Fatalf("%s: %+v", r.name, st)
		}
	}
	if dumpOps(t, a.Store) != dumpOps(t, b.Store) {
		t.Fatal("the replicas hold different ops")
	}
}

// The serving side learns its own seq from a pull's have, and from own ops pushed to it.
func TestRestoredFileServing(t *testing.T) {
	ctx := context.Background()
	b := newReplica(t, "B")
	writeN(t, b, 2, 0)
	if _, err := b.ServePull(ctx, Vector{b.Origin(): 5}, nil, Limits{}); err != nil {
		t.Fatal(err)
	}
	st, _ := b.Status(ctx)
	_, err := b.Write(ctx, Batch{Steps: []Step{create(NewID(TypeTask), TypeTask, map[string]any{"title": "x"})}})
	if st.Restoring != 5 || !errors.Is(err, ErrRestoring) {
		t.Fatalf("status %+v, write %v", st, err)
	}
	c := newReplica(t, "C")
	writeN(t, c, 3, 0)
	ops := opsOf(t, c)
	d := newReplica(t, "D")
	_ = d.Close()
	// A store on c's file under another name is c restored with fewer ops: open a fresh file for
	// origin C and push c's op 2 and 3 to it, past its gap.
	e := openReplica(t, "C", d.path+".c")
	if _, _, err := e.ServePush(ctx, ops[1:]); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(ctx); st.Restoring != 3 {
		t.Fatalf("after a push of own ops past a gap: %+v", st)
	}
}

// The start hold: a write waits while the file holds writes, and goes on when the hold ends.
func TestHoldWrites(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	if err := a.HoldWrites(ctx, a.clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Write(ctx, Batch{Steps: []Step{create(NewID(TypeTask), TypeTask, map[string]any{"title": "x"})}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the write did not wait: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := a.HoldWrites(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write still waits after the hold ended")
	}
	// A hold that has run out holds nothing.
	_ = a.HoldWrites(ctx, a.clock.Now().Add(-time.Second))
	start := time.Now()
	a.write(t, nil, create(NewID(TypeTask), TypeTask, map[string]any{"title": "y"}))
	if time.Since(start) > time.Second {
		t.Fatal("a past hold made the write wait")
	}
	// A set-aside file not copied in shows in Status until cleared.
	_ = a.NoteUncopied(ctx, "logbook.db.other-origin-1", errors.New("disk full"))
	if st, _ := a.Status(ctx); !slices.Equal(st.Uncopied, []string{"logbook.db.other-origin-1: disk full"}) {
		t.Fatalf("%+v", st.Uncopied)
	}
	_ = a.NoteUncopied(ctx, "logbook.db.other-origin-1", nil)
	if st, _ := a.Status(ctx); len(st.Uncopied) != 0 {
		t.Fatalf("%+v", st.Uncopied)
	}
}
