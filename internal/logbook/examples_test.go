package logbook

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// The worked conflict examples of logbook.md 6, one test each. Hosts A and B are two replicas;
// "apart" means no exchange while the writes happen; link exchanges ops both ways.

var (
	agentS1 = &Actor{Kind: "agent", Harness: "claude", Session: "s1"}
	phone   = &Actor{Kind: "person", Device: "SHA256:phone"}
)

// twoHosts makes A and B with one task T, created on A and known to both.
func twoHosts(t *testing.T, typ string, fields map[string]any) (a, b *replica, id string, res WriteResult) {
	t.Helper()
	a, b = newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:00"))
	b.clock.Set(at("09:00"))
	id = NewID(typ)
	res = a.write(t, nil, create(id, typ, fields))
	link(t, a, b)
	return a, b, id, res
}

// E1. Status on both hosts.
func TestE1StatusOnBothHosts(t *testing.T) {
	a, b, id, made := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	o1 := made.Ops[0]
	a.clock.Set(at("10:00"))
	doing := a.write(t, agentS1, set(id, "status", "doing")).Ops[0]
	b.clock.Set(at("10:05"))
	done := b.write(t, phone, set(id, "status", "done")).Ops[0]
	// Neither saw the other: each set's prev is the create, o1.
	for _, r := range []*replica{a, b} {
		ops, err := r.History(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range ops {
			if o.Kind == KindSet && !slices.Equal(o.Prev, []OpID{o1}) {
				t.Fatalf("%s: set prev %v, want [%v]", r.name, o.Prev, o1)
			}
		}
	}
	a.clock.Set(at("11:00"))
	b.clock.Set(at("11:00"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if rec.Status != "done" {
			t.Fatalf("%s: status %q, want done", r.name, rec.Status)
		}
		fl := flagsOf(rec, FlagConflict)
		if len(fl) != 1 || fl[0].Field != "status" || len(fl[0].Values) != 2 {
			t.Fatalf("%s: flags %+v, want one status conflict with two values", r.name, rec.Flags)
		}
		v0, v1 := fl[0].Values[0], fl[0].Values[1]
		if v0.Op != doing || str(v0.Value) != "doing" || v0.Origin != a.Origin() || v0.Actor.Harness != "claude" || v0.Actor.Session != "s1" {
			t.Fatalf("%s: first side %+v, want doing on A by claude in s1", r.name, v0)
		}
		if v1.Op != done || str(v1.Value) != "done" || v1.Origin != b.Origin() || v1.Actor.Device != phone.Device {
			t.Fatalf("%s: second side %+v, want done on B by phone", r.name, v1)
		}
	}
	// The user resolves on A: a set that saw both values clears the flag everywhere (5.2).
	res := a.write(t, phone, set(id, "status", "done"))
	ops, _ := a.History(context.Background(), id)
	last := ops[len(ops)-1]
	if last.ID() != res.Ops[0] || !slices.Equal(last.Prev, []OpID{doing, done}) {
		t.Fatalf("resolve prev %v, want both sides", last.Prev)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if rec := r.get(t, id); len(rec.Flags) != 0 || rec.Status != "done" {
			t.Fatalf("%s after resolve: status %q flags %+v", r.name, rec.Status, rec.Flags)
		}
	}
	if dump(t, a.Store) != dump(t, b.Store) {
		t.Fatal("A and B fold differently")
	}
}

// E2. Different fields.
func TestE2DifferentFields(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	a.clock.Set(at("10:00"))
	a.write(t, nil, set(id, "title", "T renamed"))
	b.clock.Set(at("10:01"))
	b.write(t, nil, set(id, "status", "doing"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if rec.Title != "T renamed" || rec.Status != "doing" || len(rec.Flags) != 0 {
			t.Fatalf("%s: title %q status %q flags %+v", r.name, rec.Title, rec.Status, rec.Flags)
		}
	}
}

// E3. Two body edits.
func TestE3TwoBodyEdits(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeGuide, map[string]any{"title": "G", "body": "first"})
	a.clock.Set(at("09:00"))
	a.clock.Advance(time.Second)
	fromA := a.write(t, nil, set(id, "body", "A's body"), note(id, "note on A")).Ops[0]
	b.clock.Set(at("09:30"))
	b.write(t, nil, set(id, "body", "B's body"), note(id, "note on B"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if rec.Body != "B's body" {
			t.Fatalf("%s: body %q, want B's", r.name, rec.Body)
		}
		if fl := flagsOf(rec, FlagConflict); len(fl) != 1 || fl[0].Field != "body" {
			t.Fatalf("%s: flags %+v, want a body conflict", r.name, rec.Flags)
		}
		if len(rec.Notes) != 2 || rec.Notes[0].Text != "note on A" || rec.Notes[1].Text != "note on B" {
			t.Fatalf("%s: notes %+v, want both in stamp order", r.name, rec.Notes)
		}
		ops, err := r.History(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(ops, func(o *Op) bool { return o.ID() == fromA && str(o.Value) == "A's body" }) {
			t.Fatalf("%s: A's body is not kept in the history", r.name)
		}
	}
}

// E4. Tag add and remove.
func TestE4TagAddAndRemove(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	a1 := a.write(t, nil, add(id, "tags", "ui")).Ops[0]
	link(t, a, b)
	rm := a.write(t, nil, remove(id, "tags", "ui")).Ops[0]
	b.write(t, nil, add(id, "tags", "ui"))
	ops, _ := a.History(context.Background(), id)
	for _, o := range ops {
		if o.ID() == rm && !slices.Equal(o.Prev, []OpID{a1}) {
			t.Fatalf("remove names %v, want [%v]", o.Prev, a1)
		}
	}
	if rec := a.get(t, id); len(rec.Tags) != 0 {
		t.Fatalf("A before link: tags %v, want none", rec.Tags)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if !slices.Equal(rec.Tags, []string{"ui"}) || len(rec.Flags) != 0 {
			t.Fatalf("%s: tags %v flags %+v, want [ui] and no flag", r.name, rec.Tags, rec.Flags)
		}
	}
}

// E5. Supersede twice.
func TestE5SupersedeTwice(t *testing.T) {
	a, b, f, _ := twoHosts(t, TypeFinding, map[string]any{"title": "F", "body": "what we learned"})
	f2, f3 := NewID(TypeFinding), NewID(TypeFinding)
	a.write(t, nil, create(f2, TypeFinding, map[string]any{"title": "F2"}), add(f2, "links", Link{Kind: LinkSupersedes, To: f}))
	b.write(t, nil, create(f3, TypeFinding, map[string]any{"title": "F3"}), add(f3, "links", Link{Kind: LinkSupersedes, To: f}))
	if rec := a.get(t, f); !rec.Superseded() || len(flagsOf(rec, FlagSupersededTwice)) != 0 {
		t.Fatalf("A before link: successors %v flags %+v", rec.Successors, rec.Flags)
	}
	link(t, a, b)
	want := []string{f2, f3}
	slices.Sort(want)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, f)
		if !slices.Equal(rec.Successors, want) || len(flagsOf(rec, FlagSupersededTwice)) != 1 {
			t.Fatalf("%s: successors %v flags %+v, want %v and superseded twice", r.name, rec.Successors, rec.Flags, want)
		}
		for _, id := range want {
			if s := r.get(t, id); s.Deleted || len(s.Flags) != 0 || s.Superseded() {
				t.Fatalf("%s: %s should stand unflagged: %+v", r.name, id, s)
			}
		}
	}
	// The user drops one: F shows one successor and the flag goes.
	a.write(t, nil, set(f3, "deleted", true))
	if rec := a.get(t, f); !slices.Equal(rec.Successors, []string{f2}) || len(rec.Flags) != 0 {
		t.Fatalf("after dropping F3: successors %v flags %+v", rec.Successors, rec.Flags)
	}
}

// E6. Delete and edit.
func TestE6DeleteAndEdit(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	a.clock.Set(at("10:00"))
	a.write(t, nil, set(id, "deleted", true))
	b.clock.Set(at("10:10"))
	b.write(t, nil, note(id, "progress on B"), set(id, "status", "doing"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if !rec.Deleted || rec.Status != "doing" || len(rec.Notes) != 1 {
			t.Fatalf("%s: deleted %v status %q notes %d, want deleted with B's edits folded", r.name, rec.Deleted, rec.Status, len(rec.Notes))
		}
		if fl := flagsOf(rec, FlagEditedAfterDelete); len(fl) != 1 || len(fl[0].Values) != 2 {
			t.Fatalf("%s: flags %+v, want edited after delete with B's two ops", r.name, rec.Flags)
		}
		list, err := r.List(context.Background(), Filter{})
		if err != nil || len(list) != 0 {
			t.Fatalf("%s: list %v %v, want the deleted record hidden", r.name, list, err)
		}
	}
	// A deleted record takes no edit but a restore.
	if _, err := a.Write(context.Background(), Batch{Steps: []Step{note(id, "x")}}); !isInvalid(err) {
		t.Fatalf("note on a deleted record: %v, want invalid", err)
	}
	a.clock.Set(at("10:20"))
	a.write(t, phone, set(id, "deleted", false))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if rec.Deleted || rec.Status != "doing" || len(rec.Notes) != 1 || rec.Notes[0].Text != "progress on B" || len(rec.Flags) != 0 {
			t.Fatalf("%s after restore: %+v", r.name, rec)
		}
	}
}

// E7. A clock ahead.
func TestE7ClockAhead(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	b.clock.offset = 10 * time.Minute
	b.clock.Set(at("10:00")) // B thinks it is 10:10
	fromB := b.write(t, nil, set(id, "title", "from B")).Ops[0]
	a.clock.Set(at("10:01"))
	push(t, b, a)
	a.clock.Set(at("10:05"))
	fromA := a.write(t, nil, set(id, "title", "from A")).Ops[0]
	ops, _ := a.History(context.Background(), id)
	var opA, opB *Op
	for _, o := range ops {
		switch o.ID() {
		case fromA:
			opA = o
		case fromB:
			opB = o
		}
	}
	if opB.Wall != at("10:10").UnixMilli() || !opB.before(opA) {
		t.Fatalf("A's op %v does not order after B's %v", opA.HLC(), opB.HLC())
	}
	if !slices.Equal(opA.Prev, []OpID{fromB}) {
		t.Fatalf("A's prev %v, want B's op", opA.Prev)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if rec := r.get(t, id); rec.Title != "from A" || len(rec.Flags) != 0 {
			t.Fatalf("%s: title %q flags %+v, want A's title and no conflict", r.name, rec.Title, rec.Flags)
		}
	}
	if st, _ := a.Status(context.Background()); len(st.Ahead) != 0 {
		t.Fatalf("10 minutes ahead flagged as far ahead: %v", st.Ahead)
	}
}

// E8. A clock far ahead.
func TestE8ClockFarAhead(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	b.clock.offset = 24 * time.Hour
	b.clock.Set(at("10:00"))
	b.write(t, nil, set(id, "status", "doing"))
	b.write(t, nil, note(id, "second op from the fast clock"))
	push(t, b, a)
	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Ahead, []string{b.Origin()}) {
		t.Fatalf("ahead %v, want B", st.Ahead)
	}
	n := 0
	for _, l := range a.logged() {
		if strings.Contains(l, "clock ahead on "+b.Origin()) {
			n++
		}
	}
	b.write(t, nil, note(id, "third"))
	push(t, b, a)
	m := 0
	for _, l := range a.logged() {
		if strings.Contains(l, "clock ahead on "+b.Origin()) {
			m++
		}
	}
	if n != 1 || m != 1 {
		t.Fatalf("clock ahead logged %d then %d times, want once", n, m)
	}
	a.clock.Set(at("10:01"))
	res := a.write(t, nil, set(id, "status", "done"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, id)
		if rec.Status != "done" || rec.Fields["status"].Op != res.Ops[0] || len(rec.Notes) != 2 {
			t.Fatalf("%s: status %q by %v, notes %d: A's later write must order after B's", r.name, rec.Status, rec.Fields["status"].Op, len(rec.Notes))
		}
	}
}

// E9. Same identity, two projects.
func TestE9SameIdentityTwoProjects(t *testing.T) {
	a, b := newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:00"))
	b.clock.Set(at("09:05"))
	const identity = "forge.example/owner/repo"
	p1, p2 := NewID(TypeProject), NewID(TypeProject)
	if !strings.HasPrefix(p1, "proj-") {
		t.Fatalf("project id %q, want proj-", p1)
	}
	t1, t2 := NewID(TypeTask), NewID(TypeTask)
	a.write(t, nil, create(p1, TypeProject, map[string]any{"title": "repo"}), add(p1, "identities", identity),
		create(t1, TypeTask, map[string]any{"title": "on A", "project": p1}))
	b.write(t, nil, create(p2, TypeProject, map[string]any{"title": "repo"}), add(p2, "identities", identity),
		create(t2, TypeTask, map[string]any{"title": "on B", "project": p2}))
	if rec := b.get(t, t2); rec.Project != p2 {
		t.Fatalf("B before link: project %q, want %q", rec.Project, p2)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		for _, id := range []string{t1, t2} {
			if rec := r.get(t, id); rec.Project != p1 {
				t.Fatalf("%s: %s shows under %q, want %q", r.name, id, rec.Project, p1)
			}
		}
		for _, p := range []string{p1, p2} {
			list, err := r.List(context.Background(), Filter{Type: TypeTask, Project: p})
			if err != nil || len(list) != 2 {
				t.Fatalf("%s: tasks under %s: %v %v, want both", r.name, p, list, err)
			}
		}
		if rec := r.get(t, p2); rec.ID != p2 {
			t.Fatalf("%s: the joined project lost its id", r.name)
		}
	}
}

// E10. Proposal decided twice.
func TestE10ProposalDecidedTwice(t *testing.T) {
	a, b, x, _ := twoHosts(t, TypeProposal, map[string]any{"title": "remember X", "target": "record",
		"draft": map[string]any{"type": "task", "title": "X"}})
	made := NewID(TypeTask)
	a.clock.Set(at("10:00"))
	a.write(t, phone, set(x, "status", "approved"), set(x, "decided_by", "SHA256:phone"),
		create(made, TypeTask, map[string]any{"title": "X"}), add(made, "links", Link{Kind: LinkFrom, To: x}))
	b.clock.Set(at("10:02"))
	b.write(t, nil, set(x, "status", "rejected"), set(x, "decided_by", "SHA256:mac"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		rec := r.get(t, x)
		if rec.Status != "approved" || str(rec.Fields["decided_by"].Value) != "SHA256:phone" {
			t.Fatalf("%s: status %q by %s, want the first decision", r.name, rec.Status, rec.Fields["decided_by"].Value)
		}
		if fl := flagsOf(rec, FlagDecidedTwice); len(fl) != 1 || len(fl[0].Values) != 2 || str(fl[0].Values[1].Value) != "rejected" {
			t.Fatalf("%s: flags %+v, want decided twice", r.name, rec.Flags)
		}
		if len(flagsOf(rec, FlagConflict)) != 0 {
			t.Fatalf("%s: a decision raised a last-writer conflict: %+v", r.name, rec.Flags)
		}
		if m := r.get(t, made); m.Deleted {
			t.Fatalf("%s: the record A made from X is gone", r.name)
		}
	}
}

// E11. An unknown field.
func TestE11UnknownField(t *testing.T) {
	a, b, id, _ := twoHosts(t, TypeTask, map[string]any{"title": "T"})
	// A runs a newer build: its next op sets a field this build does not know.
	va, _ := a.Vector(context.Background())
	seq := va[a.Origin()] + 1
	before := b.get(t, id)
	newer := rawOp(t, map[string]any{"v": 1, "o": a.Origin(), "s": seq, "w": at("10:00").UnixMilli(), "c": 0,
		"bl": seq, "k": "set", "r": id, "root": before.Root.String(), "f": "estimate", "val": 3})
	if _, err := b.Apply(context.Background(), [][]byte{newer}); err != nil {
		t.Fatal(err)
	}
	st, _ := b.Status(context.Background())
	if st.Unfolded != 1 || st.Vector[a.Origin()] != seq {
		t.Fatalf("B status %+v: want the op stored, counted in the vector, not folded", st)
	}
	if rec := b.get(t, id); rec.Revision != before.Revision || rec.Fields["estimate"].Value != nil {
		t.Fatalf("B folded the unknown field: %+v", rec)
	}
	// B forwards it as it came.
	c := newReplica(t, "C")
	push(t, b, c)
	ops, _ := c.History(context.Background(), id)
	if got := string(ops[len(ops)-1].Raw()); got != string(newer) {
		t.Fatalf("C got %s, want the bytes A sent", got)
	}
	// B updates: the new build knows the field, and the rebuild folds it.
	fields["estimate"] = fieldSpec{kind: scalarInt, types: []string{TypeTask, TypeIssue}}
	defer delete(fields, "estimate")
	if err := b.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := b.get(t, id); string(rec.Fields["estimate"].Value) != "3" || rec.Revision != before.Revision+1 {
		t.Fatalf("after the update: %+v, want estimate 3", rec.Fields)
	}
}
