package logbook

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A redacted op can still travel alone (logbook.md 7.6, Redacted ops): its forget lies more
// than a page ahead on the op's own origin, or two origins' forgets need each other across
// more than a page. These tests take one such page, lose the writer of the forget, and check
// that the replica that kept the text and the one that took the redacted op converge on the
// redacted form.

// syncQuiet syncs a and b both ways until neither moves, lets OrphanGrace pass with a repair
// round on each (as gff-hab's sync round), and syncs again.
func syncQuiet(t *testing.T, a, b *replica) {
	t.Helper()
	ctx := context.Background()
	a.maxPages, b.maxPages = 0, 0
	sync := func() {
		for range 4 {
			if _, err := a.Sync(ctx, &direct{to: b}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Sync(ctx, &direct{to: a}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync()
	for _, r := range []*replica{a, b} {
		r.clock.Advance(OrphanGrace + time.Second)
		if err := r.RepairOrphans(ctx, OrphanGrace); err != nil {
			t.Fatal(err)
		}
	}
	sync()
}

// heldRedacted reports whether r holds op id with redacted bytes.
func heldRedacted(t *testing.T, r *replica, id OpID) bool {
	t.Helper()
	var raw []byte
	_ = r.db.QueryRow(`SELECT raw FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq).Scan(&raw)
	return bytes.Contains(raw, []byte(forgottenPrefix))
}

func forgottenTags(t *testing.T, r *replica, id string) bool {
	t.Helper()
	tags := r.get(t, id).Tags
	return len(tags) == 1 && strings.HasPrefix(tags[0], forgottenPrefix)
}

// A forgets its own tag more than a page later. C, behind across both, takes one page from A:
// the tag alone, redacted. A's disk dies. R, which kept the text, and C converge on the
// redacted form.
func TestRedactedAloneSameOrigin(t *testing.T) {
	ctx := context.Background()
	a, r, c := newReplica(t, "A"), newReplica(t, "R"), newReplica(t, "C")
	id := NewID(TypeFinding)
	a.write(t, nil, create(id, TypeFinding, map[string]any{"title": "a finding"}))
	if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
		t.Fatal(err)
	}
	tag := a.write(t, nil, add(id, "tags", "ui")).Ops[0]
	if _, err := r.Sync(ctx, &direct{to: a}); err != nil {
		t.Fatal(err)
	}
	writeBulk(t, a, PageOps+50)
	forget := a.write(t, nil, Step{Kind: KindForget, Target: tag}).Ops[0]

	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	if len(ans.Ops) != 1 || !heldRedacted(t, c, tag) || hasOp(t, c.Store, forget) {
		t.Fatalf("the page was not the redacted tag alone: %d ops", len(ans.Ops))
	}
	_ = a.Close()

	c.clock.Advance(time.Second) // a later exchange
	syncQuiet(t, r, c)
	if dump(t, r.Store) != dump(t, c.Store) || dumpOps(t, r.Store) != dumpOps(t, c.Store) {
		t.Fatalf("R and C differ after A died:\n%s\n---\n%s", dump(t, r.Store), dump(t, c.Store))
	}
	if !forgottenTags(t, r, id) {
		t.Fatalf("R keeps the text: %q", r.get(t, id).Tags)
	}
}

// A forgets B's tag and B forgets A's, each more than a page ahead. C, behind across all of it,
// takes one page from A: A's tag alone, redacted. A and B die. R and C converge on the redacted
// form of A's tag.
func TestRedactedAloneCycle(t *testing.T) {
	ctx := context.Background()
	a, b, r, c := newReplica(t, "A"), newReplica(t, "B"), newReplica(t, "R"), newReplica(t, "C")
	ida, idb := NewID(TypeFinding), NewID(TypeFinding)
	a.write(t, nil, create(ida, TypeFinding, map[string]any{"title": "a's"}))
	b.write(t, nil, create(idb, TypeFinding, map[string]any{"title": "b's"}))
	link(t, a, b)
	if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
		t.Fatal(err)
	}
	tagA := a.write(t, nil, add(ida, "tags", "ui")).Ops[0]
	tagB := b.write(t, nil, add(idb, "tags", "sync")).Ops[0]
	link(t, a, b)
	if _, err := r.Sync(ctx, &direct{to: a}); err != nil {
		t.Fatal(err)
	}
	writeBulk(t, a, PageOps+50)
	a.write(t, nil, Step{Kind: KindForget, Target: tagB})
	writeBulk(t, b, PageOps+50)
	b.write(t, nil, Step{Kind: KindForget, Target: tagA})
	syncQuiet(t, a, b)

	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	if len(ans.Ops) != 1 || !heldRedacted(t, c, tagA) {
		t.Fatalf("the page was not A's redacted tag alone: %d ops", len(ans.Ops))
	}
	_, _ = a.Close(), b.Close()

	c.clock.Advance(time.Second) // a later exchange
	syncQuiet(t, r, c)
	if dump(t, r.Store) != dump(t, c.Store) || dumpOps(t, r.Store) != dumpOps(t, c.Store) {
		t.Fatalf("R and C differ after A and B died:\n%s\n---\n%s", dump(t, r.Store), dump(t, c.Store))
	}
	if !forgottenTags(t, r, ida) {
		t.Fatalf("R keeps A's text: %q", r.get(t, ida).Tags)
	}
}

// sameOriginFarAhead builds A with a tag and its own forget more than a page later, and C that
// holds only the create. It returns the record id, the tag op and A's forget.
func sameOriginFarAhead(t *testing.T, a, c *replica) (string, OpID, OpID) {
	t.Helper()
	ctx := context.Background()
	id := NewID(TypeFinding)
	a.write(t, nil, create(id, TypeFinding, map[string]any{"title": "a finding"}))
	if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
		t.Fatal(err)
	}
	tag := a.write(t, nil, add(id, "tags", "ui")).Ops[0]
	writeBulk(t, a, PageOps+50)
	forget := a.write(t, nil, Step{Kind: KindForget, Target: tag}).Ops[0]
	return id, tag, forget
}

// forgetsBy lists the actors' kinds of the forgets r holds that name id.
func forgetsBy(t *testing.T, r *replica, id OpID) []string {
	t.Helper()
	fs, err := r.forgetsOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range fs {
		var raw []byte
		if err := r.db.QueryRow(`SELECT raw FROM ops WHERE origin = ? AND seq = ?`, f.Origin, f.Seq).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		o, err := decodeOp(raw)
		if err != nil {
			t.Fatal(err)
		}
		k := ""
		if o.Actor != nil {
			k = o.Actor.Kind
		}
		kinds = append(kinds, k)
	}
	return kinds
}

func orphanCount(t *testing.T, r *replica) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM orphans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// While the forget's writer lives, the forget arrives in the same exchange, or the next one
// after an exchange cut short, and the hab writes no forget of its own: Sync repairs nothing.
func TestRepairWaitsForTheForget(t *testing.T) {
	ctx := context.Background()
	for _, cut := range []bool{false, true} {
		a, c := newReplica(t, "A"), newReplica(t, "C")
		_, tag, _ := sameOriginFarAhead(t, a, c)
		if cut {
			c.maxPages = 1
			if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
				t.Fatal(err)
			}
			if !heldRedacted(t, c, tag) || orphanCount(t, c) != 1 {
				t.Fatalf("cut: the first page did not leave the tag an orphan")
			}
			c.clock.Advance(time.Minute)
			c.maxPages = 0
		}
		if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
			t.Fatal(err)
		}
		if k := forgetsBy(t, c, tag); len(k) != 1 || k[0] == ActorHab || orphanCount(t, c) != 0 {
			t.Fatalf("cut %v: forgets by %q, %d orphans", cut, k, orphanCount(t, c))
		}
	}
}

// A replica repairs an orphan only after OrphanGrace (gff-hab's sync round calls
// RepairOrphans); answering pulls and pushes repairs nothing.
func TestRepairAfterGrace(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	_, tag, _ := sameOriginFarAhead(t, a, c)
	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ServePush(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	c.clock.Advance(time.Minute)
	if err := c.RepairOrphans(ctx, OrphanGrace); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 0 {
		t.Fatalf("repaired inside the grace: %q", k)
	}
	c.clock.Advance(OrphanGrace)
	if _, err := c.ServePull(ctx, Vector{}, nil, Limits{}); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 0 {
		t.Fatalf("a pull's answer repaired: %q", k)
	}
	if err := c.RepairOrphans(ctx, OrphanGrace); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 1 || k[0] != ActorHab || orphanCount(t, c) != 0 {
		t.Fatalf("after the grace: forgets by %q, %d orphans", k, orphanCount(t, c))
	}
}

// No repair during the start hold; one after it.
func TestRepairWaitsOutTheHold(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	_, tag, _ := sameOriginFarAhead(t, a, c)
	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	c.clock.Advance(time.Second)
	if err := c.HoldWrites(ctx, c.clock.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.RepairOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 0 {
		t.Fatalf("repaired during the hold: %q", k)
	}
	if err := c.HoldWrites(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := c.RepairOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 1 || k[0] != ActorHab {
		t.Fatalf("after the hold: forgets by %q", k)
	}
}

// A file from before the orphans table: the first open finds the orphan it already held.
func TestRepairScanAtOpen(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	_, tag, _ := sameOriginFarAhead(t, a, c)
	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`DELETE FROM orphans; DELETE FROM meta WHERE key = 'orphans_scanned'`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c2 := openReplica(t, "C", c.path)
	if orphanCount(t, c2) != 1 {
		t.Fatalf("open found %d orphans", orphanCount(t, c2))
	}
	c2.clock.Advance(time.Second)
	if err := c2.RepairOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c2, tag); len(k) != 1 || k[0] != ActorHab {
		t.Fatalf("forgets by %q", k)
	}
}

// A value that only looks like a forgotten one, written here or sent by a peer, is no orphan
// and never gets a forget: a redaction writes its op's keys sorted, which an op as written never
// has, so only redacted bytes are in redacted form; a remove, which a forget leaves alone, is never
// one either. The person reads the same note everywhere.
func TestMarkerLookalike(t *testing.T) {
	ctx := context.Background()
	look := forgottenPrefix + strings.Repeat("ab", 32)
	a, c := newReplica(t, "A"), newReplica(t, "C")
	id := NewID(TypeTask)
	a.write(t, nil, create(id, TypeTask, map[string]any{"title": "a task"}))
	own := a.write(t, nil, note(id, look)).Ops[0]
	before := dumpOps(t, a.Store)
	for range 2 {
		if _, err := c.Sync(ctx, &direct{to: a}); err != nil {
			t.Fatal(err)
		}
		c.clock.Advance(time.Hour)
		a.clock.Advance(time.Hour)
		if _, err := a.ServePull(ctx, Vector{}, nil, Limits{}); err != nil {
			t.Fatal(err)
		}
		for _, r := range []*replica{a, c} {
			if err := r.RepairOrphans(ctx, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	notes := func(r *replica) string {
		var text string
		if err := r.db.QueryRow(`SELECT text FROM notes`).Scan(&text); err != nil {
			t.Fatal(err)
		}
		return text
	}
	forgets := 0
	for _, op := range []OpID{own} {
		forgets += len(forgetsBy(t, a, op)) + len(forgetsBy(t, c, op))
	}
	if orphanCount(t, a)+orphanCount(t, c) != 0 || forgets != 0 {
		t.Fatalf("the lookalike was taken for a forgotten value: %d %d orphans", orphanCount(t, a), orphanCount(t, c))
	}
	if dumpOps(t, a.Store) != before || dumpOps(t, c.Store) != before || notes(a) != look || notes(c) != look {
		t.Fatalf("the note changed: %q %q", notes(a), notes(c))
	}
}

// A remove holds no text a forget redacts, so one whose member looks forgotten, its keys sorted as
// a redaction writes them, is still never in redacted form (logbook.md 7.6, Redacted ops).
func TestMarkerLookalikeRemove(t *testing.T) {
	look := forgottenPrefix + strings.Repeat("ab", 32)
	o := Op{V: OpVersion, Origin: "SHA256:a", Seq: 2, BatchLast: 2, Kind: KindRemove, Record: "r", Field: "tags", Value: jsonString(look)}
	if err := o.encode(); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(o.raw, &m); err != nil {
		t.Fatal(err)
	}
	sorted, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if redactedForm(o.raw) || redactedForm(sorted) {
		t.Fatalf("a remove passed as redacted: %s", sorted)
	}
}

// orphanOnC leaves C holding A's tag redacted, without its forget, seen at C's clock.
func orphanOnC(t *testing.T, a, c *replica) OpID {
	t.Helper()
	ctx := context.Background()
	_, tag, _ := sameOriginFarAhead(t, a, c)
	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, ans.Ops); err != nil {
		t.Fatal(err)
	}
	if orphanCount(t, c) != 1 {
		t.Fatalf("%d orphans", orphanCount(t, c))
	}
	c.clock.Advance(time.Second)
	return tag
}

// A repair that fails stays an orphan, logs once however often it is tried, and goes through
// once writes do.
func TestRepairRetry(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	tag := orphanOnC(t, a, c)
	if _, err := c.db.Exec(`INSERT INTO origins(origin, shared) VALUES(?, 1) ON CONFLICT(origin) DO UPDATE SET shared = 1`, "SHA256:C"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.RepairOrphans(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, l := range c.logged() {
		if strings.Contains(l, "repairing the missing forget") {
			n++
		}
	}
	if n != 1 || orphanCount(t, c) != 1 || len(forgetsBy(t, c, tag)) != 0 {
		t.Fatalf("failed repairs: %d logs, %d orphans", n, orphanCount(t, c))
	}
	if _, err := c.db.Exec(`UPDATE origins SET shared = 0 WHERE origin = ?`, "SHA256:C"); err != nil {
		t.Fatal(err)
	}
	if err := c.RepairOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 1 || k[0] != ActorHab || orphanCount(t, c) != 0 {
		t.Fatalf("after the retry: forgets by %q, %d orphans", k, orphanCount(t, c))
	}
}

// Two repairs at once write one forget.
func TestRepairConcurrent(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	tag := orphanOnC(t, a, c)
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- c.RepairOrphans(ctx, 0) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if k := forgetsBy(t, c, tag); len(k) != 1 {
		t.Fatalf("forgets by %q", k)
	}
}

// An orphan of a kind this build does not know gets no forget and stays, so a later build that
// knows the kind repairs it.
func TestRepairKeepsUnknownKind(t *testing.T) {
	ctx := context.Background()
	a, c := newReplica(t, "A"), newReplica(t, "C")
	_, tag, _ := sameOriginFarAhead(t, a, c)
	ans, err := a.ServePull(ctx, c.vec(t), nil, Limits{})
	if err != nil || len(ans.Ops) != 1 {
		t.Fatalf("pull: %d ops %v", len(ans.Ops), err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ans.Ops[0], &m); err != nil {
		t.Fatal(err)
	}
	m["k"] = json.RawMessage(`"tagify"`)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, [][]byte{raw}); err != nil {
		t.Fatal(err)
	}
	if orphanCount(t, c) != 1 {
		t.Fatalf("%d orphans", orphanCount(t, c))
	}
	c.clock.Advance(time.Second)
	if err := c.RepairOrphans(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if k := forgetsBy(t, c, tag); len(k) != 0 || orphanCount(t, c) != 1 {
		t.Fatalf("forgets by %q, %d orphans", k, orphanCount(t, c))
	}
}
