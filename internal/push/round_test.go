package push

import (
	"testing"
)

// roundPush is one round notification as the fake APNs got it.
type roundPush struct {
	title, body, subtitle, level, category, collapse, kind, round, run, session string
}

func roundPushes(reqs []fakeReq, token string) []roundPush {
	var out []roundPush
	for _, q := range alerts(reqs, token) {
		aps := q.Body["aps"].(map[string]any)
		fd := q.Body["flightdeck"].(map[string]any)
		str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
		alert := aps["alert"].(map[string]any)
		out = append(out, roundPush{
			title: str(alert, "title"), body: str(alert, "body"), subtitle: str(alert, "subtitle"),
			level: str(aps, "interruption-level"), category: str(aps, "category"),
			collapse: q.Header.Get("apns-collapse-id"), kind: str(fd, "kind"),
			round: str(fd, "round"), run: str(fd, "run"), session: str(fd, "session"),
		})
	}
	return out
}

// TestRoundRows: each row of rounds.md 10.2 under each notify setting, through RoundAllowed,
// RoundNotice and Raise, against the fake APNs. A device that did not register the round kind
// gets none. notify = never keeps only the self-pause; failures leaves out the success.
func TestRoundRows(t *testing.T) {
	base := RoundEvent{Round: "r1", Name: "nightly", Run: "run7", RunNumber: 7, Session: "s1"}
	rows := []struct {
		name     string
		ev       func(RoundEvent) RoundEvent
		title    string
		body     string
		level    string
		category string
		session  string
		allowed  map[string]bool // notify -> sent
	}{
		{"failed", func(e RoundEvent) RoundEvent {
			e.Kind, e.State, e.Reason = RoundRunFailed, "failed", "exit status 2"
			return e
		}, "Round failed: nightly", "exit status 2", "active", CategoryOpen, "s1",
			map[string]bool{"failures": true, "always": true, "never": false}},
		{"timed out", func(e RoundEvent) RoundEvent {
			e.Kind, e.State, e.Reason = RoundRunFailed, "timed_out", "timed out after 1h0m0s"
			return e
		}, "Round timed out: nightly", "timed out after 1h0m0s", "active", CategoryOpen, "s1",
			map[string]bool{"failures": true, "always": true, "never": false}},
		{"succeeded", func(e RoundEvent) RoundEvent {
			e.Kind, e.State, e.Result = RoundRunSucceeded, "succeeded", "\n  All 14 checks passed.\nDetails follow."
			return e
		}, "Round done: nightly", "All 14 checks passed.", "passive", CategoryOpen, "s1",
			map[string]bool{"failures": false, "always": true, "never": false}},
		{"paused", func(e RoundEvent) RoundEvent {
			e.Kind, e.Failures = RoundPaused, 5
			return e
		}, "Round paused: nightly", "5 failed runs in a row. Unpause it to run again.", "active", CategoryOpen, "s1",
			map[string]bool{"failures": true, "always": true, "never": true}},
		{"skips", func(e RoundEvent) RoundEvent {
			e.Kind, e.Since, e.Reason = RoundSkips, "Nov 1 09:00 UTC", "in use: working"
			return e
		}, "Round not running: nightly", "Not run since Nov 1 09:00 UTC: in use: working", "passive", CategoryOpen, "s1",
			map[string]bool{"failures": true, "always": true, "never": false}},
		{"target gone", func(e RoundEvent) RoundEvent {
			e.Kind = RoundTargetGone
			return e
		}, "Round completed: nightly", "Its target session is gone.", "passive", "", "",
			map[string]bool{"failures": true, "always": true, "never": false}},
	}
	for _, row := range rows {
		for _, notify := range []string{"failures", "always", "never"} {
			t.Run(row.name+"/"+notify, func(t *testing.T) {
				r := newNotifierRig(t)
				r.store.Register("dev-phone", phone, Production, []string{KindRound}, false)
				r.store.Register("dev-mac", mac, Production, []string{KindPermission}, false)
				ev := row.ev(base)
				want := row.allowed[notify]
				if got := RoundAllowed(notify, ev.Kind); got != want {
					t.Fatalf("RoundAllowed(%s, %s) = %v", notify, ev.Kind, got)
				}
				if !want {
					return
				}
				no := RoundNotice(ev)
				no.ID = "n-" + row.name
				if n := r.n.Raise(no); n != 1 {
					t.Fatalf("raised to %d devices, want 1 (the mac did not register round)", n)
				}
				r.n.Sync()
				if m := alerts(r.prod.requests(), mac); len(m) != 0 {
					t.Fatalf("the mac got %d round pushes", len(m))
				}
				ps := roundPushes(r.prod.requests(), phone)
				if len(ps) != 1 {
					t.Fatalf("%d pushes", len(ps))
				}
				p := ps[0]
				wantP := roundPush{title: row.title, body: row.body, level: row.level, category: row.category,
					collapse: "round-r1", kind: KindRound, round: "r1", run: "run7", session: row.session}
				p.subtitle = ""
				if p != wantP {
					t.Fatalf("push\n got %+v\nwant %+v", p, wantP)
				}
			})
		}
	}
}

// TestRoundCollapse: a round failing three times in a row raises three notices with one
// collapse id: three updates of one banner. Another round has its own.
func TestRoundCollapse(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, []string{KindRound}, false)
	for i, reason := range []string{"exit status 1", "exit status 2", "exit status 3"} {
		no := RoundNotice(RoundEvent{Kind: RoundRunFailed, Round: "r1", Name: "hourly", RunNumber: i + 1, State: "failed", Reason: reason, Session: "s1"})
		no.ID = reason
		r.n.Raise(no)
	}
	no := RoundNotice(RoundEvent{Kind: RoundRunFailed, Round: "r2", Name: "daily", State: "failed", Reason: "exit status 9"})
	no.ID = "other"
	r.n.Raise(no)
	r.n.Sync()
	ps := roundPushes(r.prod.requests(), phone)
	if len(ps) != 4 {
		t.Fatalf("%d pushes, want 4", len(ps))
	}
	for i, p := range ps[:3] {
		if p.collapse != "round-r1" || p.body != []string{"exit status 1", "exit status 2", "exit status 3"}[i] {
			t.Errorf("push %d: %+v", i, p)
		}
	}
	if ps[3].collapse != "round-r2" || ps[3].category != "" {
		t.Errorf("the other round: %+v", ps[3])
	}
	if RoundCollapse("") != "round-state" || len(RoundCollapse(string(make([]byte, 100)))) > 64 {
		t.Error("collapse id bounds")
	}
}

// TestRoundStateSetAside: the state file row goes to devices that registered round, active,
// with no actions and its own collapse id.
func TestRoundStateSetAside(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, []string{KindRound}, false)
	no := RoundNotice(RoundEvent{Kind: RoundStateSetAside})
	no.ID = "n1"
	r.n.Raise(no)
	r.n.Sync()
	ps := roundPushes(r.prod.requests(), phone)
	if len(ps) != 1 || ps[0].title != "Rounds state set aside" || ps[0].level != "active" || ps[0].category != "" || ps[0].collapse != "round-state" {
		t.Fatalf("%+v", ps)
	}
}
