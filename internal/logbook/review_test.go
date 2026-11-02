package logbook

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"slices"
	"testing"
)

// Tests for the faults the stage 2 review found: one per fault, each failing before its fix.

// peerOp encodes an op from origin at seq, a batch of one, at the given stamp.
func peerOp(t testing.TB, origin string, seq uint64, wall int64, counter uint32, m map[string]any) []byte {
	t.Helper()
	m["v"], m["o"], m["s"], m["w"], m["c"], m["bl"] = 1, origin, seq, wall, counter, seq
	return rawOp(t, m)
}

func peerCreate(t testing.TB, origin string, seq uint64, wall int64, id, title string) []byte {
	t.Helper()
	return peerOp(t, origin, seq, wall, 0, map[string]any{"k": "create", "r": id,
		"root": OpID{Origin: origin, Seq: seq}.String(),
		"val":  map[string]any{"type": "task", "fields": map[string]any{"title": title, "status": "open"}}})
}

// A create that claims a collision id (a second dash) is refused: only a fold makes such ids, so
// a peer claiming one would take an id a fold gives another record. The rest of the page lands.
func TestApplyRefusesClaimedCollisionID(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	w := at("09:00").UnixMilli()
	page := [][]byte{
		peerCreate(t, "SHA256:p1", 1, w, "task-aaaaaaaa", "first"),
		peerCreate(t, "SHA256:p2", 1, w+1, "task-aaaaaaaa", "second"),
		peerCreate(t, "SHA256:p3", 1, w+2, "task-aaaaaaaa-2", "claims the fold's id"),
		peerCreate(t, "SHA256:p4", 1, w+3, "task-bbbbbbbb", "valid"),
	}
	res, err := r.Apply(ctx, page)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Stored != 3 || res.Refused != 1 {
		t.Fatalf("%+v, want three stored and the claim refused", res)
	}
	if got := r.get(t, "task-aaaaaaaa-2"); got.Title != "second" {
		t.Fatalf("task-aaaaaaaa-2 is %q, want the second create", got.Title)
	}
	r.get(t, "task-bbbbbbbb")
	if ValidID("task-aaaaaaaa-2") || !ValidID("task-aaaaaaaa") {
		t.Fatal("ValidID must refuse an id with a collision suffix")
	}
}

// Seqs at or above 2^63 cannot be stored: an op that carries one, as its seq, its batch end, its
// root, its target or a prev, is refused, and a valid op in the same page still lands.
func TestApplyRefusesHugeSeq(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	w := at("09:00").UnixMilli()
	const huge = uint64(1) << 63
	bad := [][]byte{
		rawOp(t, map[string]any{"v": 1, "o": "SHA256:p1", "s": huge, "w": w, "c": 0, "bl": huge, "k": "create",
			"r": "task-cccccccc", "root": OpID{Origin: "SHA256:p1", Seq: huge}.String(),
			"val": map[string]any{"type": "task", "fields": map[string]any{"title": "x"}}}),
		rawOp(t, map[string]any{"v": 1, "o": "SHA256:p2", "s": 1, "w": w, "c": 0, "bl": huge, "k": "append",
			"r": "task-cccccccc", "root": "1@SHA256:p2", "f": "notes", "val": "n"}),
		peerOp(t, "SHA256:p3", 1, w, 0, map[string]any{"k": "append", "r": "task-cccccccc",
			"root": "9223372036854775808@SHA256:p1", "f": "notes", "val": "n"}),
		peerOp(t, "SHA256:p5", 1, w, 0, map[string]any{"k": "forget", "r": "task-cccccccc", "root": "1@SHA256:p4",
			"t": "18446744073709551615@SHA256:p4"}),
		peerOp(t, "SHA256:p6", 1, w, 0, map[string]any{"k": "set", "r": "task-cccccccc", "root": "1@SHA256:p4",
			"f": "title", "val": "y", "prev": []string{"9223372036854775808@SHA256:p4"}}),
	}
	page := append(slices.Clone(bad), peerCreate(t, "SHA256:p4", 1, w, "task-dddddddd", "valid"))
	res, err := r.Apply(ctx, page)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Stored != 1 || res.Refused != len(bad) {
		t.Fatalf("%+v, want the valid create stored and %d refused", res, len(bad))
	}
	r.get(t, "task-dddddddd")
	if _, err := r.Write(ctx, Batch{Steps: []Step{{Kind: KindForget, Target: OpID{Origin: "SHA256:p4", Seq: huge}}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forget of a huge seq: %v, want not found", err)
	}
}

// An op of this replica's own origin from a peer is stored only when it extends the gap-free run;
// one past a gap is refused, so local writes keep folding. Ops that do extend the run move the
// next seq on, so a replica restored from a peer never reuses a seq (M9).
func TestOwnOriginOpsFromPeer(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	forged := peerOp(t, a.Origin(), 1000, at("09:00").UnixMilli(), 0, map[string]any{"k": "append", "r": id,
		"root": "1@" + a.Origin(), "f": "notes", "val": "forged"})
	if res, err := a.Apply(ctx, [][]byte{forged}); err != nil || res.Refused != 1 {
		t.Fatalf("own op past a gap: %+v %v, want refused", res, err)
	}
	k := NewID(TypeTask)
	res := a.write(t, nil, create(k, TypeTask, map[string]any{"title": "K"}))
	if res.Ops[0].Seq != 2 {
		t.Fatalf("next write at seq %d, want 2", res.Ops[0].Seq)
	}
	a.get(t, k)

	// A's file is lost; a new file with the same origin gets A's ops back from B.
	b := newReplica(t, "B")
	a.write(t, nil, note(id, "n1"), note(id, "n2"))
	push(t, a, b)
	a2 := openReplica(t, "A", t.TempDir()+"/logbook.db")
	pushPages(t, b, a2, 1)
	got := a2.write(t, nil, note(id, "after restore"))
	if got.Ops[0].Seq != 5 {
		t.Fatalf("restored replica wrote seq %d, want 5", got.Ops[0].Seq)
	}
	push(t, a2, b)
	if st, _ := b.Status(ctx); len(st.Shared) != 0 {
		t.Fatalf("B marked %v shared: the restored replica reused a seq", st.Shared)
	}
	if rec := b.get(t, id); len(rec.Notes) != 3 {
		t.Fatalf("B notes %+v, want three", rec.Notes)
	}

	// Another host claims A's origin: A refuses to write.
	clone := peerOp(t, a.Origin(), 1, 1, 0, map[string]any{"k": "create", "r": "task-clonedvm",
		"root": "1@" + a.Origin(), "val": map[string]any{"type": "task", "fields": map[string]any{"title": "C"}}})
	if res, _ := a.Apply(ctx, [][]byte{clone}); res.Refused != 1 {
		t.Fatalf("%+v, want the clone refused", res)
	}
	if _, err := a.Write(ctx, Batch{Steps: []Step{note(id, "x")}}); !errors.Is(err, ErrSharedOrigin) {
		t.Fatalf("write with a shared origin: %v, want ErrSharedOrigin", err)
	}
}

// fileHolds reports which of the store's files (db, wal, shm) hold the text.
func fileHolds(t *testing.T, path, text string) []string {
	t.Helper()
	var out []string
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(text)) {
			out = append(out, p)
		}
	}
	return out
}

// After a forget no file of the store holds the text: not the WAL, not page slack of the db, not
// the FTS index (5.7, P9). Also when the target arrives after its forget, unredacted (M10, M19).
func TestForgetLeavesNoTextInFiles(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}), add(id, "tags", "keep"))
	const secret = "zqxvorpalsecretzqx"
	n := a.write(t, nil, note(id, "the key is "+secret+" do not share")).Ops[0]
	a.write(t, nil, note(id, "other words"))
	before, _, _ := a.OpsAfter(ctx, Vector{}, 0, 0) // the note's unredacted bytes, as a stale peer holds them
	if len(fileHolds(t, a.path, secret)) == 0 {
		t.Fatal("the probe never saw the secret in the files")
	}
	a.write(t, nil, Step{Kind: KindForget, Target: n})
	if got := fileHolds(t, a.path, secret); len(got) != 0 {
		t.Fatalf("after the forget, files hold the secret: %v", got)
	}

	// C gets the forget first, then the note's unredacted bytes.
	c := newReplica(t, "C")
	after, _, _ := a.OpsAfter(ctx, Vector{}, 0, 0)
	forget := after[len(after)-1]
	if _, err := c.Apply(ctx, [][]byte{forget}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range before {
		if _, err := c.Apply(ctx, [][]byte{raw}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := c.get(t, id); len(rec.Notes) != 2 {
		t.Fatalf("C notes %+v", rec.Notes)
	}
	if got := fileHolds(t, c.path, secret); len(got) != 0 {
		t.Fatalf("C's files hold the secret: %v", got)
	}
	if dump(t, a.Store) != dump(t, c.Store) {
		t.Fatal("A and C fold differently")
	}
}

// The counter at its top carries into the wall, so the next local stamp still orders after
// everything seen (5.1): for a local tick and for a stamp observed from a peer.
func TestHLCCounterWrap(t *testing.T) {
	last := HLC{Wall: 5000, Counter: math.MaxUint32, Origin: "SHA256:a"}
	next := tick(last, 4000, "SHA256:a")
	if next.Compare(last) <= 0 {
		t.Fatalf("tick after %v gave %v", last, next)
	}
	o := observe(HLC{Wall: 1, Origin: "SHA256:a"}, HLC{Wall: 5000, Counter: math.MaxUint32, Origin: "SHA256:z"})
	if n := tick(o, 4000, "SHA256:a"); n.Compare(HLC{Wall: 5000, Counter: math.MaxUint32, Origin: "SHA256:z"}) <= 0 {
		t.Fatalf("tick after an observed top counter gave %v", n)
	}

	ctx := context.Background()
	a := newReplica(t, "A")
	a.clock.Set(at("09:00"))
	id := NewID(TypeTask)
	made := a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	w := at("09:01").UnixMilli()
	peer := peerOp(t, "SHA256:p1", 1, w, math.MaxUint32, map[string]any{"k": "set", "r": id,
		"root": made.Ops[0].String(), "f": "title", "val": "from the peer", "prev": []string{made.Ops[0].String()}})
	if _, err := a.Apply(ctx, [][]byte{peer}); err != nil {
		t.Fatal(err)
	}
	a.write(t, nil, set(id, "title", "local, after the peer"))
	if rec := a.get(t, id); rec.Title != "local, after the peer" || len(flagsOf(rec, FlagConflict)) != 0 {
		t.Fatalf("title %q flags %+v: the local write ordered before the peer's", rec.Title, rec.Flags)
	}
}

// A writer that sees a decision cannot write a second one (5.6); only concurrent decisions flag.
func TestSecondDecisionRefused(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	id := NewID(TypeProposal)
	r.write(t, nil, create(id, TypeProposal, map[string]any{"title": "P", "target": "record"}))
	r.write(t, nil, set(id, "status", "approved"), set(id, "decided_by", "phone"))
	for _, st := range []Step{set(id, "status", "rejected"), set(id, "status", "pending"), set(id, "decided_by", "mac")} {
		if _, err := r.Write(ctx, Batch{Steps: []Step{st}}); !isInvalid(err) {
			t.Fatalf("%v on a decided proposal: %v, want invalid", st, err)
		}
	}
	if rec := r.get(t, id); rec.Status != "approved" || len(rec.Flags) != 0 {
		t.Fatalf("%+v", rec)
	}
}

// A forget from a peer that names a value with no text redacts nothing.
func TestPeerForgetOfNonTextIgnored(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	pri := a.write(t, nil, set(id, "priority", 2)).Ops[0]
	tag := a.write(t, nil, add(id, "tags", "x")).Ops[0]
	rm := a.write(t, nil, remove(id, "tags", "x")).Ops[0]
	w := at("10:00").UnixMilli()
	var page [][]byte
	for i, target := range []OpID{pri, rm} {
		page = append(page, peerOp(t, "SHA256:p1", uint64(i+1), w, 0, map[string]any{"k": "forget", "r": id,
			"root": "1@" + a.Origin(), "t": target.String()}))
	}
	if _, err := a.Apply(ctx, page); err != nil {
		t.Fatal(err)
	}
	rec := a.get(t, id)
	if str(rec.Fields["priority"].Value) != "2" || len(rec.Tags) != 0 {
		t.Fatalf("priority %s tags %v: a forget of a value with no text changed it", rec.Fields["priority"].Value, rec.Tags)
	}
	_ = tag
}

// A redaction moves the record's revision, so a reader sees the record changed.
func TestRevisionMovesOnRedaction(t *testing.T) {
	r := newReplica(t, "A")
	id := NewID(TypeTask)
	r.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	n := r.write(t, nil, note(id, "secret")).Ops[0]
	rev := r.get(t, id).Revision
	r.write(t, nil, Step{Kind: KindForget, Target: n})
	if got := r.get(t, id).Revision; got <= rev {
		t.Fatalf("revision %d after the forget, want above %d", got, rev)
	}
}

// Two creates of one id: a forget of the second record's create names the folded id, and both
// Apply and Changes report both ids when a later-arriving create renames a record.
func TestCollisionIDsReported(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:01"))
	b.clock.Set(at("09:00"))
	const id = "task-k3m9q2xz"
	made := a.write(t, nil, create(id, TypeTask, map[string]any{"title": "on A"}))
	b.write(t, nil, create(id, TypeTask, map[string]any{"title": "on B"}))
	_, cursor, err := a.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops, _, _ := b.OpsAfter(ctx, Vector{}, 0, 0)
	res, err := a.Apply(ctx, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Changed, []string{id, id + "-2"}) {
		t.Fatalf("Changed %v, want both ids", res.Changed)
	}
	ids, _, err := a.Changes(ctx, cursor)
	if err != nil || !slices.Equal(ids, []string{id, id + "-2"}) {
		t.Fatalf("Changes %v %v, want both ids", ids, err)
	}
	if rec := a.get(t, id+"-2"); rec.Root != made.Ops[0] {
		t.Fatalf("%s-2 is %v, want A's record", id, rec.Root)
	}
	got := a.write(t, nil, Step{Kind: KindForget, Target: made.Ops[0]})
	if !slices.Equal(got.Records, []string{id + "-2"}) {
		t.Fatalf("forget names %v, want the folded id", got.Records)
	}
}

// Edited after delete flags only edits stamped after the winning delete (5.6, M4).
func TestEditBeforeDeleteNotFlagged(t *testing.T) {
	r := newReplica(t, "A")
	r.clock.Set(at("09:00"))
	id := NewID(TypeTask)
	r.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	r.clock.Set(at("10:00"))
	r.write(t, nil, set(id, "status", "doing"), note(id, "n"))
	r.clock.Set(at("10:05"))
	r.write(t, nil, set(id, "deleted", true))
	if rec := r.get(t, id); len(flagsOf(rec, FlagEditedAfterDelete)) != 0 {
		t.Fatalf("flags %+v: edits before the delete were flagged", rec.Flags)
	}
}

// A peer set of type outside task and issue is kept, not folded (2.2, M6).
func TestPeerRetypeOutsideTaskIssueNotFolded(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	id := NewID(TypeTask)
	made := r.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	op := peerOp(t, "SHA256:p1", 1, at("10:00").UnixMilli(), 0, map[string]any{"k": "set", "r": id,
		"root": made.Ops[0].String(), "f": "type", "val": TypeFinding, "prev": []string{made.Ops[0].String()}})
	if _, err := r.Apply(ctx, [][]byte{op}); err != nil {
		t.Fatal(err)
	}
	if rec := r.get(t, id); rec.Type != TypeTask {
		t.Fatalf("type %q, want task", rec.Type)
	}
}

// Equal wall and counter: the higher origin, as bytes, wins (5.1, M14).
func TestOriginTieBreak(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "obs")
	w := at("09:00").UnixMilli()
	root := "1@SHA256:a"
	page := [][]byte{
		peerCreate(t, "SHA256:a", 1, w, "task-tiebreak", "T"),
		peerOp(t, "SHA256:a", 2, w+10, 0, map[string]any{"k": "set", "r": "task-tiebreak", "root": root, "f": "title", "val": "from a", "prev": []string{root}}),
		peerOp(t, "SHA256:b", 1, w+10, 0, map[string]any{"k": "set", "r": "task-tiebreak", "root": root, "f": "title", "val": "from b", "prev": []string{root}}),
	}
	if _, err := r.Apply(ctx, page); err != nil {
		t.Fatal(err)
	}
	if rec := r.get(t, "task-tiebreak"); rec.Title != "from b" {
		t.Fatalf("title %q, want the higher origin's", rec.Title)
	}
	if (HLC{Wall: 1, Origin: "b"}).Compare(HLC{Wall: 1, Origin: "a"}) <= 0 {
		t.Fatal("origin b must order after a")
	}
}
