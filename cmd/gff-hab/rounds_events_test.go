package main

// Event rounds end to end (rounds.md 4.3, 4.5, 14.1) with the real gff-hab and holder: two
// notices make one run whose GFF_ROUND_EVENTS file holds both; a pending batch outlives a
// planned and a killed restart and makes one run; export then apply of event fields gives no
// diff.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// eventFile reads a run's copied GFF_ROUND_EVENTS file.
func eventFile(t *testing.T, path string) rounds.Batch {
	t.Helper()
	var b rounds.Batch
	waitUntil(t, "the run's events file", func() bool {
		data, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(data, &b) == nil
	})
	return b
}

func notify(t *testing.T, e *habEnv, title string) {
	t.Helper()
	if out, err := exec.Command(bin, "notify", "-socket", e.sock, "-level", "active", title).CombinedOutput(); err != nil {
		t.Fatalf("notify: %v %s", err, out)
	}
}

func TestRoundEvents(t *testing.T) {
	e, repo := newRoundEnvWith(t, "GFF_TEST_ROUND_MIN_BATCH=500ms")
	writeFile(t, filepath.Join(e.work, "hab.toml"), "[rounds]\nwake_settle = \"0s\"\nmax_running = 4\nstate_save_every = \"1s\"\n")
	e.restart(false)
	c := e.control()
	out := filepath.Join(e.work, "events.json")
	copyEvents := `cp "$GFF_ROUND_EVENTS" ` + out + `.tmp && mv ` + out + `.tmp ` + out

	// Two notices within the debounce make one run with both events.
	mustRound(t, e, "add", "on-notice", "-on", "notice.raised", "-where", "text=disk", "-debounce", "1s", "-cooldown", "1s",
		"-project", repo, "-command", "--", "/bin/sh", "-c", copyEvents)
	notify(t, e, "disk almost full")
	notify(t, e, "build failed") // not matched
	notify(t, e, "disk full")
	done := waitRun(t, e, c, "on-notice", api.RoundRunStateSucceeded)
	if done.Trigger != api.RoundTriggerKindEvent || done.Events != 2 {
		t.Fatalf("run %+v", done)
	}
	b := eventFile(t, out)
	if len(b.Events) != 2 || b.Events[0].Title != "disk almost full" || b.Events[1].Title != "disk full" || b.Events[0].Level != "active" {
		t.Fatalf("events file %+v", b)
	}
	if runs := roundRuns(t, c, "on-notice"); len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	if r := getRound(t, c, "on-notice"); r.Pending != 0 || r.Trigger.Filter == nil || r.Trigger.Filter.Text != "disk" {
		t.Errorf("round after its run %+v", r)
	}

	// A pending batch outlives a killed restart (once saved) and a planned one.
	os.Remove(out)
	mustRound(t, e, "add", "kept", "-on", "notice.raised", "-where", "text=kept", "-debounce", "1h",
		"-project", repo, "-command", "--", "/bin/sh", "-c", copyEvents)
	notify(t, e, "kept one")
	waitUntil(t, "one pending", func() bool { return getRound(t, c, "kept").Pending == 1 })
	time.Sleep(1500 * time.Millisecond) // past state_save_every
	e.restart(true)
	c = e.control()
	if r := getRound(t, c, "kept"); r.Pending != 1 {
		t.Fatalf("after a kill: pending %d", r.Pending)
	}
	notify(t, e, "kept two")
	waitUntil(t, "two pending", func() bool { return getRound(t, c, "kept").Pending == 2 })
	e.restart(false) // the stop saves the batch at once
	c = e.control()
	if r := getRound(t, c, "kept"); r.Pending != 2 {
		t.Fatalf("after a planned restart: pending %d", r.Pending)
	}
	mustRound(t, e, "run", "kept") // run now takes the batch
	done = waitRun(t, e, c, "kept", api.RoundRunStateSucceeded)
	if done.Events != 2 {
		t.Fatalf("run %+v", done)
	}
	if b := eventFile(t, out); len(b.Events) != 2 || b.Events[0].Title != "kept one" || b.Events[1].Title != "kept two" {
		t.Fatalf("events file %+v", b)
	}
	if runs := roundRuns(t, c, "kept"); len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}

	// Export then apply: the event fields round-trip with no diff.
	exported := mustRound(t, e, "export")
	for _, w := range []string{`on = "notice.raised"`, `text = "disk"`, `debounce = "1s"`, `debounce = "1h0m0s"`} {
		if !strings.Contains(exported, w) {
			t.Errorf("export lacks %q:\n%s", w, exported)
		}
	}
	file := filepath.Join(e.work, "rounds.toml")
	writeFile(t, file, exported)
	if out := mustRound(t, e, "apply", file); strings.TrimSpace(out) != "no changes" {
		t.Fatalf("apply of the exported file: %s", out)
	}
}
