package capcomengine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

type fakeProc struct {
	pid  int
	done chan struct{}
	once sync.Once
	hold bool // Stop leaves it running: an engine that finishes its turn first
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Pid() int              { return p.pid }
func (p *fakeProc) Stop(time.Duration) error {
	if !p.hold {
		p.exit()
	}
	return nil
}
func (p *fakeProc) exit() { p.once.Do(func() { close(p.done) }) }

type fakeJob struct {
	mu      sync.Mutex
	starts  []JobSpec
	procs   []*fakeProc
	adopt   *fakeProc
	started chan *fakeProc
	fail    error // Start fails with it
}

func (j *fakeJob) Start(_ context.Context, spec JobSpec) (Proc, error) {
	j.mu.Lock()
	if err := j.fail; err != nil {
		j.mu.Unlock()
		return nil, err
	}
	p := &fakeProc{pid: 1000 + len(j.procs), done: make(chan struct{})}
	j.starts = append(j.starts, spec)
	j.procs = append(j.procs, p)
	j.mu.Unlock()
	j.started <- p
	return p, nil
}

func (j *fakeJob) Adopt(context.Context) (Proc, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.adopt == nil {
		return nil, false
	}
	return j.adopt, true
}

type supT struct {
	t       *testing.T
	dir     string
	clock   *fakeClock
	job     *fakeJob
	sup     *Supervisor
	link    *Link
	cfg     capcom.ServiceConfig
	env     map[string]string
	secret  string
	runs    int
	logs    []string
	notices []string
	notes   []string
	mu      sync.Mutex
}

func newSup(t *testing.T, mutate func(*supT)) *supT {
	t.Helper()
	s := &supT{t: t, dir: t.TempDir(), clock: newClock(), job: &fakeJob{started: make(chan *fakeProc, 20)}, env: map[string]string{}}
	s.cfg = capcom.ServiceConfig{Engine: capcom.EngineClaude, Auth: capcom.AuthOAuthToken, Limits: capcom.DefaultLimits}
	if mutate != nil {
		mutate(s)
	}
	th, _, err := capcomthread.Open(filepath.Join(s.dir, "thread"), s.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := OpenStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.link = NewLink(LinkConfig{Thread: th, Record: rec, Now: s.clock.Now, After: s.clock.After, Hello: func() (Message, bool) { return Message{}, true }})
	s.sup = NewSupervisor(SupervisorConfig{
		Dir: s.dir, Job: s.job, Link: s.link, Record: rec,
		Config:     func() capcom.ServiceConfig { s.mu.Lock(); defer s.mu.Unlock(); return s.cfg },
		Getenv:     func(k string) string { s.mu.Lock(); defer s.mu.Unlock(); return s.env[k] },
		RouteAddr:  func(tok string) string { return "http+unix:///sock/hooks/" + tok },
		EngineArgv: func() ([]string, error) { return []string{"node", "/engine.mjs"}, nil },
		RunSecret: func(_ context.Context, argv []string, _ time.Duration) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.runs++
			if s.secret == "" {
				return "", errors.New("the command failed")
			}
			return s.secret + "\n", nil
		},
		After:  s.clock.After,
		Now:    s.clock.Now,
		Note:   func(text string) { s.mu.Lock(); s.notes = append(s.notes, text); s.mu.Unlock() },
		Notice: func(title, body string) { s.mu.Lock(); s.notices = append(s.notices, title); s.mu.Unlock() },
		Logf: func(f string, a ...any) {
			s.mu.Lock()
			s.logs = append(s.logs, strings.TrimSpace(fmt.Sprintf(f, a...)))
			s.mu.Unlock()
		},
	})
	return s
}

func (s *supT) started() *fakeProc {
	s.t.Helper()
	select {
	case p := <-s.job.started:
		return p
	case <-time.After(5 * time.Second):
		s.t.Fatal("no engine start")
	}
	return nil
}

func (s *supT) noStart() {
	s.t.Helper()
	select {
	case <-s.job.started:
		s.t.Fatal("an engine start")
	case <-time.After(30 * time.Millisecond):
	}
}

func (s *supT) waitState(state string) SupStatus {
	s.t.Helper()
	var st SupStatus
	waitFor(s.t, "state "+state, func() bool { st = s.sup.Status(); return st.State == state })
	return st
}

// noFileHolds checks that no file under the folder holds the planted value.
func (s *supT) noFileHolds(value string) {
	s.t.Helper()
	filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), value) {
			s.t.Fatalf("%s holds the credential", p)
		}
		return nil
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range append(append(s.logs, s.notes...), s.notices...) {
		if strings.Contains(l, value) {
			s.t.Fatalf("a log line holds the credential: %s", l)
		}
	}
}

func TestSupervisorNoCredential(t *testing.T) {
	s := newSup(t, nil)
	s.sup.Ensure(context.Background())
	st := s.waitState(StateError)
	if st.Reason != "no Claude token: run claude setup-token and set [capcom] oauth_token_command" {
		t.Fatal(st.Reason)
	}
	s.noStart()
	s.mu.Lock()
	s.cfg.Auth = capcom.AuthAPIKey
	s.mu.Unlock()
	s.sup.Ensure(context.Background())
	if st := s.waitState(StateError); !strings.HasPrefix(st.Reason, "no API key") {
		t.Fatal(st.Reason)
	}
}

func TestSupervisorEngineUnset(t *testing.T) {
	s := newSup(t, func(s *supT) { s.cfg.Engine = capcom.EngineNone; s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "x-token" })
	s.sup.Ensure(context.Background())
	s.waitState(StateOff)
	s.noStart()
}

// The command is the first source and the environment the second; the value reaches hello only,
// never the job's environment file, a log or any file (10.2).
func TestSupervisorCredentialSources(t *testing.T) {
	s := newSup(t, func(s *supT) {
		s.cfg.OAuthCommand = []string{"print-token"}
		s.secret = "planted-from-command"
		s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "planted-from-env"
		s.env["CLAUDE_CONFIG_DIR"] = "/cfg"
	})
	s.sup.Ensure(context.Background())
	s.started()
	if c, ok := s.sup.Credential(); !ok || c.Value != "planted-from-command" || c.Kind != capcom.AuthOAuthToken {
		t.Fatalf("credential %v", c)
	}
	spec := s.job.starts[0]
	env, _ := os.ReadFile(filepath.Join(s.dir, EnvFile))
	tok, pane := s.sup.Token(), s.sup.PaneToken()
	if tok == pane || !strings.Contains(string(env), "GFF_HAB=http+unix:///sock/hooks/"+pane) || !strings.Contains(string(env), TokenVar+"="+tok) ||
		!strings.Contains(string(env), "CLAUDE_CONFIG_DIR=/cfg") {
		t.Fatalf("env file %q", env)
	}
	if fi, _ := os.Stat(filepath.Join(s.dir, EnvFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode %v", fi.Mode())
	}
	for _, e := range spec.Env {
		if strings.Contains(e, "planted") {
			t.Fatalf("the job's environment holds the credential: %s", e)
		}
	}
	s.noFileHolds("planted-from-command")
	s.noFileHolds("planted-from-env")

	// The command fails: the environment is the source.
	s2 := newSup(t, func(s *supT) {
		s.cfg.OAuthCommand = []string{"print-token"}
		s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "planted-from-env"
	})
	s2.sup.Ensure(context.Background())
	s2.started()
	if c, _ := s2.sup.Credential(); c.Value != "planted-from-env" {
		t.Fatalf("credential %v", c)
	}
	// An API key from its own variable.
	s3 := newSup(t, func(s *supT) { s.cfg.Auth = capcom.AuthAPIKey; s.env["ANTHROPIC_API_KEY"] = "planted-key" })
	s3.sup.Ensure(context.Background())
	s3.started()
	if c, _ := s3.sup.Credential(); c.Value != "planted-key" || c.Kind != capcom.AuthAPIKey {
		t.Fatalf("credential %v", c)
	}
}

// An installed hab with no command says so in one line, once.
func TestSupervisorInstalledLine(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "planted" })
	s.sup.cfg.Installed = true
	s.sup.Ensure(context.Background())
	p := s.started()
	s.sup.Lose()
	p.exit()
	s.waitState(StateOff)
	s.sup.Ensure(context.Background())
	s.started()
	n := 0
	s.mu.Lock()
	for _, l := range s.logs {
		if strings.Contains(l, "an installed gff-hab reads the credential from [capcom] oauth_token_command") {
			n++
		}
	}
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("the installed line logged %d times", n)
	}
}

// Crashes restart after restart_delay with a new token each start; crash_limit crashes in
// crash_window stop the restarts, and Resume clears them (3.2).
func TestSupervisorCrashes(t *testing.T) {
	s := newSup(t, func(s *supT) {
		s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok"
		s.cfg.Limits.CrashLimit = 3
		s.cfg.Limits.RestartDelay = 2 * time.Second
	})
	s.sup.Ensure(context.Background())
	p := s.started()
	tokens := map[string]bool{s.sup.Token(): true}
	for i := 0; i < 2; i++ {
		p.exit()
		s.waitState(StateError)
		s.clock.Advance(time.Second)
		s.noStart()
		s.clock.Advance(time.Second)
		p = s.started()
		if tokens[s.sup.Token()] {
			t.Fatal("a restart kept the token")
		}
		tokens[s.sup.Token()] = true
	}
	p.exit()
	st := s.waitState(StateError)
	if !strings.Contains(st.Reason, "crashed 3 times") {
		t.Fatal(st.Reason)
	}
	s.clock.Advance(time.Minute)
	s.noStart()
	// A new gff-hab keeps the stop.
	s.sup.Ensure(context.Background())
	s.noStart()
	s.sup.Resume(context.Background())
	s.started()
	if len(s.sup.cfg.Record.Get().Crashes) != 0 {
		t.Fatal("resume kept the crash count")
	}
}

// A crash outside the window does not count.
func TestSupervisorCrashWindow(t *testing.T) {
	s := newSup(t, func(s *supT) {
		s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok"
		s.cfg.Limits.CrashLimit = 2
		s.cfg.Limits.CrashWindow = time.Minute
	})
	s.sup.Ensure(context.Background())
	p := s.started()
	p.exit()
	s.waitState(StateError)
	s.clock.Advance(2 * time.Minute)
	p = s.started()
	p.exit()
	s.waitState(StateError)
	s.clock.Advance(5 * time.Second)
	s.started() // the first crash fell out of the window
}

// A refused credential reads the source again once after token_retry; a second refusal stops
// the engine with one notice; Resume reads the source again (10.2).
func TestSupervisorRefusedToken(t *testing.T) {
	s := newSup(t, func(s *supT) { s.cfg.OAuthCommand = []string{"print-token"}; s.secret = "first" })
	s.sup.Ensure(context.Background())
	p := s.started()
	s.sup.EngineError(ErrKindAuth, "refused", true)
	p.exit()
	s.waitState(StateError)
	s.mu.Lock()
	s.secret = "second"
	s.mu.Unlock()
	s.clock.Advance(4 * time.Minute)
	s.noStart()
	s.clock.Advance(time.Minute)
	p = s.started()
	if c, _ := s.sup.Credential(); c.Value != "second" {
		t.Fatal("the retry did not run the command again")
	}
	s.sup.EngineError(ErrKindAuth, "refused", true)
	p.exit()
	st := s.waitState(StateError)
	if !strings.Contains(st.Reason, "claude setup-token") {
		t.Fatal(st.Reason)
	}
	s.clock.Advance(time.Hour)
	s.noStart()
	s.mu.Lock()
	notices := len(s.notices)
	s.mu.Unlock()
	if notices != 1 {
		t.Fatalf("%d notices", notices)
	}
	s.mu.Lock()
	s.secret = "third"
	s.mu.Unlock()
	s.sup.Resume(context.Background())
	s.started()
	if c, _ := s.sup.Credential(); c.Value != "third" {
		t.Fatal("resume did not read the source again")
	}
	s.noFileHolds("third")
}

// A good turn between two refusals starts the count again.
func TestSupervisorRefusalCountClears(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.Ensure(context.Background())
	p := s.started()
	s.sup.EngineError(ErrKindAuth, "", true)
	p.exit()
	s.waitState(StateError)
	s.clock.Advance(5 * time.Minute)
	p = s.started()
	s.sup.TurnEnded(ResultSuccess)
	s.sup.EngineError(ErrKindAuth, "", true)
	p.exit()
	s.waitState(StateError)
	s.clock.Advance(5 * time.Minute)
	s.started()
}

func TestSupervisorAdoptAndBuildDrain(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	writeEnvFile(filepath.Join(s.dir, EnvFile), []string{"GFF_HAB=http+unix:///sock/hooks/oldpane0123456789", TokenVar + "=oldtoken0123456789"})
	s.sup.cfg.Record.Update(func(r *Record) { r.Build = "build-1" })
	s.sup.cfg.Build = func() string { return "build-2" }
	old := &fakeProc{pid: 77, done: make(chan struct{})}
	s.job.adopt = old
	s.sup.Ensure(context.Background())
	s.noStart()
	if s.sup.Token() != "oldtoken0123456789" || s.sup.PaneToken() != "oldpane0123456789" || s.sup.Status().State != StateRunning {
		t.Fatalf("adopt: token %q, %+v", s.sup.Token(), s.sup.Status())
	}
	s.job.adopt = nil
	s.sup.Ready() // no connection: the job is stopped at once
	s.started()
	if s.sup.cfg.Record.Get().Build != "build-2" {
		t.Fatal("the new build is not recorded")
	}
}

// A start that fails leaves the build of before recorded: a build that never ran is not the
// current one.
func TestSupervisorFailedStartKeepsBuild(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.cfg.Record.Update(func(r *Record) { r.Build = "build-1" })
	s.sup.cfg.Build = func() string { return "build-2" }
	s.job.mu.Lock()
	s.job.fail = errors.New("no launchd")
	s.job.mu.Unlock()
	s.sup.Ensure(context.Background())
	s.waitState(StateError)
	if b := s.sup.cfg.Record.Get().Build; b != "build-1" {
		t.Fatalf("after a failed start the record names %q, want build-1", b)
	}
}

func TestSupervisorPauseResume(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.Ensure(context.Background())
	s.started()
	s.sup.Pause() // no connection: the job ends at once
	s.waitState(StatePaused)
	s.clock.Advance(time.Hour)
	s.noStart()
	s.sup.Ensure(context.Background()) // a gff-hab restart keeps the pause
	s.noStart()
	s.sup.Resume(context.Background())
	s.started()
}

// A resume while a pause drains is no crash: the engine exits after its turn and starts again
// at once, with no crash counted (3.2, 11).
func TestSupervisorResumeDuringPauseDrain(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.Ensure(context.Background())
	p := s.started()
	p.hold = true
	s.sup.Pause()
	s.sup.Resume(context.Background())
	s.noStart()
	p.exit()
	s.started()
	if n := len(s.sup.cfg.Record.Get().Crashes); n != 0 {
		t.Fatalf("%d crashes counted", n)
	}
}

// A gff-hab restart while a pause drains keeps the pause: the adopted engine's exit starts
// nothing, and the state is paused.
func TestSupervisorAdoptPausedDrain(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.cfg.Record.Update(func(r *Record) { r.Paused = true })
	old := &fakeProc{pid: 77, done: make(chan struct{})}
	s.job.adopt = old
	s.sup.Ensure(context.Background())
	s.job.adopt = nil
	old.exit()
	s.waitState(StatePaused)
	s.clock.Advance(time.Hour)
	s.noStart()
	if n := len(s.sup.cfg.Record.Get().Crashes); n != 0 {
		t.Fatalf("%d crashes counted", n)
	}
}

func TestSupervisorNodeMissing(t *testing.T) {
	s := newSup(t, func(s *supT) { s.env["CLAUDE_CODE_OAUTH_TOKEN"] = "tok" })
	s.sup.cfg.EngineArgv = func() ([]string, error) { return nil, errors.New("Node.js not found: install Node.js 18 or later") }
	s.sup.Ensure(context.Background())
	if st := s.waitState(StateError); !strings.HasPrefix(st.Reason, "Node.js not found") {
		t.Fatal(st.Reason)
	}
	s.noStart()
}
