package main

// Watch rounds end to end (rounds.md 4.4, 4.7, 14.1 stage 11) with the real gff-hab and holder
// and a fake watcher program: the watcher session and its pane, event lines into one run, the
// drop line, a gff-hab restart that leaves the watcher running and delivers what it printed
// meanwhile, the cursor at the next start, an edit that restarts it and one that does not, the
// backoff and the exit pause, pause and delete, Resume refused, and a secret set that reaches
// the watcher and nothing else.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeWatcher is a watcher program: it notes each start (its pid, the cursor, the round and two
// secret variables) in <dir>/log, writes one line on stderr, then prints whatever the test puts
// in <dir>/say, and exits with status 3 while <dir>/die exists.
const fakeWatcher = `dir=$1
echo "start $$ $(date +%s) cursor=$GFF_WATCH_CURSOR round=$GFF_ROUND secret=$WATCH_SECRET token=$WATCH_TOKEN" >> "$dir/log"
echo "watcher stderr line" >&2
while :; do
  if [ -f "$dir/die" ]; then exit 3; fi
  if [ -f "$dir/say" ]; then cat "$dir/say"; rm -f "$dir/say"; fi
  sleep 0.1
done
`

// syncBuffer is a log both gff-hab's stderr copier and the test read.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Stderr.Write(p)
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type watchStart struct {
	pid, cursor, round, secret, token string
	at                                int64
}

func watchStarts(t *testing.T, dir string) []watchStart {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	var out []watchStart
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		if len(f) < 7 || f[0] != "start" {
			continue
		}
		at, _ := strconv.ParseInt(f[2], 10, 64)
		val := func(s, k string) string { return strings.TrimPrefix(s, k+"=") }
		out = append(out, watchStart{pid: f[1], at: at, cursor: val(f[3], "cursor"), round: val(f[4], "round"),
			secret: val(f[5], "secret"), token: val(f[6], "token")})
	}
	return out
}

// say has the fake watcher print lines.
func say(t *testing.T, dir string, lines ...string) {
	t.Helper()
	waitUntil(t, "the watcher to print what it was given", func() bool {
		_, err := os.Stat(filepath.Join(dir, "say"))
		return errors.Is(err, os.ErrNotExist)
	})
	writeFile(t, filepath.Join(dir, "say.tmp"), strings.Join(lines, "\n")+"\n")
	if err := os.Rename(filepath.Join(dir, "say.tmp"), filepath.Join(dir, "say")); err != nil {
		t.Fatal(err)
	}
}

func watcherOf(t *testing.T, c *control.Client, name string) api.RoundWatcher {
	t.Helper()
	r := getRound(t, c, name)
	if r.Watcher == nil {
		return api.RoundWatcher{}
	}
	return *r.Watcher
}

func waitWatcher(t *testing.T, c *control.Client, name string, state api.RoundWatcherState) api.RoundWatcher {
	t.Helper()
	var w api.RoundWatcher
	waitUntil(t, "watcher "+string(state), func() bool {
		w = watcherOf(t, c, name)
		return w.State == state
	})
	return w
}

func TestWatchRound(t *testing.T) {
	logBuf := &syncBuffer{}
	e, repo := newRoundEnvWith(t, "GFF_TEST_ROUND_MIN_BATCH=500ms")
	e.stderr = logBuf
	wdir := filepath.Join(e.work, "watcher")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(e.work, "fakewatch.sh")
	writeFile(t, script, fakeWatcher)
	// Invented secret values: the test checks that they reach the watcher and nothing else.
	const secret, token = "vars-value-6f1d2c", "command-value-93ab7e"
	tokenFile := filepath.Join(e.work, "token.env")
	writeFile(t, tokenFile, "# a comment line\nWATCH_TOKEN="+token+"\n")
	writeFile(t, filepath.Join(e.work, "hab.toml"), "[rounds]\nwake_settle = \"0s\"\nmax_running = 4\nstate_save_every = \"1s\"\n"+
		"watch_pause_after = 3\n\n[rounds.secrets.mail]\nvars = { WATCH_SECRET = \""+secret+"\" }\ncommand = [\"/bin/cat\", \""+tokenFile+"\"]\n")
	e.restart(false)
	c := e.control()
	out := filepath.Join(e.work, "events.json")
	copyEvents := `cp "$GFF_ROUND_EVENTS" ` + out + `.tmp && mv ` + out + `.tmp ` + out

	if o, err := roundCLI(e, "", "add", "nope", "-watch", "-secrets", "missing", "-project", repo, "-command", "--", "/bin/sh", script, wdir, "--", "/bin/true"); err == nil || !strings.Contains(o, "no secret set missing") {
		t.Fatalf("a missing secret set: %v %s", err, o)
	}
	mustRound(t, e, "add", "mail", "-watch", "-secrets", "mail", "-kinds", "mail", "-debounce", "1s", "-cooldown", "1s",
		"-project", repo, "-command", "--", "/bin/sh", script, wdir, "--", "/bin/sh", "-c", copyEvents)
	w := waitWatcher(t, c, "mail", watchRunning)
	waitUntil(t, "the first start", func() bool { return len(watchStarts(t, wdir)) == 1 })
	first := watchStarts(t, wdir)[0]
	r := getRound(t, c, "mail")
	if first.secret != secret || first.token != token || first.round != r.ID || first.cursor != "" {
		t.Fatalf("the watcher's environment: %+v", first)
	}
	ss, ok := runSessionOf(structureOf(t, c), w.Session)
	if !ok || ss.Name != "mail-watch" || ss.Round == nil || !ss.Round.Watcher || ss.Round.Round != r.ID || ss.Round.Run != "" {
		t.Fatalf("the watcher session %+v", ss)
	}
	waitUntil(t, "the watcher's stderr in its pane", func() bool { return bytes.Contains(e.ringBytes(ss.Pane), []byte("watcher stderr line")) })

	// A burst makes one run: two mail events, a cursor, a bad line, another kind, a repeat.
	say(t, wdir,
		`{"kind":"mail","key":"m1","title":"Invoice 1042 from billing@example.com","level":"active","data":{"from":"billing@example.com"}}`,
		`{"cursor":"c1"}`,
		`not json at all`,
		`{"kind":"news","title":"not counted"}`,
		`{"kind":"mail","key":"m1","title":"a repeat"}`,
		`{"kind":"mail","key":"m2","title":"Alert from alerts@example.net"}`)
	done := waitRun(t, e, c, "mail", api.RoundRunStateSucceeded)
	if done.Trigger != api.RoundTriggerKindWatch || done.Events != 2 {
		t.Fatalf("run %+v", done)
	}
	if b := eventFile(t, out); len(b.Events) != 2 || b.Events[0].Kind != "mail" || b.Events[1].Title != "Alert from alerts@example.net" {
		t.Fatalf("events %+v", b)
	}
	waitUntil(t, "the events and the drop line in the pane", func() bool {
		ring := e.ringBytes(ss.Pane)
		return bytes.Contains(ring, []byte("event: Invoice 1042")) && bytes.Contains(ring, []byte("dropped 1 lines; the last: not a JSON object"))
	})
	if w := watcherOf(t, c, "mail"); w.Drops != 1 {
		t.Errorf("drops %d, want 1 (quiet drops are not counted)", w.Drops)
	}

	// A planned gff-hab restart leaves the watcher running; what it prints meanwhile arrives
	// once after the restart.
	e.stopHab(false)
	os.Remove(out)
	say(t, wdir, `{"kind":"mail","key":"m3","title":"while gff-hab was down"}`)
	e.serve()
	c = e.control()
	waitWatcher(t, c, "mail", watchRunning)
	done = waitRun(t, e, c, "mail", api.RoundRunStateSucceeded)
	waitUntil(t, "the second run", func() bool { return len(roundRuns(t, c, "mail")) == 2 })
	done = waitRun(t, e, c, "mail", api.RoundRunStateSucceeded)
	if b := eventFile(t, out); done.Events != 1 || len(b.Events) != 1 || b.Events[0].Title != "while gff-hab was down" {
		t.Fatalf("the line printed while gff-hab was down: %+v %+v", done, b)
	}
	if s := watchStarts(t, wdir); len(s) != 1 {
		t.Fatalf("the restart started the watcher again: %+v", s)
	}
	// A killed gff-hab, too, leaves the watcher (the same pid) running.
	e.restart(true)
	c = e.control()
	waitWatcher(t, c, "mail", watchRunning)
	time.Sleep(time.Second)
	if s := watchStarts(t, wdir); len(s) != 1 {
		t.Fatalf("a killed gff-hab's restart started the watcher again: %+v", s)
	}

	// An edit of another field leaves the watcher alone; an edit of watch restarts it, at once
	// and with the cursor, and counts toward no backoff.
	mustRound(t, e, "edit", "mail", "-timeout", "5m")
	time.Sleep(time.Second)
	if s := watchStarts(t, wdir); len(s) != 1 {
		t.Fatalf("an edit of the timeout restarted the watcher: %+v", s)
	}
	mustRound(t, e, "edit", "mail", "-kinds", "mail,news")
	waitUntil(t, "the restart after the edit", func() bool { return len(watchStarts(t, wdir)) == 2 })
	if s := watchStarts(t, wdir)[1]; s.cursor != "c1" || s.pid == first.pid {
		t.Errorf("the second start %+v", s)
	}
	w = waitWatcher(t, c, "mail", watchRunning)
	if w.Restarts != 1 || !strings.Contains(w.LastExit, "hung up by gff-hab") {
		t.Errorf("after the edit %+v", w)
	}

	// Resume on the watcher session is refused: gff-hab starts watchers.
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var ce *control.Error
	if err := c.Call(ctx, "session.resume", api.SessionRef{Session: w.Session}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
		t.Errorf("session.resume on the watcher session: %v", err)
	}

	// Crashes back off 1 s, then 2 s; the third quick exit pauses the round (watch_pause_after 3).
	writeFile(t, filepath.Join(wdir, "die"), "")
	waitUntil(t, "the exit pause", func() bool {
		r := getRound(t, c, "mail")
		return r.Paused && r.PausedReason != ""
	})
	s := watchStarts(t, wdir)
	if len(s) != 4 {
		t.Fatalf("starts %+v, want 2 and two restarts after crashes", s)
	}
	if d1, d2 := s[2].at-s[1].at, s[3].at-s[2].at; d1 < 1 || d2 < 2 || d2 < d1 {
		t.Errorf("backoff gaps %d s and %d s, want about 1 and 2", d1, d2)
	}
	w = waitWatcher(t, c, "mail", watchStopped)
	if w.LastExit != "exit status 3" {
		t.Errorf("stopped watcher %+v", w)
	}
	// Unpause clears the count and starts it again.
	os.Remove(filepath.Join(wdir, "die"))
	mustRound(t, e, "unpause", "mail")
	waitWatcher(t, c, "mail", watchRunning)

	// Export and apply carry the watcher and the set's name, never a value.
	exported := mustRound(t, e, "export")
	for _, want := range []string{`secrets = "mail"`, `[rounds.watch]`, `kinds = ["mail", "news"]`} {
		if !strings.Contains(exported, want) {
			t.Errorf("export lacks %q:\n%s", want, exported)
		}
	}
	file := filepath.Join(e.work, "rounds.toml")
	writeFile(t, file, exported)
	if o := mustRound(t, e, "apply", file); strings.TrimSpace(o) != "no changes" {
		t.Fatalf("apply of the exported file: %s", o)
	}

	// A pause stops the watcher; a delete removes its session once its pane exited.
	mustRound(t, e, "pause", "mail")
	waitUntil(t, "the watcher session exited", func() bool {
		ss, ok := runSessionOf(structureOf(t, c), w.Session)
		return ok && ss.State == api.SessionStateExited
	})
	n := len(watchStarts(t, wdir))
	mustRound(t, e, "rm", "-yes", "mail")
	waitUntil(t, "the watcher session removed", func() bool {
		_, ok := runSessionOf(structureOf(t, c), w.Session)
		return !ok
	})
	if len(watchStarts(t, wdir)) != n {
		t.Error("the watcher started after its round was paused")
	}

	// The secret set's values reached the watcher and nothing else: not gff-hab's log, not a
	// file of its state (the rounds store, the session records, the resume book, the catalog).
	for _, v := range []string{secret, token} {
		if strings.Contains(logBuf.String(), v) {
			t.Errorf("gff-hab's log holds a secret value")
		}
		filepath.Walk(e.state, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Mode()&os.ModeType != 0 {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(v)) {
				t.Errorf("%s holds a secret value", strings.TrimPrefix(p, e.state))
			}
			return nil
		})
		if strings.Contains(exported, v) {
			t.Error("the export holds a secret value")
		}
	}
}

// TestWatchPaneRefused: the wrapper in a pane that is not the round's watcher pane gets its
// lines refused (round.watch_event, denied), and says so in its pane.
func TestWatchPaneRefused(t *testing.T) {
	e, repo := newRoundEnv(t)
	c := e.control()
	mustRound(t, e, "add", "w", "-watch", "-project", repo, "-command", "--", "/bin/sleep", "60", "--", "/bin/true")
	r := getRound(t, c, "w")
	waitWatcher(t, c, "w", watchRunning)
	var pane string
	defer func() {
		if t.Failed() {
			t.Logf("pane: %q", e.ringBytes(pane))
		}
	}()
	pane = e.spawn(c, "exec "+bin+` watch-pane -round `+r.ID+` -- /bin/sh -c 'echo "{\"title\":\"x\"}"; sleep 30'`)
	waitUntil(t, "the refusal in the pane", func() bool {
		return bytes.Contains(e.ringBytes(pane), []byte("gff-hab refused 1 lines")) && bytes.Contains(e.ringBytes(pane), []byte("not the watcher pane"))
	})
	if r := getRound(t, c, "w"); r.Pending != 0 {
		t.Errorf("a refused line joined the batch: %d", r.Pending)
	}
}
