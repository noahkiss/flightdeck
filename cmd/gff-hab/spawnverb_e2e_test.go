package main

// End-to-end tests of the spawn verb (harness-adapters.md, Spawn) with the real gff-hab and
// holder: an agent's pane calls the spawn tool, a fake Codex on PATH plays the teammate, and
// its review comes back to the spawner over channels.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
)

// fakeTeammate stands in for Codex (or Claude Code) started by a spawn. It saves its argv,
// folder and session id, reports working over OSC 7501 and idle by the SessionStart hook,
// reads one paste of the length in seed-len, then runs one turn: UserPromptSubmit, its
// rollout (the transcript file with the final message), Stop. It then waits for the end file.
const fakeTeammate = `#!/bin/sh
D=@DIR@
app=$(basename "$0")
sid=019a0000-0000-7000-8000-$(printf '%012d' $$)
echo "$*" > "$D/argv-$sid"
pwd -P > "$D/cwd-$sid"
p() { printf '{"session_id":"%s","cwd":"/work/app","hook_event_name":"%s"%s}' "$sid" "$1" "$2"; }
hook() { p "$1" "$2" | "$GFF_HAB_BIN" hook "$app" "$1"; }
stty raw -echo
printf '\033]7501;state=working:app=%s\033\\' "$app"
hook SessionStart ',"source":"startup"'
head -c "$(cat "$D/seed-len")" > "$D/seed-$sid.tmp" && mv "$D/seed-$sid.tmp" "$D/seed-$sid"
hook UserPromptSubmit ',"prompt":"x"'
out="$CODEX_HOME/sessions/2026/01/01/rollout-2026-01-01T10-00-00-$sid.jsonl"
mkdir -p "$(dirname "$out")"
sed "s/@SID@/$sid/g" "$D/transcript" > "$out.tmp" && mv "$out.tmp" "$out"
hook Stop ''
while [ ! -e "$D/end" ]; do sleep 0.05; done
`

// fakeSpawner is the spawning agent's pane: it saves its GFF_HAB, reports idle, and keeps
// every message pasted into it.
const fakeSpawner = `
printf '%s' "$GFF_HAB" > "$DIR/addr.tmp" && mv "$DIR/addr.tmp" "$DIR/addr"
stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
exec cat > "$DIR/alice-in"
`

const (
	spawnSeed   = "You are a reviewer. Review the diff on this branch and list what to fix."
	spawnReview = "Review: LGTM, one nit: cart.go rounds the total twice."
)

// spawnFakes writes the fake teammate as codex and claude in a new folder, its transcript and
// the seed's delivered length for a spawner named alice on a hab with host id test-host, and
// returns the folder and the work folder the fakes write to.
func spawnFakes(t *testing.T) (fakes, work string) {
	t.Helper()
	fakes, work = shortDir(t), shortDir(t)
	for _, name := range []string{"codex", "claude"} {
		writeFile(t, filepath.Join(fakes, name), strings.ReplaceAll(fakeTeammate, "@DIR@", work))
		if err := os.Chmod(filepath.Join(fakes, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(work, "transcript"), codexTranscript(spawnReview))
	seed := "\x1b[200~" + channels.SeedEnvelope(anyID, "test-host/alice", spawnSeed) + "\x1b[201~\r"
	writeFile(t, filepath.Join(work, "seed-len"), strconv.Itoa(len(seed)))
	t.Cleanup(func() { writeFile(t, filepath.Join(work, "end"), "") })
	return fakes, work
}

// gitRepo makes a repository with a remote in a new folder under dir.
func gitRepo(t *testing.T, dir, name string) string {
	t.Helper()
	repo := filepath.Join(dir, name)
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://example.com/"+name+".git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, out)
	}
	// Pushes go to a bare repository beside it, so a group's lead can push its branch (groups.md
	// 7.1) with no network; the remote's URL, the project's identity, stays.
	bare := filepath.Join(dir, name+"-origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "url."+bare+".pushInsteadOf", "https://example.com/"+name+".git").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
	return repo
}

// startSpawner files alice in folder on e and returns her GFF_HAB.
func startSpawner(t *testing.T, e *habEnv, work, folder string) string {
	t.Helper()
	ss := createSession(t, e.sock, api.SessionCreateParams{
		Name: "alice", Argv: []string{"/bin/sh", "-c", fakeSpawner}, ProjectDir: folder, Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + work}})
	waitState(t, e.sock, ss.Pane, "alice idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	addr := filepath.Join(work, "addr")
	waitUntil(t, "alice's GFF_HAB", func() bool { b, err := os.ReadFile(addr); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addr)
	return string(b)
}

// teammateRun finds the fake teammate's session id by the argv file holding want.
func teammateRun(t *testing.T, work, want string) string {
	t.Helper()
	var sid string
	waitUntil(t, "a teammate started with "+want, func() bool {
		files, _ := filepath.Glob(filepath.Join(work, "argv-*"))
		for _, f := range files {
			if b, _ := os.ReadFile(f); strings.Contains(string(b), want) {
				sid = strings.TrimPrefix(filepath.Base(f), "argv-")
				return true
			}
		}
		return false
	})
	return sid
}

// TestSpawnEndToEnd (task acceptance): from an agent's pane, spawn "a gpt reviewer" with no
// folder: the alias picks Codex and its model from the hab's config, the session starts in the
// spawner's project folder with the seed in its start envelope, and its review comes back to
// the spawner as a channel message from it. The alias table changes with no restart; a
// detached spawn reports nothing. The teammate's native session id is known, so a plain
// terminal resumes it with codex resume, like any other session.
func TestSpawnEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	fakes, work := spawnFakes(t)
	e := newHolderEnv(t, 0)
	cfg := filepath.Join(work, "hab.toml")
	writeFile(t, cfg, "[aliases]\ngpt = { harness = \"codex\", model = \"gpt-5.5\" }\n")
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_CONFIG=" + cfg, "GFF_HAB_BIN=" + bin,
		"CODEX_HOME=" + filepath.Join(work, "codex"), "CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), noRaceExitSleep}
	e.serve()
	repo := gitRepo(t, e.work, "shop")
	addr := startSpawner(t, e, work, repo)
	c := hookCaller{addr}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "spawn")

	res, text, err := tl.run(ctx, c, toolArgs{"harness": "gpt", "seed": spawnSeed})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(spawnResult)
	cf := r.Confirmation
	if cf.State != "done" || cf.Start.Harness != "codex" || cf.Start.Model != "gpt-5.5" || cf.Start.Alias != "gpt" || !cf.Start.Teammate ||
		r.Address != "codex-"+cf.ID[:6] || !strings.Contains(text, "Its channel address: codex-"+cf.ID[:6]) {
		t.Fatalf("spawn: %+v %+v\n%s", r, cf.Start, text)
	}
	if real, _ := filepath.EvalSymlinks(repo); cf.Start.Folder != repo && cf.Start.Folder != real {
		t.Fatalf("folder %q, want the spawner's %q", cf.Start.Folder, repo)
	}
	sid := teammateRun(t, work, "--model gpt-5.5")
	argv, _ := os.ReadFile(filepath.Join(work, "argv-"+sid))
	cwd, _ := os.ReadFile(filepath.Join(work, "cwd-"+sid))
	if real, _ := filepath.EvalSymlinks(repo); !strings.Contains(string(argv), "--no-daemon") || strings.TrimSpace(string(cwd)) != real {
		t.Fatalf("teammate argv %q in %q", argv, cwd)
	}
	waitUntil(t, "the seed in the teammate", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "seed-"+sid))
		return strings.Contains(string(b), "[begin start message ") && strings.Contains(string(b), spawnSeed) &&
			strings.Contains(string(b), "gff-hab send 'test-host/alice'")
	})
	waitUntil(t, "the review back at alice", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "alice-in"))
		s := string(b)
		return strings.Contains(s, "[Flightdeck: agent message from 'codex-"+cf.ID[:6]+"'") &&
			strings.Contains(s, "(Flightdeck: my turn ended. My final message follows.)") && strings.Contains(s, spawnReview)
	})
	st := agentStates(t, e.sock)[cf.Result.Pane]
	if st.Session != sid || st.Harness != "codex" {
		t.Fatalf("the teammate's agent state: %+v, want native id %s", st, sid)
	}

	// The owner edits the table: the next spawn uses it, with no restart. Detached, it sends
	// nothing back.
	writeFile(t, cfg, "[aliases]\ngpt = { harness = \"codex\", model = \"gpt-6\" }\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfg, later, later); err != nil {
		t.Fatal(err)
	}
	res, text, err = tl.run(ctx, c, toolArgs{"harness": "gpt", "seed": spawnSeed, "mode": "detached", "name": "second"})
	if err != nil || res.(spawnResult).Confirmation.Start.Model != "gpt-6" || !strings.Contains(text, "It runs detached") {
		t.Fatalf("after the edit: %v %+v\n%s", err, res, text)
	}
	sid2 := teammateRun(t, work, "--model gpt-6")
	waitUntil(t, "the second teammate's turn", func() bool {
		st := agentStates(t, e.sock)[res.(spawnResult).Confirmation.Result.Pane]
		return st.Session == sid2 && st.State == api.AgentStatusDone
	})
	time.Sleep(time.Second) // a report would come within this
	if b, _ := os.ReadFile(filepath.Join(work, "alice-in")); strings.Count(string(b), "[begin agent message ") != 1 {
		t.Fatalf("alice got %d messages:\n%s", strings.Count(string(b), "[begin agent message "), b)
	}
	// An unknown word is refused with the harnesses and the aliases.
	if _, _, err := tl.run(ctx, c, toolArgs{"harness": "sol", "seed": "x"}); errKind(err) != api.ErrorKindInvalid ||
		!strings.Contains(err.Error(), "aliases from this hab's [aliases] table: gpt") {
		t.Fatalf("unknown alias: %v", err)
	}
}

// TestSpawnOnPeer: a spawn on another host goes over the link the spawner's hab dialed; the
// teammate runs there, and its review comes back over the mesh to the spawner, addressed by the
// link's own hello.
func TestSpawnOnPeer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	fakes, work := spawnFakes(t)
	cfg := filepath.Join(work, "hab.toml")
	writeFile(t, cfg, "[aliases]\nreviewer = { harness = \"codex\", model = \"gpt-5.5\" }\n")
	m := newMeshPairEnv(t, fakes, "GFF_HAB_CONFIG="+cfg, "GFF_HAB_BIN="+bin,
		"CODEX_HOME="+filepath.Join(work, "codex"), "CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	addr := startSpawner(t, m.a, work, repo)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "spawn")
	res, text, err := tl.run(ctx, hookCaller{addr}, toolArgs{"harness": "reviewer", "seed": spawnSeed, "host": "bee", "folder": repo})
	if err != nil {
		t.Fatal(err)
	}
	cf := res.(spawnResult).Confirmation
	name := "codex-" + cf.ID[:6]
	if cf.State != "done" || cf.Result.Host != "bee" || res.(spawnResult).Address != "bee/"+name || !strings.Contains(text, "Its channel address: bee/"+name) {
		t.Fatalf("spawn: %+v\n%s", cf, text)
	}
	sid := teammateRun(t, work, "--model gpt-5.5")
	if st := agentStates(t, m.b.sock)[cf.Result.Pane]; st.Pane == "" {
		waitUntil(t, "the teammate on b", func() bool { return agentStates(t, m.b.sock)[cf.Result.Pane].Session == sid })
	}
	waitUntil(t, "the review back at alice on a", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "alice-in"))
		s := string(b)
		return strings.Contains(s, "[Flightdeck: agent message from 'bee/"+name+"'") && strings.Contains(s, spawnReview)
	})
}
