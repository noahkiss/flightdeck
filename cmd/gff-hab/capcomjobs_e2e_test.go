package main

// Capcom jobs end to end with the stub engine and the hab's own round runner
// (capcom-service.md 9.6, 18 A9): a capcom round with no words gets its event turn's hand-off
// refused; a person grants hand_off, and under allow the hand-off goes through the route, a
// card names the job, and the outcome is acted; under ask the same call opens one kind action
// confirmation naming the job, runs nothing, and the outcome is still acted; the user's confirm
// runs it once through the route; under off it is refused. The words come from the runner's
// message: a pane's create with words is refused.

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

func TestCapcomJobsE2E(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := newServiceEnv(t, "\n[settings]\n\"capcom.quiet_hours\" = \"00:00-00:00\"\n\"capcom.events.batch_max\" = 1\n")
	e.serve()
	c := e.control()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stub log:\n%s", e.stubLog())
		}
	})
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	waitUntil(t, "the engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	waitUntil(t, "capcom-service in this hab's catalog", func() bool {
		ent, service, ok := capcomHome(catalogOf(t, c))
		return ok && service && ent.Catalog.Capcom != nil && ent.Catalog.Capcom.Service != ""
	})
	outcome := func(name string, n int) string {
		runs := roundRuns(t, c, name)
		if len(runs) < n || runs[0].Outcome == nil {
			return ""
		}
		return runs[0].Outcome.Word
	}
	replyTo := func(n int) string {
		t.Helper()
		var r string
		waitUntil(t, "the job's reply", func() bool {
			var got []string
			for _, it := range threadText(t, c) {
				if it.Kind == "message" && it.Author.Kind == "capcom" && strings.HasPrefix(it.Text, "handoff: ") {
					got = append(got, it.Text)
				}
			}
			if len(got) >= n {
				r = got[0] // newest first
				return true
			}
			return false
		})
		return r
	}

	// A job with no words: refused as in A7.
	mustRound(t, e.habEnv, "add", "mail", "-mode", "capcom", "-manual", "try handoff")
	mustRound(t, e.habEnv, "run", "mail")
	if r := waitRun(t, e.habEnv, c, "mail", api.RoundRunStateSucceeded); r.Reason != reasonAccepted {
		t.Fatalf("run %+v", r)
	}
	if r := replyTo(1); !strings.Contains(r, "ask the user first") {
		t.Fatalf("no words: %q", r)
	}
	waitUntil(t, "outcome told", func() bool { return outcome("mail", 1) == "told" })

	// A person grants hand_off; under allow (the default) the call goes through the route.
	mustRound(t, e.habEnv, "edit", "mail", "-yes", "-actions", "hand_off")
	if r := getRound(t, c, "mail"); len(r.Actions) != 1 || r.Actions[0] != "hand_off" {
		t.Fatalf("after the grant: %+v", r.Actions)
	}
	mustRound(t, e.habEnv, "run", "mail")
	if r := replyTo(2); strings.Contains(r, "ask the user first") || !strings.Contains(r, "nobody") {
		t.Fatalf("allow: %q", r)
	}
	waitUntil(t, "outcome acted", func() bool { return outcome("mail", 2) == "acted" })
	card := false
	for _, it := range threadText(t, c) {
		if it.Kind == "card" && it.Card != nil && it.Card.Type == cardJob && strings.Contains(it.Text, "/mail,") {
			card = true
		}
	}
	if !card {
		t.Fatal("no card names the job")
	}

	// ask: one confirmation naming the job; nothing ran; the outcome is acted before the tap.
	if err := setSetting(c, "capcom.event_actions", `"ask"`); err != nil {
		t.Fatal(err)
	}
	mustRound(t, e.habEnv, "run", "mail")
	if r := replyTo(3); !strings.Contains(r, "waits for the user") {
		t.Fatalf("ask: %q", r)
	}
	waitUntil(t, "outcome acted under ask", func() bool { return outcome("mail", 3) == "acted" })
	var l api.ConfirmList
	call(t, c, "confirm.list", api.ConfirmListParams{}, &l)
	if len(l.Confirmations) != 1 || l.Confirmations[0].Kind != "action" || l.Confirmations[0].Action == nil || !strings.HasSuffix(l.Confirmations[0].Action.Job, "/mail") {
		t.Fatalf("confirmations %+v", l.Confirmations)
	}
	// The tap runs the call through the route: channel.send answers that nobody is there.
	var done api.ConfirmationResult
	call(t, c, "confirm.answer", api.ConfirmAnswerParams{ID: l.Confirmations[0].ID, Choice: "confirm"}, &done)
	if done.Confirmation.State != "failed" || !strings.Contains(done.Confirmation.Reason, "nobody") {
		t.Fatalf("confirmed: %+v", done.Confirmation)
	}

	// off: refused, whatever the job says.
	if err := setSetting(c, "capcom.event_actions", `"off"`); err != nil {
		t.Fatal(err)
	}
	mustRound(t, e.habEnv, "run", "mail")
	if r := replyTo(4); !strings.Contains(r, "capcom.event_actions is off") {
		t.Fatalf("off: %q", r)
	}

	// The settings' ranges (19.1).
	for id, v := range map[string]string{"capcom.event_actions": `"sometimes"`, "capcom.event_actions_per_turn": "21", "capcom.event_action_max_age": "0"} {
		if err := setSetting(c, id, v); errKind(err) != api.ErrorKindInvalid {
			t.Fatalf("settings.set %s %s: %v", id, v, err)
		}
	}
}
