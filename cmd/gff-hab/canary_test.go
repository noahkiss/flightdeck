package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/friction"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// TestCanary is diagnostics test 11, the end-to-end guard of 10.1 and 10.2: one random string
// goes through pane input, terminal output (still on screen when the bundle is made, and in the
// region a detection rule reads), a chat send, a hook payload (sent, and failed), and an
// environment value, and a permission request a pane is still blocked on (its command, its
// description and its transcript). Afterwards no file under <state>/logs or <state>/tallies, no
// health record, no friction log tail, no gff-hab status output and no bundle file, detect.json
// included, holds it. A second string, a peer's label in the config, is a name: logs may hold
// it, under a name key or in a legacy line, and the bundle replaces it, so only the bundle and
// status are checked for it (8.3). A third, a session's name, is in a friction note's text,
// which only the name rule scrubs: the bundle and the archive are checked for it.
//
// The Capcom leg (a turn on a test engine that echoes the string on stderr and in a provider
// error) waits for the engine's capcom.log; see the spec's stage 5.
func TestCanary(t *testing.T) {
	t.Parallel()
	canary, label, sessName := "cnry"+randHex(6), "lbl"+randHex(6), "ses"+randHex(6)
	e := newHolderEnv(t, 0)
	d := shortDir(t)
	peersFile := filepath.Join(d, "peers.toml")
	writeFile(t, peersFile, "[[peers]]\nlabel = \""+label+"\"\nendpoints = [\"studio-gone\"]\n")
	standIn := sshStandIn(t, map[string]string{})
	e.env = []string{"GFF_HAB_PEERS=" + peersFile, "PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH")}
	e.serve()
	buildSleeper(t, e.work)
	// Claude's prompt box with the string typed into it: the region rule live_prompt_box reads.
	writeFile(t, filepath.Join(e.work, "screen"), strings.Replace(detectScreen, "❯\n", "❯ "+canary+"\n", 1))

	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	env := []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "BIN=" + bin, "CANARY_ENV=" + canary}
	var agent, shell api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "canary-agent", Cwd: "/", Cols: 80, Rows: 24, Env: env,
		Argv: []string{"/bin/sh", "-c", `cat "$DIR/screen"; exec "$DIR/claude" 600`}}, &agent); err != nil {
		t.Fatal(err)
	}
	// The shell prints the environment value, sends a hook with it in the payload through its
	// own GFF_HAB, then echoes what it is typed.
	script := `printf 'env %s\n' "$CANARY_ENV"
printf '{"prompt":"%s","hook_event_name":"UserPromptSubmit","session_id":"canary","cwd":"/"}' "$CANARY_ENV" | "$BIN" hook claude UserPromptSubmit
printf 'hook sent\n'
exec cat`
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: sessName, Cwd: "/", Cols: 80, Rows: 24, Env: env,
		Argv: []string{"/bin/sh", "-c", script}}, &shell); err != nil {
		t.Fatal(err)
	}
	// A pane blocked on a permission request whose command, description and transcript hold the
	// string, still blocked when the bundle is made (diagnostics review H1).
	writeFile(t, filepath.Join(e.work, "transcript.jsonl"), `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I will fetch `+canary+`"}]}}`+"\n")
	writeFile(t, filepath.Join(e.work, "permission.json"), `{"session_id":"canary-perm","cwd":"/","hook_event_name":"PermissionRequest","tool_name":"Bash",`+
		`"tool_input":{"command":"curl https://example.org/`+canary+`","description":"fetch `+canary+`"},"transcript_path":"`+filepath.Join(e.work, "transcript.jsonl")+`"}`)
	var perm api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "canary-perm", Cwd: "/", Cols: 80, Rows: 24, Env: env,
		Argv: []string{"/bin/sh", "-c", `printf '\033]7501;state=working:app=claude\033\\'; "$BIN" hook claude PermissionRequest < "$DIR/permission.json"; exec "$DIR/claude" 600`}}, &perm); err != nil {
		t.Fatal(err)
	}
	waitState(t, e.sock, perm.Pane, "blocked on the request", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	waitUntil(t, "the shell to send its hook", func() bool { return strings.Contains(screenOf(t, e, shell.Pane), "hook sent") })
	if err := c.Call(ctx, "pane.input", api.InputParams{Pane: shell.Pane, Text: "typed " + canary + "\n"}, nil); err != nil {
		t.Fatal(err)
	}
	// A chat send to a session with no agent is refused; its text must still go nowhere.
	c.Call(ctx, "session.send", api.SessionSendParams{Session: shell.Session, Text: "chat " + canary, Origin: api.SendOriginTyped}, nil)
	// A hook that finds no hab writes hook.log.
	hk := exec.Command(bin, "hook", "claude", "UserPromptSubmit")
	hk.Env = append(os.Environ(), "GFF_HAB_STATE="+e.state, "GFF_HAB="+hook.Addr(filepath.Join(d, "gone.sock"), "0123456789abcdef0123"))
	hk.Stdin = strings.NewReader(`{"prompt":"` + canary + `","hook_event_name":"UserPromptSubmit"}`)
	if err := hk.Run(); err != nil {
		t.Fatal(err)
	}
	var noted api.FrictionNoted
	if err := c.Call(ctx, "diag.friction", api.FrictionParams{Text: "went back to another terminal from " + sessName, Session: shell.Session}, &noted); err != nil {
		t.Fatal(err)
	}

	// Positive controls: the string is where the test put it, so its absence below means
	// something. The screen shows it; detection with text reads it in the winning rule's region.
	waitUntil(t, "the typed string on screen", func() bool { return strings.Count(screenOf(t, e, shell.Pane), canary) >= 2 })
	var withText string
	waitUntil(t, "detection to read the prompt box", func() bool {
		withText = run(t, "detect", "explain", "-socket", e.sock, "-json", agent.Pane)
		return strings.Contains(withText, canary) && strings.Contains(withText, "live_prompt_box")
	})
	// Detection with text shows the open request's command: -no-text must drop it.
	if js := run(t, "detect", "explain", "-socket", e.sock, "-json", perm.Pane); !strings.Contains(js, `"command": "curl https://example.org/`+canary) {
		t.Fatalf("detect explain shows no open request:\n%s", js)
	}
	if b, _ := os.ReadFile(filepath.Join(logsDir(e.state), hookLogFile)); !strings.Contains(string(b), "hook failed") {
		t.Fatalf("hook.log: %q", b)
	}
	waitUntil(t, "the label in a log line", func() bool { return strings.Contains(habLog(t, e.state), label) })

	status := runEnv(t, e.env, "status", "-socket", e.sock, "-state", e.state, "-holders", e.hdir)
	bundleDir := filepath.Join(d, "bundle")
	bundleArgs := []string{"bundle", "-socket", e.sock, "-state", e.state, "-holders", e.hdir, "-config", filepath.Join(d, "no-hab.toml")}
	out := runEnv(t, e.env, append(bundleArgs, "-dir", bundleDir)...)
	if !strings.Contains(out, "Review the files before you share them.") || !strings.Contains(out, "known-name") {
		t.Errorf("bundle output:\n%s", out)
	}
	archive := filepath.Join(d, "b.tar.gz")
	runEnv(t, e.env, append(bundleArgs, "-o", archive)...)

	// The string is in no diagnostics file, no status output and no bundle.
	check := func(where, data string, s string) {
		t.Helper()
		if strings.Contains(strings.ToLower(data), strings.ToLower(s)) {
			t.Errorf("%s holds %q:\n%s", where, s, data)
		}
	}
	for _, sub := range []string{"logs", "tallies"} {
		for p, data := range filesUnder(t, filepath.Join(e.state, sub)) {
			check(p, data, canary)
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.state, healthFile)); err == nil {
		check(healthFile, string(b), canary)
	}
	notes, _, _ := friction.List(filepath.Join(e.state, friction.Dir), time.Time{})
	if len(notes) != 1 || len(notes[0].Log) == 0 {
		t.Fatalf("friction notes %d", len(notes))
	}
	for _, l := range notes[0].Log {
		check("a friction tail", l.Text, canary)
	}
	bundle := filesUnder(t, bundleDir)
	if testing.Verbose() {
		for p, data := range bundle {
			t.Logf("%s:\n%s", p, data)
		}
	}
	if dj := bundle["detect.json"]; !strings.Contains(dj, "live_prompt_box") || !strings.Contains(dj, `"tool": "Bash"`) {
		t.Fatalf("detect.json in the bundle:\n%s", dj)
	}
	for _, s := range []string{canary, label, sessName} {
		if s != sessName {
			check("gff-hab status", status, s)
		}
		for p, data := range bundle {
			check("bundle "+p, data, s)
		}
		for p, data := range untar(t, archive) {
			check("archive "+p, data, s)
		}
	}
}

func screenOf(t *testing.T, e *habEnv, pane string) string {
	t.Helper()
	out, _ := exec.Command(bin, "screen", "-socket", e.sock, pane).Output()
	return string(out)
}

// filesUnder is every regular file under dir, by its path.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

// untar is every file of a .tar.gz, unpacked, by its name.
func untar(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("the archive is empty")
	}
	return out
}
