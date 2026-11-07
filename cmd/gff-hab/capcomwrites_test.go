package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// replyAfter returns the first Capcom reply that follows the event item whose text holds
// marker, or "".
func replyAfter(items []api.CapcomItem, marker string) (api.CapcomItem, bool) {
	for i, it := range items { // newest first
		if (it.Kind == "event" || it.Kind == "round") && strings.Contains(it.Text, marker) {
			for j := i - 1; j >= 0; j-- {
				if items[j].Kind == "message" && items[j].Author.Kind == "capcom" {
					return items[j], true
				}
			}
		}
	}
	return api.CapcomItem{}, false
}

func notifyAt(t *testing.T, sock, level, title string) {
	t.Helper()
	if out, err := exec.Command(bin, "notify", "-socket", sock, "-level", level, title).CombinedOutput(); err != nil {
		t.Fatalf("notify: %v %s", err, out)
	}
}

func setSetting(c *control.Client, id, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got api.SettingsResult
	if err := c.Call(ctx, "settings.get", api.SettingsGetParams{IDs: []string{id}}, &got); err != nil || len(got.Settings) != 1 {
		return err
	}
	return c.Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{{ID: id, Value: []byte(value), Revision: got.Settings[0].Revision}}}, nil)
}

// Capcom writes first end to end with the stub engine (capcom-service.md 9, 18 A7): an active
// notice reaches the engine as one event turn whose reply is marked wrote first; a passive one
// reaches nothing; an event turn's hand-off and start are refused with "ask the user first",
// while a user turn's are not; a capcom round on this hab is accepted, its outcome told or quiet;
// with round messages off it is declined off; settings.set refuses a value outside a range.
func TestCapcomWritesFirst(t *testing.T) {
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

	notifyAt(t, e.sock, "passive", "silent marker")
	notifyAt(t, e.sock, "active", "disk full")
	waitUntil(t, "the event turn's reply", func() bool { _, ok := replyAfter(threadText(t, c), "disk full"); return ok })
	r, _ := replyAfter(threadText(t, c), "disk full")
	if !r.Event {
		t.Fatalf("the reply is not marked wrote first: %+v", r)
	}
	for _, it := range threadText(t, c) {
		if strings.Contains(it.Text, "silent marker") {
			t.Fatalf("a passive notice reached the thread: %+v", it)
		}
	}
	if !strings.Contains(e.stubLog(), "event batch b") {
		t.Fatalf("no event batch in the stub log:\n%s", e.stubLog())
	}

	// An event turn may not act (9.4); a user turn may.
	for _, ask := range []string{"try handoff", "try start"} {
		notifyAt(t, e.sock, "active", ask)
		waitUntil(t, "the reply to "+ask, func() bool { _, ok := replyAfter(threadText(t, c), ask); return ok })
		if r, _ := replyAfter(threadText(t, c), ask); !strings.Contains(r.Text, "ask the user first") || !strings.Contains(r.Text, `"denied"`) {
			t.Fatalf("%s in an event turn: %q", ask, r.Text)
		}
	}
	if r := e.sayWait(t, c, "try handoff"); strings.Contains(r, "ask the user first") {
		t.Fatalf("a user turn's hand-off was refused as an event turn's: %q", r)
	}

	// Out of range (19.1).
	for id, v := range map[string]string{"capcom.events.daily_turns": "0", "capcom.events.batch_window": "9", "capcom.approval_max_age": "0"} {
		if err := setSetting(c, id, v); errKind(err) != api.ErrorKindInvalid {
			t.Fatalf("settings.set %s %s: %v", id, v, err)
		}
	}

	// A capcom round on the home hab itself (9.5): accepted, then the outcome.
	mustRound(t, e.habEnv, "add", "cc", "-mode", "capcom", "-manual", "check the builds")
	mustRound(t, e.habEnv, "run", "cc")
	run1 := waitRun(t, e.habEnv, c, "cc", api.RoundRunStateSucceeded)
	if run1.Reason != reasonAccepted {
		t.Fatalf("run %+v", run1)
	}
	waitUntil(t, "outcome told", func() bool {
		runs := roundRuns(t, c, "cc")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})
	mustRound(t, e.habEnv, "add", "cq", "-mode", "capcom", "-manual", "stay quiet")
	mustRound(t, e.habEnv, "run", "cq")
	waitRun(t, e.habEnv, c, "cq", api.RoundRunStateSucceeded)
	t.Cleanup(func() {
		if t.Failed() {
			for _, r := range roundRuns(t, c, "cq") {
				t.Logf("cq run %+v outcome %+v", r, r.Outcome)
			}
		}
	})
	waitUntil(t, "outcome quiet", func() bool {
		runs := roundRuns(t, c, "cq")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "quiet"
	})
	if err := setSetting(c, "capcom.events.round_messages", "false"); err != nil {
		t.Fatal(err)
	}
	mustRound(t, e.habEnv, "run", "cc")
	r2 := waitRun(t, e.habEnv, c, "cc", api.RoundRunStateSkipped)
	if r2.Reason != "declined by Capcom: off" {
		t.Fatalf("declined run %+v", r2)
	}
}
