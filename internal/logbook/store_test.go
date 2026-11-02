package logbook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A set of a finding's title or body is refused locally with a hint to supersede; from a peer it
// is kept in the log, not folded, and logged once (5.6, 14).
func TestFindingBodySetRefused(t *testing.T) {
	ctx := context.Background()
	a, b, f, _ := twoHosts(t, TypeFinding, map[string]any{"title": "F", "body": "learned"})
	for _, field := range []string{"body", "title"} {
		_, err := a.Write(ctx, Batch{Steps: []Step{set(f, field, "changed")}})
		var ie *InvalidError
		if !errors.As(err, &ie) || !errors.Is(err, ErrInvalid) || !strings.Contains(ie.Hint, "supersede") {
			t.Fatalf("set %s of a finding: %v, want invalid with a supersede hint", field, err)
		}
	}
	if _, err := a.Write(ctx, Batch{Steps: []Step{set(f, "type", TypeTask)}}); !isInvalid(err) {
		t.Fatalf("retype a finding: %v, want invalid", err)
	}
	// Tags, links, notes, project and deleted may change.
	a.write(t, nil, add(f, "tags", "go"), note(f, "seen again"), set(f, "project", "proj-abcdefgh"))

	// A faulty peer sends a body set.
	rec := b.get(t, f)
	vb, _ := b.Vector(ctx)
	seq := vb[b.Origin()] + 1
	bad := rawOp(t, map[string]any{"v": 1, "o": b.Origin(), "s": seq, "w": at("10:00").UnixMilli(), "c": 0,
		"bl": seq, "k": "set", "r": f, "root": rec.Root.String(), "f": "body", "val": "rewritten",
		"prev": []string{rec.Root.String()}})
	if _, err := a.Apply(ctx, [][]byte{bad}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, [][]byte{bad}); err != nil { // a repeat logs nothing more
		t.Fatal(err)
	}
	got := a.get(t, f)
	if got.Body != "learned" {
		t.Fatalf("the faulty set was folded: body %q", got.Body)
	}
	ops, _ := a.History(ctx, f)
	if !slices.ContainsFunc(ops, func(o *Op) bool { return o.Seq == seq && o.Origin == b.Origin() }) {
		t.Fatal("the faulty set is not kept in the log")
	}
	n := 0
	for _, l := range a.logged() {
		if strings.Contains(l, "not folded") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %d times, want once: %v", n, a.logged())
	}
	// A rebuild keeps it out too.
	before := dump(t, a.Store)
	if err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if dump(t, a.Store) != before {
		t.Fatal("rebuild folded differently")
	}
}

// An op of a later version or an unknown kind is kept and forwarded, as its bytes came, but not
// folded (4.4, 14).
func TestNewerOpKeptAndForwarded(t *testing.T) {
	ctx := context.Background()
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	rec := b.get(t, id)
	va, _ := a.Vector(ctx)
	s1, s2, s3 := va[a.Origin()]+1, va[a.Origin()]+2, va[a.Origin()]+3
	ops := [][]byte{
		// A version this build does not know, with a body shape it could not read.
		rawOp(t, map[string]any{"v": 2, "o": a.Origin(), "s": s1, "w": at("10:00").UnixMilli(), "c": 0, "bl": s1,
			"k": "set", "r": id, "root": rec.Root.String(), "f": "status", "val": "doing",
			"prev": map[string]any{"vector": 7}, "extra": []int{1, 2}}),
		// A kind this build does not know.
		rawOp(t, map[string]any{"v": 1, "o": a.Origin(), "s": s2, "w": at("10:01").UnixMilli(), "c": 0, "bl": s2,
			"k": "move", "r": id, "root": rec.Root.String(), "val": "somewhere"}),
		// A record type this build does not know.
		rawOp(t, map[string]any{"v": 1, "o": a.Origin(), "s": s3, "w": at("10:02").UnixMilli(), "c": 0, "bl": s3,
			"k": "create", "r": "habit-abcdefgh", "root": OpID{Origin: a.Origin(), Seq: s3}.String(),
			"val": map[string]any{"type": "habit", "fields": map[string]any{"title": "H"}}}),
	}
	res, err := b.Apply(ctx, ops)
	if err != nil || res.Stored != 3 || res.Refused != 0 {
		t.Fatalf("apply: %+v %v, want three stored", res, err)
	}
	st, _ := b.Status(ctx)
	if st.Unfolded != 3 || st.Vector[a.Origin()] != s3 || st.Waiting != 0 {
		t.Fatalf("status %+v: want three unfolded ops counted in the vector", st)
	}
	if got := b.get(t, id); got.Status != "open" || got.Revision != rec.Revision {
		t.Fatalf("an unknown op was folded: %+v", got)
	}
	if _, err := b.Get(ctx, "habit-abcdefgh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a record of an unknown type was folded: %v", err)
	}
	// A third replica that pulls from B gets the same bytes, and B's next write orders after them.
	c := newReplica(t, "C")
	push(t, b, c)
	got, _, err := c.OpsAfter(ctx, Vector{a.Origin(): s1 - 1}, 0, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("C holds %d ops: %v", len(got), err)
	}
	for i := range ops {
		if string(got[i]) != string(ops[i]) {
			t.Fatalf("op %d forwarded as %s, want %s", i, got[i], ops[i])
		}
	}
	// Reopening the file keeps them.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b2 := openReplica(t, "B", b.path)
	if st, _ := b2.Status(ctx); st.Unfolded != 3 {
		t.Fatalf("after reopen: %+v", st)
	}
	// An op whose envelope is unusable is refused.
	if res, _ := b2.Apply(ctx, [][]byte{[]byte(`{"v":1,"o":"x"}`), []byte(`not json`)}); res.Refused != 2 {
		t.Fatalf("bad envelopes: %+v, want two refused", res)
	}
}

// FTS5 works with the pure-Go driver: title, body, notes and tags are searched; deleted records
// and FTS syntax in the words do no harm.
func TestFindFTS5(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	g, k, d := NewID(TypeGuide), NewID(TypeTask), NewID(TypeTask)
	r.write(t, nil, create(g, TypeGuide, map[string]any{"title": "Restart the holder", "body": "Use the drain path"}),
		create(k, TypeTask, map[string]any{"title": "Fix the flaky test"}), note(k, "the watchdog fires early"),
		add(k, "tags", "holder"), create(d, TypeTask, map[string]any{"title": "drain twice"}))
	r.write(t, nil, set(d, "deleted", true))
	cases := map[string][]string{
		"holder":         {g, k}, // a title, a tag
		"drain":          {g},    // a body; the deleted record left out
		"watchdog early": {k},    // a note, both words
		"flaky watchdog": {k},
		"nothing":        nil,
		`AND( "*`:        nil, // FTS5 syntax is matched as plain words
	}
	for q, want := range cases {
		got, err := r.Find(ctx, q, 10)
		if err != nil {
			t.Fatalf("find %q: %v", q, err)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("find %q: %v, want %v", q, got, want)
		}
	}
}

// A forgotten value is redacted in the log and the index on every replica, in either arrival
// order (5.7).
func TestForget(t *testing.T) {
	ctx := context.Background()
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	secret := "token-hunter2-secret"
	n := a.write(t, nil, note(id, "the key is "+secret)).Ops[0]
	c := newReplica(t, "C")
	push(t, a, b) // B holds the note before the forget
	a.write(t, nil, Step{Kind: KindForget, Target: n})
	link(t, a, b)
	// C gets the forget before the note: the ops of A's two batches arrive in reverse order.
	all, _, _ := a.OpsAfter(ctx, Vector{}, 0, 0)
	slices.Reverse(all)
	for _, raw := range all {
		if _, err := c.Apply(ctx, [][]byte{raw}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*replica{a, b, c} {
		rec := r.get(t, id)
		if len(rec.Notes) != 1 || !strings.HasPrefix(rec.Notes[0].Text, forgottenPrefix) {
			t.Fatalf("%s: notes %+v, want one forgotten note", r.name, rec.Notes)
		}
		if hits, _ := r.Find(ctx, "hunter2", 10); len(hits) != 0 {
			t.Fatalf("%s: the index still finds the forgotten text", r.name)
		}
		raw, _, _ := r.OpsAfter(ctx, Vector{}, 0, 0)
		for _, o := range raw {
			if strings.Contains(string(o), secret) {
				t.Fatalf("%s: an op still holds the secret: %s", r.name, o)
			}
		}
		var leaked int
		if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM ops WHERE instr(raw, ?) > 0`, "hunter2").Scan(&leaked); err != nil || leaked != 0 {
			t.Fatalf("%s: %d stored ops hold the secret (%v)", r.name, leaked, err)
		}
	}
	if dump(t, a.Store) != dump(t, b.Store) || dump(t, a.Store) != dump(t, c.Store) {
		t.Fatal("replicas fold the forget differently")
	}
	// The same op redacted again is a repeat, not a second op with one id.
	push(t, b, c)
	if st, _ := c.Status(ctx); len(st.Shared) != 0 {
		t.Fatalf("a redacted repeat looked like a shared origin: %v", st.Shared)
	}
	// A forget of a value with no text is refused.
	st := a.write(t, nil, set(id, "priority", 2)).Ops[0]
	if _, err := a.Write(ctx, Batch{Steps: []Step{{Kind: KindForget, Target: st}}}); !isInvalid(err) {
		t.Fatalf("forget a priority: %v, want invalid", err)
	}
}

// Two creates of one id made apart: the lower stamp keeps the id, the other becomes id-2 with a
// flag, and later ops reach the right record (2.3).
func TestIDCollision(t *testing.T) {
	a, b := newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:00"))
	b.clock.Set(at("09:01"))
	const id = "task-k3m9q2xz"
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "on A"}))
	b.write(t, nil, create(id, TypeTask, map[string]any{"title": "on B"}))
	b.write(t, nil, note(id, "B's note"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		first, second := r.get(t, id), r.get(t, id+"-2")
		if first.Title != "on A" || len(first.Flags) != 0 {
			t.Fatalf("%s: %s is %q flags %+v", r.name, id, first.Title, first.Flags)
		}
		if second.Title != "on B" || len(flagsOf(second, FlagIDCollision)) != 1 || len(second.Notes) != 1 {
			t.Fatalf("%s: %s-2 is %+v", r.name, id, second)
		}
	}
	if _, err := a.Write(context.Background(), Batch{Steps: []Step{create(id, TypeTask, map[string]any{"title": "x"})}}); !isInvalid(err) {
		t.Fatalf("create of a held id: %v, want invalid", err)
	}
}

// A batch folds all at once: a replica holding part of it, or ops after a gap, folds none of
// them until the rest arrive (4.2, 7.1).
func TestBatchAllOrNothing(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}), add(id, "tags", "x"), note(id, "n"))
	a.write(t, nil, set(id, "status", "doing"))
	ops, _, _ := a.OpsAfter(ctx, Vector{}, 0, 0)
	if len(ops) != 4 {
		t.Fatalf("%d ops", len(ops))
	}
	// The second batch alone, then the first batch without its last op.
	if _, err := b.Apply(ctx, [][]byte{ops[3], ops[0], ops[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("part of a batch was folded: %v", err)
	}
	st, _ := b.Status(ctx)
	if st.Vector[a.Origin()] != 2 || st.Waiting != 3 {
		t.Fatalf("status %+v: want 2 held and 3 waiting", st)
	}
	if _, err := b.Apply(ctx, [][]byte{ops[2]}); err != nil {
		t.Fatal(err)
	}
	if rec := b.get(t, id); rec.Status != "doing" || len(rec.Tags) != 1 || len(rec.Notes) != 1 {
		t.Fatalf("after the gap filled: %+v", rec)
	}
	if dump(t, a.Store) != dump(t, b.Store) {
		t.Fatal("A and B fold differently")
	}
}

// The writing rules of 2.2 and 14.
func TestWriteRefusals(t *testing.T) {
	ctx := context.Background()
	r := newReplica(t, "A")
	k, f, p := NewID(TypeTask), NewID(TypeFinding), NewID(TypePlan)
	r.write(t, nil, create(k, TypeTask, map[string]any{"title": "T"}), create(f, TypeFinding, map[string]any{"title": "F"}),
		create(p, TypePlan, map[string]any{"title": "P"}))
	bad := map[string][]Step{
		"body over 256 KiB":       {set(k, "body", strings.Repeat("x", MaxBody+1))},
		"two-line title":          {set(k, "title", "a\nb")},
		"title over 200":          {set(k, "title", strings.Repeat("x", 201))},
		"status of another type":  {set(k, "status", "current")},
		"priority 4":              {set(k, "priority", 4)},
		"field of another type":   {set(k, "pinned", true)},
		"set of a set field":      {set(k, "tags", "x")},
		"set twice in one batch":  {set(k, "status", "doing"), set(k, "status", "done")},
		"supersedes from a task":  {add(k, "links", Link{Kind: LinkSupersedes, To: f})},
		"stage to a finding":      {add(p, "links", Link{Kind: LinkStage, To: f, Pos: 1})},
		"unknown link kind":       {add(k, "links", Link{Kind: "likes", To: f})},
		"tag with a space":        {add(k, "tags", "two words")},
		"remove a missing tag":    {remove(k, "tags", "nope")},
		"create with no title":    {create("", TypeTask, nil)},
		"create of unknown type":  {create("", "habit", map[string]any{"title": "x"})},
		"create with a tag field": {create("", TypeTask, map[string]any{"title": "x", "tags": "a"})},
		"proposal not pending":    {create("", TypeProposal, map[string]any{"title": "x", "status": "approved"})},
		"empty note":              {note(k, "  ")},
	}
	for name, steps := range bad {
		if _, err := r.Write(ctx, Batch{Steps: steps}); !isInvalid(err) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	if _, err := r.Write(ctx, Batch{Steps: []Step{set("task-zzzzzzzz", "status", "done")}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown record: %v, want not found", err)
	}
	// A refused batch leaves nothing: no op, no seq used.
	if v, _ := r.Vector(ctx); v[r.Origin()] != 3 {
		t.Fatalf("vector %v after refusals, want 3", v)
	}
	// A task and an issue retype into each other and keep their id.
	r.write(t, nil, set(k, "type", TypeIssue), set(k, "component", "holder"))
	if rec := r.get(t, k); rec.Type != TypeIssue || rec.ID != k || str(rec.Fields["component"].Value) != "holder" {
		t.Fatalf("retype: %+v", rec)
	}
	// A link to a record not here yet is kept and shown so.
	r.write(t, nil, add(k, "links", Link{Kind: LinkRelates, To: "task-notheree"}))
	if rec := r.get(t, k); len(rec.Links) != 1 || rec.Links[0].Here {
		t.Fatalf("links %+v, want one not here yet", rec.Links)
	}
}

// The file: owner-only, its origin fixed at first open, the CLI opening it with no origin.
func TestOpenFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "logbook.db")
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("new file with no origin: %v", err)
	}
	r := openReplica(t, "A", path)
	id := NewID(TypeTask)
	r.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v, want 0600", fi.Mode(), err)
	}
	if _, err := Open(ctx, path, Options{Origin: "SHA256:other"}); err == nil {
		t.Fatal("another origin opened the file")
	}
	cli, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if cli.Origin() != r.Origin() {
		t.Fatalf("CLI origin %q, want %q", cli.Origin(), r.Origin())
	}
	// The hab sees the CLI's write through Changes.
	_, cursor, err := r.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cli.Write(ctx, Batch{Steps: []Step{note(id, "from the CLI")}})
	if err != nil || res.Ops[0].Seq != 2 {
		t.Fatalf("CLI write: %+v %v, want seq 2", res, err)
	}
	ids, next, err := r.Changes(ctx, cursor)
	if err != nil || !slices.Equal(ids, []string{id}) || next <= cursor {
		t.Fatalf("changes: %v %d %v", ids, next, err)
	}
	if rec := r.get(t, id); len(rec.Notes) != 1 {
		t.Fatalf("the hab does not see the CLI's note: %+v", rec.Notes)
	}
}

// Two different ops with one op id: the first stays and the origin's further ops are refused
// (7.4).
func TestSharedOrigin(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "T"}))
	push(t, a, b)
	clone := rawOp(t, map[string]any{"v": 1, "o": a.Origin(), "s": 1, "w": 1, "c": 0, "bl": 1, "k": "create",
		"r": "task-clonedvm", "root": "1@" + a.Origin(), "val": map[string]any{"type": "task", "fields": map[string]any{"title": "C"}}})
	res, _ := b.Apply(ctx, [][]byte{clone})
	if res.Refused != 1 {
		t.Fatalf("%+v, want the second op refused", res)
	}
	a.write(t, nil, note(id, "later"))
	push(t, a, b)
	st, _ := b.Status(ctx)
	if !slices.Equal(st.Shared, []string{a.Origin()}) || st.Vector[a.Origin()] != 1 {
		t.Fatalf("status %+v: want the origin marked shared and its later ops refused", st)
	}
	if rec := b.get(t, id); rec.Title != "T" {
		t.Fatalf("the first op did not stay: %+v", rec)
	}
}
