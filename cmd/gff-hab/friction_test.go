package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/friction"
	"github.com/noahkiss/flightdeck/internal/trace"
)

// TestFrictionNotes is diagnostics test 6 and the stage 1 acceptance for part (e): a note from
// gff (gff-hab friction, the command gff friction runs) and one from the app (diag.friction
// from a client of kind app) each land with their text, session and a log tail that holds that
// session's lines. The text never enters hab.log. list and show read the notes (9.2).
func TestFrictionNotes(t *testing.T) {
	t.Parallel()
	e := newHabEnv(t, 0)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "fric", Argv: []string{"/bin/sh", "-c", "exit 3"},
		Cwd: "/", Env: []string{"PATH=/usr/bin:/bin"}}, &res); err != nil {
		t.Fatal(err)
	}
	// The test's holder writes to the test's stderr; a holder run by a service manager writes
	// its file in the logs folder, in local time to the second. One line names the pane.
	holderLine := "gff-hab-holder: " + time.Now().Format("2006/01/02 15:04:05") + " pane " + res.Pane + ": spawned\n"
	if err := os.WriteFile(filepath.Join(logsDir(e.state), holderLogPrefix+"one.log"), []byte(holderLine), 0o600); err != nil {
		t.Fatal(err)
	}

	const cliText, appText = "went back to tmux to split\tpanes", "the phone keyboard hid the composer"
	out, err := exec.Command(bin, "friction", "-socket", e.sock, "-session", "fric", "-blocker", cliText).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "friction note ") {
		t.Fatalf("gff-hab friction: %v\n%s", err, out)
	}
	cliTrace := strings.Fields(string(out))[2]
	if !trace.Valid(cliTrace) || cliTrace[0] != trace.Hab {
		t.Fatalf("cli note trace %q: gff-hab friction sends none, so the hab's h trace", cliTrace)
	}
	// The app's note takes its request's trace.
	appTrace := trace.New(trace.App)
	var noted api.FrictionNoted
	if err := c.Call(trace.With(ctx, appTrace), "diag.friction", api.FrictionParams{Text: "  " + appText + "\n", Session: res.Session, Pane: res.Pane}, &noted); err != nil {
		t.Fatal(err)
	}
	if noted.Trace != appTrace {
		t.Fatalf("trace %q, want the request's %s", noted.Trace, appTrace)
	}

	notes, bad, err := friction.List(filepath.Join(e.state, friction.Dir), time.Time{})
	if err != nil || bad != 0 || len(notes) != 2 {
		t.Fatalf("notes: %d, bad %d, %v", len(notes), bad, err)
	}
	for i, want := range []struct {
		trace, text, client string
		blocker             bool
		pane                string
	}{{cliTrace, cliText, "cli", true, ""}, {noted.Trace, appText, "app", false, res.Pane}} {
		n := notes[i]
		if n.Trace != want.trace || n.Text != want.text || n.Client != want.client || n.Blocker != want.blocker ||
			n.Session != res.Session || n.Pane != want.pane || n.Version != "dev" {
			t.Errorf("note %d: %+v", i, n)
		}
		found := false
		for _, l := range n.Log {
			if l.T.IsZero() || l.Proc == "" {
				t.Errorf("note %d: tail line %+v", i, l)
			}
			found = found || strings.Contains(l.Text, res.Pane) || strings.Contains(string(l.Line), res.Pane)
		}
		if !found {
			t.Errorf("note %d: the tail holds no line about the session's pane %s: %+v", i, res.Pane, n.Log)
		}
		st, err := os.Stat(filepath.Join(e.state, friction.Dir))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("friction folder: %v", err)
		}
	}

	// The text is the note's alone (10.1): hab.log names the trace, never the words.
	waitUntil(t, "the stored lines in hab.log", func() bool {
		return strings.Count(habLog(t, e.state), "friction note stored: trace ") == 2
	})
	for _, w := range []string{"tmux", "keyboard"} {
		if strings.Contains(habLog(t, e.state), w) {
			t.Errorf("hab.log holds the note's word %q", w)
		}
	}

	var lb bytes.Buffer
	if err := frictionList([]string{"-state", e.state}, &lb, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := lb.String(); strings.Count(got, "\n") != 2 || !strings.Contains(got, "blocker") ||
		!strings.Contains(got, "went back to tmux to split panes") || !strings.Contains(got, res.Session) {
		t.Errorf("friction list:\n%s", got)
	}
	var sb bytes.Buffer
	if err := frictionShow([]string{"-state", e.state, strings.ToLower(noted.Trace)}, &sb); err != nil {
		t.Fatal(err)
	}
	if got := sb.String(); !strings.Contains(got, appText) || !strings.Contains(got, "kind     preference") ||
		!strings.Contains(got, "log tail: ") || !strings.Contains(got, res.Pane) {
		t.Errorf("friction show:\n%s", got)
	}

	// Refused: no text, and text over the limit. Neither is stored.
	for _, p := range []api.FrictionParams{{Text: " \n"}, {Text: strings.Repeat("x", friction.MaxText+1)}} {
		if err := c.Call(ctx, "diag.friction", p, &noted); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("diag.friction %d characters: %v", len(p.Text), err)
		}
	}
	if notes, _, _ := friction.List(filepath.Join(e.state, friction.Dir), time.Time{}); len(notes) != 2 {
		t.Errorf("%d notes after two refused", len(notes))
	}
}

// TestFrictionNoHab: with no gff-hab answering, gff friction fails with a hint and keeps nothing
// (diagnostics.md 12).
func TestFrictionNoHab(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	out, err := exec.Command(bin, "friction", "-socket", filepath.Join(d, "none.sock"), "a note").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "is gff-hab running?") {
		t.Fatalf("gff-hab friction with no hab: %v\n%s", err, out)
	}
}

// TestFrictionTailForm: the tail keeps a structured line as its JSON object, not an escaped
// string, and keeps its continuation lines apart in text; show prints both (review L7).
func TestFrictionTailForm(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if err := os.MkdirAll(logsDir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	at := now.Add(-time.Minute).Format("2006-01-02T15:04:05.000Z07:00")
	log := `{"t":"` + at + `","lvl":"warn","proc":"hab","comp":"link","msg":"read failed","err":"eof"}` + "\n" +
		"  a partial line\n" + "  another\n"
	if err := os.WriteFile(filepath.Join(logsDir(state), habLogFile), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := logTail(state, now)
	if len(tail) != 1 {
		t.Fatalf("tail %+v", tail)
	}
	l := tail[0]
	if !strings.HasPrefix(string(l.Line), `{"t":`) || !strings.Contains(string(l.Line), `"msg":"read failed"`) ||
		l.Text != "  a partial line\n  another" {
		t.Fatalf("tail line %s %q", l.Line, l.Text)
	}
	tr := trace.New(trace.Hab)
	if err := friction.Write(frictionDir(state), friction.Note{T: now, Trace: tr, Client: "cli", Text: "n", Log: tail}, now); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(frictionDir(state), "*.json"))
	if len(files) != 1 {
		t.Fatalf("notes %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Contains(string(b), `\"msg\"`) || !strings.Contains(string(b), `"line":{"t":`) {
		t.Errorf("the note's file holds the line escaped or indented:\n%s", b)
	}
	var out bytes.Buffer
	if err := frictionShow([]string{"-state", state, tr}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "read failed") || !strings.Contains(s, "a partial line") || strings.Contains(s, `"msg"`) {
		t.Errorf("show:\n%s", s)
	}
}
