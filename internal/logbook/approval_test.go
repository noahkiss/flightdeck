package logbook

import (
	"context"
	"slices"
	"testing"
)

// Approval (logbook.md 9.3): an approve batch carries the proposing agent as its actor with the
// approving device in approved_by. When a decision stamped before it stands, what it made is
// hidden and flagged.

var approver = &Actor{Kind: "agent", Harness: "claude", Session: "s1", ApprovedBy: "SHA256:phone"}

// proposalHosts makes A and B with a pending proposal X and a task T, known to both.
func proposalHosts(t *testing.T) (a, b *replica, x, task string) {
	t.Helper()
	a, b, x, _ = twoHosts(t, TypeProposal, map[string]any{"title": "remember X", "target": "record",
		"draft": map[string]any{"changes": []any{}}})
	task = NewID(TypeTask)
	a.write(t, nil, create(task, TypeTask, map[string]any{"title": "T"}))
	link(t, a, b)
	return a, b, x, task
}

// approve writes X's approve on r as one batch: a new task linked from X, a status change on
// task, and the decision.
func approve(t *testing.T, r *replica, x, task string) string {
	t.Helper()
	made := NewID(TypeTask)
	r.write(t, approver, create(made, TypeTask, map[string]any{"title": "X"}), add(made, "links", Link{Kind: LinkFrom, To: x}),
		set(task, "status", "doing"), set(x, "status", "approved"), set(x, "decided_by", "SHA256:phone"))
	return made
}

func reject(t *testing.T, r *replica, x string) {
	t.Helper()
	r.write(t, &Actor{Kind: "person", Device: "SHA256:mac"}, set(x, "status", "rejected"), set(x, "decided_by", "SHA256:mac"))
}

// refoldSame folds r again from its ops and wants the same folded tables (P2).
func refoldSame(t *testing.T, r *replica) {
	t.Helper()
	before := dump(t, r.Store)
	if err := r.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, r.Store); after != before {
		t.Fatalf("%s: rebuild differs from the incremental fold:\n%s\n---\n%s", r.name, before, after)
	}
}

func TestRejectWinsOverApprove(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	b.clock.Set(at("09:58"))
	reject(t, b, x)
	a.clock.Set(at("10:00"))
	made := approve(t, a, x, task)
	revA := a.get(t, made).Revision
	if m := a.get(t, made); m.Deleted || a.get(t, task).Status != "doing" {
		t.Fatalf("A before the link: approve not applied: %+v", m)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		p := r.get(t, x)
		if p.Status != "rejected" || len(flagsOf(p, FlagDecidedTwice)) != 1 {
			t.Fatalf("%s: proposal %q flags %+v, want rejected and decided twice", r.name, p.Status, p.Flags)
		}
		m := r.get(t, made)
		if !m.Deleted {
			t.Fatalf("%s: the overruled approve's record is visible", r.name)
		}
		fl := flagsOf(m, FlagApprovalOverruled)
		if len(fl) != 1 || len(fl[0].Values) != 1 || str(fl[0].Values[0].Value) != "rejected" {
			t.Fatalf("%s: flags %+v, want approval overruled by the reject", r.name, m.Flags)
		}
		if r == a && m.Revision <= revA {
			t.Fatalf("A: revision %d after the overrule, was %d: it must go up", m.Revision, revA)
		}
		tk := r.get(t, task)
		if tk.Status != "open" || len(flagsOf(tk, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: task %q flags %+v, want the overruled change not folded and flagged", r.name, tk.Status, tk.Flags)
		}
		refoldSame(t, r)
	}
	if dump(t, a.Store) != dump(t, b.Store) {
		t.Fatal("A and B folded differently")
	}
	// A person restores the hidden record: it shows again, still flagged.
	a.clock.Set(at("11:00"))
	a.write(t, phone, set(made, "deleted", false))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if m := r.get(t, made); m.Deleted || len(flagsOf(m, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: after restore: deleted %v flags %+v", r.name, m.Deleted, m.Flags)
		}
		refoldSame(t, r)
	}
}

func TestApproveWinsOverReject(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	a.clock.Set(at("10:00"))
	made := approve(t, a, x, task)
	b.clock.Set(at("10:02"))
	reject(t, b, x)
	if _, err := b.Get(context.Background(), made); err == nil {
		t.Fatal("B holds A's record before the link")
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if p := r.get(t, x); p.Status != "approved" || len(flagsOf(p, FlagDecidedTwice)) != 1 {
			t.Fatalf("%s: proposal %q flags %+v", r.name, p.Status, p.Flags)
		}
		m := r.get(t, made)
		if m.Deleted || len(m.Flags) != 0 {
			t.Fatalf("%s: the standing approve's record: deleted %v flags %+v", r.name, m.Deleted, m.Flags)
		}
		if tk := r.get(t, task); tk.Status != "doing" {
			t.Fatalf("%s: task status %q, want the approve's change", r.name, tk.Status)
		}
		refoldSame(t, r)
	}
}

// A third host gets the approve before the proposal and the earlier reject: the record shows,
// then hides when the reject folds, and Changes names it.
func TestOverruleArrivesLate(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	b.clock.Set(at("09:58"))
	reject(t, b, x)
	a.clock.Set(at("10:00"))
	made := approve(t, a, x, task)
	c := newReplica(t, "C")
	c.clock.Set(at("10:05"))
	push(t, a, c) // A holds the proposal, the task and the approve, not the reject
	if m := c.get(t, made); m.Deleted {
		t.Fatal("C: record hidden before any reject arrived")
	}
	cur, err := c.Cursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	push(t, b, c)
	if m := c.get(t, made); !m.Deleted || len(flagsOf(m, FlagApprovalOverruled)) != 1 {
		t.Fatalf("C: after the reject: deleted %v flags %+v", m.Deleted, m.Flags)
	}
	ids, _, err := c.Changes(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, made) || !slices.Contains(ids, task) || !slices.Contains(ids, x) {
		t.Fatalf("Changes after the reject: %v, want %s, %s and %s", ids, x, made, task)
	}
	refoldSame(t, c)
	link(t, a, b)
	if dump(t, a.Store) != dump(t, c.Store) {
		t.Fatal("A and C folded differently")
	}
}

// Two approves made apart: the first stands, the second's record is hidden, so nothing is
// applied twice.
func TestTwoApprovesApplyOnce(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	a.clock.Set(at("10:00"))
	first := approve(t, a, x, task)
	b.clock.Set(at("10:01"))
	second := approve(t, b, x, task)
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if m := r.get(t, first); m.Deleted {
			t.Fatalf("%s: first approve's record hidden", r.name)
		}
		if m := r.get(t, second); !m.Deleted || len(flagsOf(m, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: second approve's record: deleted %v flags %+v", r.name, m.Deleted, m.Flags)
		}
		refoldSame(t, r)
	}
}

func TestExpiresIsFixed(t *testing.T) {
	r := newReplica(t, "A")
	x := NewID(TypeProposal)
	if _, err := r.Write(context.Background(), Batch{Steps: []Step{create(x, TypeProposal,
		map[string]any{"title": "m", "target": "memory", "expires": "soon"})}}); !isInvalid(err) {
		t.Fatalf("expires that is not a time: %v, want invalid", err)
	}
	r.write(t, nil, create(x, TypeProposal, map[string]any{"title": "m", "target": "memory", "expires": "2026-12-02T00:00:00Z"}))
	if _, err := r.Write(context.Background(), Batch{Steps: []Step{set(x, "expires", "2027-01-01T00:00:00Z")}}); !isInvalid(err) {
		t.Fatalf("set of expires: %v, want invalid", err)
	}
	if v := str(r.get(t, x).Fields["expires"].Value); v != "2026-12-02T00:00:00Z" {
		t.Fatalf("expires %q", v)
	}
}

// An overruled change to a deleted record is not folded, so it is no edit after the delete: the
// record shows approval overruled, never edited after delete (5.6, 9.3).
func TestOverruledEditNotEditedAfterDelete(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	a.clock.Set(at("09:30"))
	a.write(t, phone, set(task, "deleted", true))
	a.clock.Set(at("09:58"))
	reject(t, a, x)
	// B has seen neither: its approve notes the task.
	b.clock.Set(at("10:00"))
	made := NewID(TypeTask)
	b.write(t, approver, create(made, TypeTask, map[string]any{"title": "X"}), note(task, "from the draft"),
		set(x, "status", "approved"), set(x, "decided_by", "SHA256:phone"))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		tk := r.get(t, task)
		if !tk.Deleted || len(tk.Notes) != 0 || len(flagsOf(tk, FlagApprovalOverruled)) != 1 || len(flagsOf(tk, FlagEditedAfterDelete)) != 0 {
			t.Fatalf("%s: task deleted %v notes %d flags %+v", r.name, tk.Deleted, len(tk.Notes), tk.Flags)
		}
		refoldSame(t, r)
	}
}

// A proposal N that an overruled approve of X made, whose root sorts before X's ("10@A" before
// "1@A"): N folds after X's standing decision whatever the roots' order, so every replica hides
// and flags it, and a rebuild agrees with the incremental fold.
func TestProposalMadeByOverruledApprove(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	b.clock.Set(at("09:58"))
	reject(t, b, x)
	a.clock.Set(at("10:00"))
	for i := 0; i < 7; i++ { // seqs 3 to 9 on A, so N's create is seq 10
		a.write(t, phone, note(task, "filler"))
	}
	n := NewID(TypeProposal)
	res := a.write(t, approver, create(n, TypeProposal, map[string]any{"title": "N", "target": "record",
		"draft": map[string]any{"changes": []any{}}}), set(x, "status", "approved"), set(x, "decided_by", "SHA256:phone"))
	if s := res.Ops[0].String(); s >= a.get(t, x).Root.String() {
		t.Fatalf("N's root %s does not sort before X's %s", s, a.get(t, x).Root)
	}
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		p := r.get(t, n)
		if !p.Deleted || len(flagsOf(p, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: N deleted %v flags %+v, want hidden and approval overruled", r.name, p.Deleted, p.Flags)
		}
		refoldSame(t, r)
	}
	if dump(t, a.Store) != dump(t, b.Store) {
		t.Fatal("A and B folded differently")
	}
}

// Only a person's restore shows an overruled approve's record again (9.4): an agent's restore
// stamped after the approve leaves it hidden, a person's shows it, still flagged.
func TestOnlyPersonRestoresOverruled(t *testing.T) {
	a, b, x, task := proposalHosts(t)
	b.clock.Set(at("09:58"))
	reject(t, b, x)
	a.clock.Set(at("10:00"))
	made := approve(t, a, x, task)
	link(t, a, b)
	a.clock.Set(at("11:00"))
	a.write(t, agentS1, set(made, "deleted", false))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if m := r.get(t, made); !m.Deleted {
			t.Fatalf("%s: an agent's restore showed the overruled record", r.name)
		}
		refoldSame(t, r)
	}
	b.clock.Set(at("11:30"))
	b.write(t, phone, set(made, "deleted", false))
	link(t, a, b)
	for _, r := range []*replica{a, b} {
		if m := r.get(t, made); m.Deleted || len(flagsOf(m, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: after a person's restore: deleted %v flags %+v", r.name, m.Deleted, m.Flags)
		}
		refoldSame(t, r)
	}
}

// Two approves that each touch the other's proposal (9.4, Fold order): X's approve on A notes Y,
// and Y's approve on B, which a reject on A beat, notes X. Each proposal's fold reads the other's
// standing decision, so no fold order serves both; the standing decision comes from the
// proposal's own ops instead. On A, Y's overruled note on X must be hidden and X flagged, as a
// fresh replica folds it.
func TestApprovesTouchingEachOther(t *testing.T) {
	a, b := newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:00"))
	b.clock.Set(at("09:00"))
	fields := map[string]any{"title": "P", "target": "record", "draft": map[string]any{"changes": []any{}}}
	x, y := NewID(TypeProposal), NewID(TypeProposal)
	a.write(t, nil, create(x, TypeProposal, fields))
	a.write(t, nil, create(y, TypeProposal, fields))
	link(t, a, b)
	if xr, yr := a.get(t, x).Root.String(), a.get(t, y).Root.String(); xr >= yr {
		t.Fatalf("X's root %s does not sort before Y's %s", xr, yr)
	}
	a.clock.Set(at("09:30"))
	reject(t, a, y)
	a.clock.Set(at("09:40"))
	a.write(t, approver, note(y, "from X's approve"), set(x, "status", "approved"), set(x, "decided_by", "SHA256:phone"))
	b.clock.Set(at("10:00"))
	b.write(t, approver, note(x, "from Y's approve"), set(y, "status", "approved"), set(y, "decided_by", "SHA256:phone"))
	link(t, a, b)
	obs := newReplica(t, "obs")
	push(t, a, obs)
	for _, r := range []*replica{a, b, obs} {
		p := r.get(t, x)
		if len(p.Notes) != 0 || len(flagsOf(p, FlagApprovalOverruled)) != 1 {
			t.Fatalf("%s: X notes %+v flags %+v, want Y's overruled note hidden and X flagged", r.name, p.Notes, p.Flags)
		}
		if q := r.get(t, y); len(q.Notes) != 1 || len(flagsOf(q, FlagApprovalOverruled)) != 0 {
			t.Fatalf("%s: Y notes %+v flags %+v, want X's note kept", r.name, q.Notes, q.Flags)
		}
		refoldSame(t, r)
	}
	if dump(t, a.Store) != dump(t, b.Store) || dump(t, a.Store) != dump(t, obs.Store) {
		t.Fatal("A, B and a fresh replica folded differently")
	}
}
