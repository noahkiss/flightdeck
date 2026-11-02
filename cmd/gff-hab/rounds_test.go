package main

// End-to-end tests of rounds (rounds.md 3, 5.1, 7, 8.4, 13) with the real gff-hab and holder
// binaries: command runs in visible panes, restarts mid-run, every way a run ends, the events,
// export and apply, and the pane door.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/holderclient"
)

// newRoundEnv is a holder and a gff-hab with rounds on a fast tick and no wake settle, and a
// git repository with a remote for the rounds' project.
func newRoundEnv(t *testing.T) (*habEnv, string) {
	t.Helper()
	return newRoundEnvWith(t)
}

// newRoundEnvWith is newRoundEnv with env added to gff-hab's environment.
func newRoundEnvWith(t *testing.T, env ...string) (*habEnv, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := newHolderEnv(t, 0)
	cfg := filepath.Join(e.work, "hab.toml")
	writeFile(t, cfg, "[rounds]\nwake_settle = \"0s\"\nmax_running = 4\n")
	e.env = append([]string{"GFF_HAB_CONFIG=" + cfg, "GFF_TEST_ROUND_TICK=200ms"}, env...)
	e.serve()
	repo := filepath.Join(e.work, "app")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://example.com/team/app.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, out)
	}
	return e, repo
}

// roundCLI runs gff-hab round on e's socket ($GFF_HAB_SOCKET, so the flags keep their order)
// with stdin, and returns its output and error.
func roundCLI(e *habEnv, stdin string, args ...string) (string, error) {
	cmd := exec.Command(bin, append([]string{"round"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GFF_HAB_SOCKET="+e.sock, "GFF_HAB=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustRound(t *testing.T, e *habEnv, args ...string) string {
	t.Helper()
	out, err := roundCLI(e, "", args...)
	if err != nil {
		t.Fatalf("gff-hab round %v: %v\n%s", args, err, out)
	}
	return out
}

func getRound(t *testing.T, c *control.Client, name string) api.Round {
	t.Helper()
	var r api.RoundResult
	call(t, c, "round.get", api.RoundRef{Name: name}, &r)
	return r.Round
}

func roundRuns(t *testing.T, c *control.Client, name string) []api.RoundRun {
	t.Helper()
	var r api.RoundRuns
	call(t, c, "round.runs", api.RoundRunsParams{RoundRef: api.RoundRef{Name: name}, Limit: 200}, &r)
	return r.Runs
}

// waitRun waits until the round's newest run reaches state.
func waitRun(t *testing.T, e *habEnv, c *control.Client, name string, state api.RoundRunState) api.RoundRun {
	t.Helper()
	var last []api.RoundRun
	deadline := time.Now().Add(scaleTimeout(30 * time.Second))
	for time.Now().Before(deadline) {
		last = roundRuns(t, c, name)
		if len(last) > 0 && last[0].State == state {
			return last[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("round %s: no run %s: %+v", name, state, last)
	return api.RoundRun{}
}

func structureOf(t *testing.T, c *control.Client) api.Structure {
	t.Helper()
	var st api.StructureResult
	call(t, c, "structure.get", api.StructureGetParams{}, &st)
	return st.Structure
}

func runSessionOf(st api.Structure, id string) (api.Session, bool) {
	for _, s := range st.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return api.Session{}, false
}

func paneIn(st api.Structure, id string) bool {
	for _, p := range st.Panes {
		if p.ID == id {
			return true
		}
	}
	return false
}

// TestRoundEveryMinute: a -every 1m command round runs at its slot in a visible pane with the
// GFF_ROUND_* variables, ends succeeded, its pane closes and its session stays exited; the
// events carry rising revisions and one run_updated per state change.
func TestRoundEveryMinute(t *testing.T) {
	t.Parallel()
	e, repo := newRoundEnv(t)
	c := e.control()
	ev := e.control()
	ev.KeepAlive(control.DefaultHeartbeat) // the slot may be most of a minute away
	call(t, ev, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil)
	envFile := filepath.Join(e.work, "env")
	script := "env | grep '^GFF_ROUND' | LC_ALL=C sort > " + envFile + ".tmp; mv " + envFile + ".tmp " + envFile + "; sleep 1"
	out := mustRound(t, e, "add", "minutely", "-every", "1m", "-project", repo, "-command", "--", "/bin/sh", "-c", script)
	if !strings.Contains(out, "minutely") {
		t.Fatalf("add: %s", out)
	}
	r := getRound(t, c, "minutely")
	if r.Trigger.Cron != "*/1 * * * *" && r.Trigger.Cron != "* * * * *" {
		t.Errorf("every 1m compiled to %q", r.Trigger.Cron)
	}
	if r.CreatedBy != "" || r.ProjectRemote == "" || r.NextSlot == "" {
		t.Errorf("round: %+v", r)
	}

	// The slot comes within a minute.
	var run api.RoundRun
	deadline := time.Now().Add(scaleTimeout(90 * time.Second))
	for {
		if runs := roundRuns(t, c, "minutely"); len(runs) > 0 && runs[0].State == api.RoundRunStateRunning && runs[0].Session != "" {
			run = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no run started at the slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if run.Slot == "" || run.Trigger != api.RoundTriggerKindCron || run.Number != 1 {
		t.Fatalf("run: %+v", run)
	}
	st := structureOf(t, c)
	ss, ok := runSessionOf(st, run.Session)
	if !ok || ss.Round == nil || ss.Round.Round != r.ID || ss.Round.Run != run.ID || ss.Name != "minutely" || !paneIn(st, ss.Pane) {
		t.Fatalf("run session: %+v", ss)
	}
	done := waitRun(t, e, c, "minutely", api.RoundRunStateSucceeded)
	if done.ID != run.ID || done.EndedAt == "" {
		t.Fatalf("ended: %+v", done)
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "GFF_ROUND=" + r.ID + "\nGFF_ROUND_RUN=" + run.ID + "\nGFF_ROUND_SLOT=" + run.Slot + "\n"
	if string(b) != want {
		t.Errorf("run environment:\n%s\nwant:\n%s", b, want)
	}
	// on_finish close: the pane goes, the session record stays, exited.
	waitUntil(t, "the run's pane closed", func() bool {
		st := structureOf(t, c)
		ss, ok := runSessionOf(st, run.Session)
		return ok && ss.State == api.SessionStateExited && !paneIn(st, ss.Pane)
	})
	if r := getRound(t, c, "minutely"); r.LastOK == "" || r.LastRun == nil || r.LastRun.State != api.RoundRunStateSucceeded {
		t.Errorf("after the run: %+v", r)
	}

	// Events: round.updated with rising revisions; one run_updated per state of the run.
	var revs []uint64
	states := map[api.RoundRunState]int{}
	timeout := time.After(scaleTimeout(5 * time.Second))
	for states[api.RoundRunStateSucceeded] == 0 {
		select {
		case n := <-ev.Notifications():
			if n.Method != "event" {
				continue
			}
			ev, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			switch ev.Type {
			case control.EventRoundUpdated:
				if ev.Revision == nil || ev.ID != r.ID {
					t.Fatalf("round.updated: %+v", ev)
				}
				revs = append(revs, *ev.Revision)
			case control.EventRoundRunUpdated:
				var rr api.RoundRun
				if err := json.Unmarshal(ev.Data, &rr); err != nil || ev.Revision != nil {
					t.Fatalf("round.run_updated: %+v %v", ev, err)
				}
				if rr.ID == run.ID {
					states[rr.State]++
				}
			}
		case <-timeout:
			t.Fatalf("events: revisions %v, run states %v", revs, states)
		}
	}
	if len(revs) < 2 {
		t.Fatalf("round.updated revisions %v", revs)
	}
	for i := 1; i < len(revs); i++ {
		if revs[i] != revs[i-1]+1 {
			t.Fatalf("revisions do not rise by one: %v", revs)
		}
	}
	if states[api.RoundRunStateQueued] != 1 || states[api.RoundRunStateRunning] < 1 || states[api.RoundRunStateSucceeded] != 1 {
		t.Errorf("run states %v", states)
	}
	// The snapshot carries the rounds, at the revision the last event named.
	var snap api.StructureResult
	call(t, e.control(), "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &snap)
	if len(snap.Rounds) != 1 || snap.Rounds[0].Revision < revs[len(revs)-1] {
		t.Errorf("snapshot rounds: %+v (last revision %d)", snap.Rounds, revs[len(revs)-1])
	}
}

// TestRoundSurvivesRestarts: one run lives through three gff-hab restarts (planned and killed)
// and still ends succeeded, with one run record and one session.
func TestRoundSurvivesRestarts(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	gate := filepath.Join(e.work, "go")
	mustRound(t, e, "add", "steady", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c",
		"while [ ! -e "+gate+" ]; do sleep 0.05; done; exit 0")
	mustRound(t, e, "run", "steady")
	run := waitRun(t, e, c, "steady", api.RoundRunStateRunning)
	waitUntil(t, "the run's session", func() bool { return roundRuns(t, c, "steady")[0].Session != "" })
	run = roundRuns(t, c, "steady")[0]
	for i, kill := range []bool{false, true, false} {
		e.restart(kill)
		c = e.control()
		if runs := roundRuns(t, c, "steady"); len(runs) != 1 || runs[0].State != api.RoundRunStateRunning {
			t.Fatalf("after restart %d: %+v", i+1, runs)
		}
	}
	writeFile(t, gate, "")
	done := waitRun(t, e, c, "steady", api.RoundRunStateSucceeded)
	runs := roundRuns(t, c, "steady")
	if len(runs) != 1 || done.ID != run.ID || done.Session != run.Session {
		t.Fatalf("runs after the restarts: %+v", runs)
	}
	// A run whose program exits while gff-hab is down ends by its exit at the next start.
	mustRound(t, e, "run", "steady")
	waitUntil(t, "the second run's session", func() bool {
		r := roundRuns(t, c, "steady")
		return len(r) == 2 && r[0].Session != ""
	})
	os.Remove(gate)
	e.stopHab(false)
	writeFile(t, gate, "")
	time.Sleep(300 * time.Millisecond)
	e.serve()
	c = e.control()
	waitRun(t, e, c, "steady", api.RoundRunStateSucceeded)
}

// TestRoundEnds: exit 1 fails and keeps the pane; a run past its timeout is hung up and ends
// timed_out; typing into a run pane, or closing it, ends taken_over and keeps the session;
// round.run with a live run answers conflict.
func TestRoundEnds(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "fails", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", "exit 1")
	mustRound(t, e, "add", "slow", "-manual", "-timeout", "1s", "-project", repo, "-command", "--", "/bin/sh", "-c", "sleep 60")
	mustRound(t, e, "add", "typed", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", "sleep 60")
	mustRound(t, e, "add", "closed", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", "sleep 60")

	mustRound(t, e, "run", "fails")
	f := waitRun(t, e, c, "fails", api.RoundRunStateFailed)
	if f.Reason != "exit status 1" {
		t.Errorf("failed reason %q", f.Reason)
	}
	st := structureOf(t, c)
	if ss, ok := runSessionOf(st, f.Session); !ok || !paneIn(st, ss.Pane) || ss.State != api.SessionStateExited {
		t.Errorf("a failed run's pane must stay: %+v", ss)
	}

	mustRound(t, e, "run", "slow")
	to := waitRun(t, e, c, "slow", api.RoundRunStateTimedOut)
	waitUntil(t, "the timed-out run hung up", func() bool {
		ss, ok := runSessionOf(structureOf(t, c), to.Session)
		return ok && ss.State == api.SessionStateExited
	})

	mustRound(t, e, "run", "typed")
	waitUntil(t, "the typed run's session", func() bool { return roundRuns(t, c, "typed")[0].Session != "" })
	typed := roundRuns(t, c, "typed")[0]
	// A second run while one is live is a conflict.
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var ce *control.Error
	if err := c.Call(ctx, "round.run", api.RoundRef{Name: "typed"}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
		t.Fatalf("round.run with a live run: %v", err)
	}
	if out, err := roundCLI(e, "", "run", "typed"); err == nil || !strings.Contains(out, "live run") {
		t.Errorf("round run with a live run: %v %s", err, out)
	}
	ss, _ := runSessionOf(structureOf(t, c), typed.Session)
	call(t, c, "pane.input", api.InputParams{Pane: ss.Pane, Text: "x"}, nil)
	tk := waitRun(t, e, c, "typed", api.RoundRunStateTakenOver)
	if tk.Reason != reasonTyped {
		t.Errorf("taken over reason %q", tk.Reason)
	}
	time.Sleep(300 * time.Millisecond)
	st = structureOf(t, c)
	if ss, ok := runSessionOf(st, typed.Session); !ok || ss.State != api.SessionStateRunning || !paneIn(st, ss.Pane) {
		t.Errorf("a taken-over session must stay: %+v", ss)
	}

	mustRound(t, e, "run", "closed")
	waitUntil(t, "the closed run's session", func() bool { return roundRuns(t, c, "closed")[0].Session != "" })
	cl := roundRuns(t, c, "closed")[0]
	ss, _ = runSessionOf(structureOf(t, c), cl.Session)
	call(t, c, "pane.close", api.PaneRef{Pane: ss.Pane}, nil)
	if got := waitRun(t, e, c, "closed", api.RoundRunStateTakenOver); got.Reason != reasonClosed {
		t.Errorf("closed reason %q", got.Reason)
	}
	call(t, c, "pane.close", api.PaneRef{Pane: func() string { s, _ := runSessionOf(structureOf(t, c), typed.Session); return s.Pane }()}, nil)
}

// TestRoundHostRestarted: a run live when its holder ends (a host reboot) fails "host
// restarted" at the next gff-hab start.
func TestRoundHostRestarted(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "reboot", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", "sleep 60")
	mustRound(t, e, "run", "reboot")
	waitUntil(t, "the run's session", func() bool { return roundRuns(t, c, "reboot")[0].Session != "" })
	e.stopHab(false)
	e.holder.Process.Signal(syscall.SIGTERM)
	select {
	case <-e.holderDone:
	case <-time.After(scaleTimeout(10 * time.Second)):
		t.Fatal("the holder did not stop")
	}
	startHolder(t, e.hdir, "two")
	e.serve()
	c = e.control()
	if got := waitRun(t, e, c, "reboot", api.RoundRunStateFailed); got.Reason != reasonHostRestarted {
		t.Fatalf("reason %q", got.Reason)
	}
}

// TestRoundExportApply: export then apply of the same file changes nothing; an edited file
// shows its diff, changes nothing on no, and applies on yes.
func TestRoundExportApply(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "nightly", "-cron", "0 3 * * *", "-tz", "UTC", "-project", repo, "-command", "--", "make", "check")
	mustRound(t, e, "add", "hourly", "-every", "1h", "-project", repo, "-timeout", "5m", "-command", "--", "true")
	mustRound(t, e, "pause", "hourly")
	exported := mustRound(t, e, "export")
	if !strings.Contains(exported, `project_remote = "example.com/team/app"`) || strings.Contains(exported, repo) {
		t.Fatalf("export must name the project by its remote:\n%s", exported)
	}
	file := filepath.Join(e.work, "rounds.toml")
	writeFile(t, file, exported)
	if out := mustRound(t, e, "apply", file); strings.TrimSpace(out) != "no changes" {
		t.Fatalf("apply of the exported file: %s", out)
	}

	edited := strings.Replace(exported, `timeout = "5m0s"`, `timeout = "10m0s"`, 1)
	edited = strings.Replace(edited, "paused = true\n", "", 1)
	// nightly goes; a new round comes, written by hand with a preset and no defaults.
	i := strings.Index(edited, "[[rounds]]")
	j := strings.Index(edited[i+1:], "[[rounds]]") + i + 1
	if strings.Contains(edited[i:j], `name = "nightly"`) {
		edited = edited[:i] + edited[j:]
	} else {
		edited = edited[:j]
	}
	edited += "\n[[rounds]]\nname = \"weekly\"\nmode = \"fresh\"\nevery = \"1d\"\nproject_remote = \"https://example.com/team/app.git\"\ncommand = [\"true\"]\n"
	writeFile(t, file, edited)
	out, err := roundCLI(e, "n\n", "apply", file)
	if err == nil {
		t.Fatalf("apply answered no, but succeeded: %s", out)
	}
	for _, w := range []string{"+ create weekly", "- delete nightly", "~ update hourly", `timeout: "5m0s" -> "10m0s"`, "~ unpause hourly"} {
		if !strings.Contains(out, w) {
			t.Errorf("diff lacks %q:\n%s", w, out)
		}
	}
	var l api.RoundList
	call(t, c, "round.list", api.RoundListParams{}, &l)
	if len(l.Rounds) != 2 || getRound(t, c, "hourly").TimeoutMS != (5*time.Minute).Milliseconds() {
		t.Fatalf("no changed something: %+v", l.Rounds)
	}
	out = mustRound(t, e, "apply", "-yes", file)
	if !strings.Contains(out, "applied 4 changes") {
		t.Fatalf("apply -yes: %s", out)
	}
	call(t, c, "round.list", api.RoundListParams{}, &l)
	names := []string{}
	for _, r := range l.Rounds {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "hourly,weekly" {
		t.Fatalf("rounds after apply: %v", names)
	}
	if h := getRound(t, c, "hourly"); h.Paused || h.TimeoutMS != (10*time.Minute).Milliseconds() {
		t.Errorf("hourly after apply: %+v", h)
	}
	if out := mustRound(t, e, "apply", file); strings.TrimSpace(out) != "no changes" {
		t.Errorf("a second apply: %s", out)
	}
	// rm asks first.
	if out, err := roundCLI(e, "n\n", "rm", "weekly"); err == nil {
		t.Errorf("rm answered no: %s", out)
	}
	mustRound(t, e, "rm", "-yes", "weekly")
	if out, err := roundCLI(e, "", "show", "weekly"); err == nil {
		t.Errorf("show after rm: %s", out)
	}
}

// TestRoundPaneDoor: `gff-hab round add -target self` from inside a pane names that pane's
// session, and the round's created_by is the session.
func TestRoundPaneDoor(t *testing.T) {
	e, _ := newRoundEnv(t)
	c := e.control()
	outFile := filepath.Join(e.work, "add-out")
	script := `"$HAB" round add selfie -mode resume -manual -target self 'check the deploy' > "$OUT.tmp" 2>&1; mv "$OUT.tmp" "$OUT"; sleep 60`
	made := createSession(t, e.sock, api.SessionCreateParams{Name: "agent", Argv: []string{"/bin/sh", "-c", script}, Cwd: "/",
		Env: []string{"HAB=" + bin, "OUT=" + outFile, "PATH=/usr/bin:/bin"}})
	waitUntil(t, "round add from the pane", func() bool { _, err := os.Stat(outFile); return err == nil })
	b, _ := os.ReadFile(outFile)
	r := getRound(t, c, "selfie")
	if r.Target != made.Session || r.CreatedBy != made.Session || r.Mode != api.RoundModeResume {
		t.Fatalf("round from the pane: %+v\n%s", r, b)
	}
	// From the control socket, self names no session.
	if out, err := roundCLI(e, "", "add", "other", "-mode", "resume", "-manual", "-target", "self", "hi"); err == nil {
		t.Errorf("target self outside a pane: %s", out)
	}
	call(t, c, "pane.close", api.PaneRef{Pane: made.Pane}, nil)
}

// gatedRound adds a manual command round whose program waits for the file gate, then exits 0,
// and starts one run; it returns the run once its session is known.
func gatedRound(t *testing.T, e *habEnv, c *control.Client, repo, name, gate string, extra ...string) api.RoundRun {
	t.Helper()
	args := append([]string{"add", name, "-manual", "-project", repo}, extra...)
	args = append(args, "-command", "--", "/bin/sh", "-c", "while [ ! -e "+gate+" ]; do sleep 0.05; done; exit 0")
	mustRound(t, e, args...)
	mustRound(t, e, "run", name)
	waitUntil(t, name+"'s session", func() bool { r := roundRuns(t, c, name); return len(r) > 0 && r[0].Session != "" })
	return roundRuns(t, c, name)[0]
}

// TestRoundHolderDies: the holder dies while gff-hab runs; once a new holder is up and the link
// has reached every holder, the run fails "host restarted" in seconds, not at its timeout.
func TestRoundHolderDies(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	run := gatedRound(t, e, c, repo, "crash", filepath.Join(e.work, "never"), "-timeout", "10m")
	e.holder.Process.Signal(syscall.SIGKILL)
	<-e.holderDone
	startHolder(t, e.hdir, "two")
	got := waitRun(t, e, c, "crash", api.RoundRunStateFailed)
	if got.ID != run.ID || got.Reason != reasonHostRestarted {
		t.Fatalf("after the holder died: %+v", got)
	}
}

// TestRoundHolderConnectionDrops: the link's connection to a living holder drops (another
// client takes the holder) and comes back; the run goes on and ends by its exit.
func TestRoundHolderConnectionDrops(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	gate := filepath.Join(e.work, "go")
	run := gatedRound(t, e, c, repo, "blip", gate)
	hc := holderclient.New(holderclient.Config{Dir: e.hdir, Build: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if err := hc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run's session lost", func() bool {
		ss, _ := runSessionOf(structureOf(t, c), run.Session)
		return ss.State == api.SessionStateExited
	})
	hc.Close()
	waitUntil(t, "the run's session back", func() bool {
		ss, _ := runSessionOf(structureOf(t, c), run.Session)
		return ss.State == api.SessionStateRunning
	})
	if r := roundRuns(t, c, "blip")[0]; r.State != api.RoundRunStateRunning {
		t.Fatalf("a dropped connection ended the run: %+v", r)
	}
	writeFile(t, gate, "")
	if got := waitRun(t, e, c, "blip", api.RoundRunStateSucceeded); got.ID != run.ID {
		t.Fatalf("ended: %+v", got)
	}
}

// TestRoundLostStart: gff-hab killed after the spawn and before the run saved its session; the
// next gff-hab finds the session by its Session.round, watches it, refuses a second run, and
// ends the run by its exit.
func TestRoundLostStart(t *testing.T) {
	e, repo := newRoundEnv(t)
	gate := filepath.Join(e.work, "go")
	mark := filepath.Join(e.work, "started")
	mustRound(t, e, "add", "orphan", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c",
		"touch "+mark+"; while [ ! -e "+gate+" ]; do sleep 0.05; done; exit 0")
	e.stopHab(false)
	base := e.env
	e.env = append(append([]string{}, base...), "GFF_TEST_SPAWN_DELAY=4s")
	e.serve()
	done := make(chan struct{})
	go func() { roundCLI(e, "", "run", "orphan"); close(done) }()
	waitUntil(t, "the program running", func() bool { _, err := os.Stat(mark); return err == nil })
	e.stopHab(true)
	<-done
	e.env = base
	e.serve()
	c := e.control()
	r := waitRun(t, e, c, "orphan", api.RoundRunStateRunning)
	if r.Session == "" {
		t.Fatalf("the run did not get its session back: %+v", r)
	}
	if out, err := roundCLI(e, "", "run", "orphan"); err == nil {
		t.Fatalf("a second run started beside the found one: %s", out)
	}
	writeFile(t, gate, "")
	if got := waitRun(t, e, c, "orphan", api.RoundRunStateSucceeded); got.ID != r.ID {
		t.Fatalf("ended: %+v", got)
	}
	live := 0
	for _, ss := range structureOf(t, c).Sessions {
		if ss.Round != nil {
			live++
		}
	}
	if live != 1 || len(roundRuns(t, c, "orphan")) != 1 {
		t.Fatalf("run sessions %d, runs %+v", live, roundRuns(t, c, "orphan"))
	}
}

// fakeAgentRun is a command that reports an idle agent through OSC 7501 and then reads input,
// so session.send can deliver to it.
const fakeAgentRun = `stty raw -echo; printf '\033]7501;state=idle:app=claude\033\\'; while :; do sleep 0.05; done`

// TestRoundSendAndKeep: session.send delivered to a run session ends the run taken_over; a
// send the adapter refuses takes nothing over; on_finish keep leaves a succeeded run's pane;
// the second run sees GFF_ROUND_LAST_OK, the first run's start.
func TestRoundSendAndKeep(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "agent", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", fakeAgentRun)
	mustRound(t, e, "add", "plain", "-manual", "-project", repo, "-command", "--", "/bin/sh", "-c", "sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()

	// A refused send: the pane has no agent.
	mustRound(t, e, "run", "plain")
	waitUntil(t, "plain's session", func() bool { return roundRuns(t, c, "plain")[0].Session != "" })
	plain := roundRuns(t, c, "plain")[0]
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: plain.Session, Text: "hi"}, nil); err == nil {
		t.Fatal("a send to a pane with no agent was taken")
	}
	time.Sleep(200 * time.Millisecond)
	if r := roundRuns(t, c, "plain")[0]; r.State != api.RoundRunStateRunning {
		t.Fatalf("a refused send took the run over: %+v", r)
	}

	mustRound(t, e, "run", "agent")
	waitUntil(t, "agent's session", func() bool { return roundRuns(t, c, "agent")[0].Session != "" })
	ag := roundRuns(t, c, "agent")[0]
	ss, _ := runSessionOf(structureOf(t, c), ag.Session)
	waitState(t, e.sock, ss.Pane, "the run's agent idle", func(s api.AgentState) bool { return s.State == api.AgentStatusIdle })
	if err := c.Call(ctx, "session.send", api.SessionSendParams{Session: ag.Session, Text: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := waitRun(t, e, c, "agent", api.RoundRunStateTakenOver); got.Reason != reasonSent {
		t.Errorf("reason %q", got.Reason)
	}

	envFile := filepath.Join(e.work, "env")
	script := "env | grep '^GFF_ROUND_LAST_OK' > " + envFile + ".tmp; mv " + envFile + ".tmp " + envFile
	mustRound(t, e, "add", "kept", "-manual", "-on-finish", "keep", "-project", repo, "-command", "--", "/bin/sh", "-c", script)
	mustRound(t, e, "run", "kept")
	first := waitRun(t, e, c, "kept", api.RoundRunStateSucceeded)
	b, _ := os.ReadFile(envFile)
	if len(b) != 0 {
		t.Errorf("the first run has GFF_ROUND_LAST_OK: %s", b)
	}
	time.Sleep(300 * time.Millisecond)
	st := structureOf(t, c)
	if ss, ok := runSessionOf(st, first.Session); !ok || !paneIn(st, ss.Pane) {
		t.Errorf("on_finish keep closed the pane: %+v", ss)
	}
	os.Remove(envFile)
	mustRound(t, e, "run", "kept")
	waitUntil(t, "the second run", func() bool {
		r := roundRuns(t, c, "kept")
		return len(r) == 2 && r[0].State == api.RoundRunStateSucceeded
	})
	waitUntil(t, "the second run's env", func() bool { _, err := os.Stat(envFile); return err == nil })
	b, _ = os.ReadFile(envFile)
	if want := "GFF_ROUND_LAST_OK=" + first.StartedAt + "\n"; string(b) != want {
		t.Errorf("second run: %q, want %q", b, want)
	}
}

// TestRoundCLIRefusals: add with -via and no -project names the problem; edit -paused points
// at pause and unpause.
func TestRoundCLIRefusals(t *testing.T) {
	e, repo := newRoundEnv(t)
	out, err := roundCLI(e, "", "add", "far", "-via", bin+" connect -socket "+e.sock, "-manual", "-command", "--", "true")
	if err == nil || !strings.Contains(out, "needs -project") {
		t.Errorf("add -via without -project: %v %s", err, out)
	}
	if out, err := roundCLI(e, "", "add", "far", "-via", bin+" connect -socket "+e.sock, "-manual", "-project", "app", "-command", "--", "true"); err == nil || !strings.Contains(out, "absolute") {
		t.Errorf("add -via with a relative -project: %v %s", err, out)
	}
	mustRound(t, e, "add", "far", "-via", bin+" connect -socket "+e.sock, "-manual", "-project", repo, "-command", "--", "true")
	out, err = roundCLI(e, "", "edit", "far", "-paused")
	if err == nil || !strings.Contains(out, "round pause") {
		t.Errorf("edit -paused: %v %s", err, out)
	}
}

// TestRoundApplyReplans: a revision raised between the plan and the answer still applies, with
// the new revisions; a plan that changed is shown again and asked again.
func TestRoundApplyReplans(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "steady", "-manual", "-project", repo, "-timeout", "5m", "-command", "--", "true")
	file := filepath.Join(e.work, "rounds.toml")
	writeFile(t, file, strings.Replace(mustRound(t, e, "export"), `timeout = "5m0s"`, `timeout = "7m0s"`, 1))

	apply := func(between func(), answers ...string) string {
		t.Helper()
		cmd := exec.Command(bin, "round", "apply", file)
		cmd.Env = append(os.Environ(), "GFF_HAB_SOCKET="+e.sock, "GFF_HAB=")
		in, _ := cmd.StdinPipe()
		outR, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		b := make([]byte, 4096)
		for _, a := range answers {
			for !strings.HasSuffix(buf.String(), "[y/N] ") {
				n, err := outR.Read(b)
				buf.Write(b[:n])
				if err != nil {
					t.Fatalf("apply ended early: %v\n%s", err, buf.String())
				}
			}
			between()
			between = func() {}
			in.Write([]byte(a + "\n"))
			buf.WriteString("<" + a + ">")
		}
		in.Close()
		rest, _ := io.ReadAll(outR)
		buf.Write(rest)
		cmd.Wait()
		return buf.String()
	}

	// Raise the round's revision while the prompt waits: the same plan applies.
	out := apply(func() {
		call(t, c, "round.pause", api.RoundRef{Name: "steady"}, nil)
		call(t, c, "round.unpause", api.RoundRef{Name: "steady"}, nil)
	}, "y")
	if !strings.Contains(out, "applied 1 changes") || getRound(t, c, "steady").TimeoutMS != (7*time.Minute).Milliseconds() {
		t.Fatalf("apply after a revision bump:\n%s", out)
	}

	// Change the plan while the prompt waits: the new plan is shown and asked again.
	writeFile(t, file, strings.Replace(mustRound(t, e, "export"), `timeout = "7m0s"`, `timeout = "9m0s"`, 1))
	out = apply(func() {
		r := getRound(t, c, "steady")
		up := r.RoundSpec
		up.ProjectRemote, up.TimeoutMS = "", (8 * time.Minute).Milliseconds()
		call(t, c, "round.update", api.RoundUpdateParams{ID: r.ID, Round: up}, nil)
	}, "y", "n")
	if !strings.Contains(out, "changed while you answered") || !strings.Contains(out, `timeout: "8m0s" -> "9m0s"`) || !strings.Contains(out, "nothing applied") {
		t.Fatalf("apply after the plan changed:\n%s", out)
	}
	if got := getRound(t, c, "steady").TimeoutMS; got != (8 * time.Minute).Milliseconds() {
		t.Fatalf("a refused new plan changed the round: %d", got)
	}
}
