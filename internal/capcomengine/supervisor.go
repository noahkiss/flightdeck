package capcomengine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcom"
)

// Files in the Capcom folder the supervisor writes.
const (
	EnvFile = "engine.env" // the job's environment: GFF_HAB with the pane route token, the engine route token, CLAUDE_CONFIG_DIR (3.3)
	// TokenVar names the engine route's token in the job's environment. GFF_HAB holds another
	// token, for the pane route: the engine passes GFF_HAB on to the SDK's binary and gff-hab
	// mcp, and keeps this one to itself, so a process with GFF_HAB cannot open /capcom/<token>
	// (3.3, 4.4).
	TokenVar = "GFF_HAB_CAPCOM_TOKEN"
	PidFile  = "engine.pid" // ProcessJob's pid
	LogFile  = "engine.log" // the engine's stdout and stderr
)

// Supervisor states, for the session row (7.6).
const (
	StateOff     = "off"     // [capcom] engine is unset, or this hab does not hold the mark
	StateRunning = "running" // the job runs; the link says whether the engine is connected
	StatePaused  = "paused"  // gff-hab capcom pause (11)
	StateError   = "error"   // the engine cannot run: Reason says why
)

// SupStatus is the supervisor's state and why.
type SupStatus struct {
	State  string
	Reason string
}

// SupervisorConfig wires a supervisor.
type SupervisorConfig struct {
	Dir    string // <state>/capcom
	Job    Job
	Link   *Link
	Record *Store
	// Config reads the [capcom] section again; its problems are the caller's to log.
	Config func() capcom.ServiceConfig
	// Getenv reads gff-hab's own environment (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY,
	// CLAUDE_CONFIG_DIR).
	Getenv func(string) string
	// Installed is true for a hab an init system runs (gff-hab install): its environment never
	// holds the token (10.2).
	Installed bool
	// RouteAddr makes GFF_HAB for a token: http+unix://<hook socket>/hooks/<token>.
	RouteAddr func(token string) string
	// EngineArgv is the command that runs the engine (node and the engine file). An error is the
	// session row's reason, such as "Node.js not found".
	EngineArgv func() ([]string, error)
	// BaseEnv is the environment every engine start begins from (PATH, HOME, the locale).
	BaseEnv func() []string
	// Build names the engine build that would start now. An adopted engine of another build is
	// drained and the new build started (3.2).
	Build func() string
	// Prepare writes the engine folder's manual and .mcp.json (3.4).
	Prepare func(dir string) error
	// RunSecret runs a credential command and returns what it printed (10.2).
	RunSecret func(ctx context.Context, argv []string, timeout time.Duration) (string, error)
	After     func(d time.Duration, f func()) (stop func() bool)
	Now       func() time.Time
	OnStatus  func(SupStatus)
	Note      func(text string)        // a Flightdeck note in the thread
	Notice    func(title, body string) // an active notice
	Logf      func(string, ...any)
	Grace     time.Duration // SIGTERM to SIGKILL when the supervisor ends a job; 0: 10 s
}

// Supervisor starts, adopts, restarts and drains the engine job (3.2), with a new route token
// at each start (3.3) and the credential in memory only (10.2).
type Supervisor struct {
	cfg  SupervisorConfig
	once *capcom.Once

	mu        sync.Mutex
	proc      Proc
	token     string // the engine route's (/capcom/<token>)
	paneToken string // the pane route's, in GFF_HAB (/hooks/<token>)
	cred      *Credential
	planned   string // why the running engine is meant to exit: pause, lose, build, auth, start, stop
	authFails int
	status    SupStatus
	retry     func() bool // a pending restart or token retry
	drainNew  bool        // an adopted engine of another build: drain it once it is ready
	lastStart time.Time
}

// NewSupervisor returns a supervisor; nothing starts until Ensure.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Grace == 0 {
		cfg.Grace = 10 * time.Second
	}
	if cfg.Note == nil {
		cfg.Note = func(string) {}
	}
	if cfg.Notice == nil {
		cfg.Notice = func(string, string) {}
	}
	if cfg.BaseEnv == nil {
		cfg.BaseEnv = func() []string { return nil }
	}
	if cfg.Build == nil {
		cfg.Build = func() string { return "" }
	}
	return &Supervisor{cfg: cfg, once: capcom.NewOnce(cfg.Logf), status: SupStatus{State: StateOff}}
}

// Status returns the supervisor's state.
func (s *Supervisor) Status() SupStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Supervisor) setLocked(state, reason string) {
	s.status = SupStatus{State: state, Reason: reason}
}

func (s *Supervisor) report() {
	if s.cfg.OnStatus != nil {
		s.cfg.OnStatus(s.Status())
	}
}

// Token is the route token of the engine that runs now, or "". Any other token is refused on
// the engine route (3.3).
func (s *Supervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// PaneToken is the pane route token of the engine that runs now, in its GFF_HAB, or "": the
// token its tool calls (gff-hab mcp) reach the pane route with, as the Capcom session.
func (s *Supervisor) PaneToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paneToken
}

// Credential is what hello hands the engine (10.2).
func (s *Supervisor) Credential() (Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred == nil {
		return Credential{}, false
	}
	return *s.cred, true
}

// noCredential is the session row's line when there is no credential (10.2).
func noCredential(auth string) string {
	if auth == capcom.AuthAPIKey {
		return "no API key: set [capcom] api_key_command"
	}
	return "no Claude token: run claude setup-token and set [capcom] oauth_token_command"
}

// loadCredential reads the credential: the command first, then gff-hab's environment (10.2).
// It returns the reason when there is none. The value is never logged.
func (s *Supervisor) loadCredential(ctx context.Context, cfg capcom.ServiceConfig) (*Credential, string) {
	cmd, env := cfg.OAuthCommand, "CLAUDE_CODE_OAUTH_TOKEN"
	if cfg.Auth == capcom.AuthAPIKey {
		cmd, env = cfg.APIKeyCommand, "ANTHROPIC_API_KEY"
	}
	if len(cmd) > 0 && s.cfg.RunSecret != nil {
		v, err := s.cfg.RunSecret(ctx, cmd, cfg.Limits.CommandTimeout)
		v = strings.TrimSpace(v)
		switch {
		case err != nil:
			s.cfg.Logf("capcom: %v", err) // RunSecret's error holds at most a cut first line of stderr
		case v == "" || strings.ContainsAny(v, " \t\r\n"):
			s.cfg.Logf("capcom: [capcom] %s printed no credential", commandKey(cfg.Auth))
		default:
			return &Credential{Kind: cfg.Auth, Value: v}, ""
		}
	}
	if len(cmd) == 0 && s.cfg.Installed {
		s.once.Log(fmt.Sprintf("capcom: an installed gff-hab reads the credential from [capcom] %s; gff-hab install never copies %s into the job", commandKey(cfg.Auth), env))
	}
	if v := strings.TrimSpace(s.cfg.Getenv(env)); v != "" {
		return &Credential{Kind: cfg.Auth, Value: v}, ""
	}
	return nil, noCredential(cfg.Auth)
}

func commandKey(auth string) string {
	if auth == capcom.AuthAPIKey {
		return "api_key_command"
	}
	return "oauth_token_command"
}

// Ensure runs the engine when the config names one: it adopts a job that runs, or starts one.
// A pause or a stop after crashes or refusals holds until Resume.
func (s *Supervisor) Ensure(ctx context.Context) {
	cfg := s.cfg.Config()
	rec := s.cfg.Record.Get()
	s.mu.Lock()
	switch {
	case cfg.Engine == capcom.EngineNone:
		s.setLocked(StateOff, "")
		s.mu.Unlock()
		s.report()
		return
	case s.proc != nil:
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if p, ok := s.cfg.Job.Adopt(ctx); ok {
		token, paneToken := readTokens(filepath.Join(s.cfg.Dir, EnvFile))
		cred, why := s.loadCredential(ctx, cfg)
		planned := ""
		if rec.Paused {
			planned = "pause" // a pause drain that a gff-hab restart cut: it stays a pause
		}
		s.mu.Lock()
		s.proc, s.token, s.paneToken, s.cred, s.planned = p, token, paneToken, cred, planned
		s.drainNew = rec.Build != "" && rec.Build != s.cfg.Build()
		if why != "" {
			s.cfg.Logf("capcom: adopted the running engine, but %s", why)
		}
		s.setLocked(StateRunning, "")
		s.mu.Unlock()
		s.cfg.Logf("capcom: adopted the running engine (pid %d)", p.Pid())
		go s.watch(p)
		s.report()
		return
	}
	switch {
	case rec.Paused:
		s.mu.Lock()
		s.setLocked(StatePaused, "")
		s.mu.Unlock()
		s.report()
		return
	case rec.Stopped != "":
		s.mu.Lock()
		s.setLocked(StateError, rec.Stopped)
		s.mu.Unlock()
		s.report()
		return
	}
	s.start(ctx)
}

// start starts the engine with a new token. The caller holds no lock.
func (s *Supervisor) start(ctx context.Context) {
	cfg := s.cfg.Config()
	fail := func(reason string) {
		s.mu.Lock()
		s.setLocked(StateError, reason)
		s.mu.Unlock()
		s.cfg.Logf("capcom: the engine does not start: %s", reason)
		s.report()
	}
	if cfg.Engine == capcom.EngineNone {
		return
	}
	if s.cfg.Record.Get().Paused {
		s.mu.Lock()
		s.setLocked(StatePaused, "")
		s.mu.Unlock()
		s.report()
		return
	}
	cred, why := s.loadCredential(ctx, cfg)
	if cred == nil {
		fail(why)
		return
	}
	argv, err := s.cfg.EngineArgv()
	if err != nil {
		fail(err.Error())
		return
	}
	dir := EngineDir(s.cfg.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(err.Error())
		return
	}
	if s.cfg.Prepare != nil {
		if err := s.cfg.Prepare(dir); err != nil {
			fail("engine folder: " + err.Error())
			return
		}
	}
	token, paneToken := newToken(), newToken()
	envs := []string{"GFF_HAB=" + s.cfg.RouteAddr(paneToken), TokenVar + "=" + token}
	if v := s.cfg.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		envs = append(envs, "CLAUDE_CONFIG_DIR="+v)
	}
	// The token is in place before the job runs, so its first connection is taken; the old one
	// is refused from here on.
	s.mu.Lock()
	s.token, s.paneToken, s.cred, s.planned, s.drainNew = token, paneToken, cred, "", false
	s.mu.Unlock()
	if err := writeEnvFile(filepath.Join(s.cfg.Dir, EnvFile), envs); err != nil {
		fail("engine environment file: " + err.Error())
		return
	}
	// The build is recorded before the job runs, so a running engine is never recorded as an
	// older build, even when gff-hab stops between the two.
	build := s.cfg.Build()
	s.cfg.Record.Update(func(r *Record) { r.Build = build })
	p, err := s.cfg.Job.Start(ctx, JobSpec{Argv: argv, Dir: dir, Env: append(s.cfg.BaseEnv(), envs...), Log: filepath.Join(s.cfg.Dir, LogFile)})
	if err != nil {
		fail("start the engine job: " + err.Error())
		return
	}
	s.mu.Lock()
	s.proc = p
	s.lastStart = s.cfg.Now()
	s.setLocked(StateRunning, "")
	s.mu.Unlock()
	s.cfg.Logf("capcom: started the engine (pid %d)", p.Pid())
	go s.watch(p)
	s.report()
}

func (s *Supervisor) watch(p Proc) {
	<-p.Done()
	s.exited(p)
}

// exited handles the end of the engine process.
func (s *Supervisor) exited(p Proc) {
	cfg := s.cfg.Config()
	s.mu.Lock()
	if s.proc != p {
		s.mu.Unlock()
		return
	}
	s.proc = nil
	planned := s.planned
	s.planned = ""
	s.mu.Unlock()
	s.cfg.Link.Drop()
	switch planned {
	case "pause":
		s.mu.Lock()
		s.setLocked(StatePaused, "")
		s.mu.Unlock()
		s.cfg.Logf("capcom: the engine stopped for the pause")
	case "lose", "stop":
		s.mu.Lock()
		s.token, s.paneToken = "", ""
		s.setLocked(StateOff, "")
		s.mu.Unlock()
		s.cfg.Logf("capcom: the engine stopped (%s)", planned)
	case "build":
		s.cfg.Logf("capcom: the engine of the earlier build finished; starting this build's engine")
		s.start(context.Background())
		return
	case "resume":
		// A resume came while a pause drained: the engine had its shutdown, so it exits after
		// its turn; that exit is planned, and the engine starts again at once.
		s.cfg.Logf("capcom: the paused engine finished its turn; starting it again for the resume")
		s.start(context.Background())
		return
	case "start":
		// The start check failed: a restart would fail the same way (5.1).
		reason := "the engine's start check failed: " + LastLine(filepath.Join(s.cfg.Dir, LogFile))
		s.cfg.Record.Update(func(r *Record) { r.Stopped = reason })
		s.mu.Lock()
		s.setLocked(StateError, reason)
		s.mu.Unlock()
	case "auth":
		s.mu.Lock()
		fails := s.authFails
		s.mu.Unlock()
		if fails >= 2 {
			reason := "the Claude token was refused again: run claude setup-token, update the token your command prints, then gff-hab capcom resume"
			if cfg.Auth == capcom.AuthAPIKey {
				reason = "the API key was refused again: update the key your command prints, then gff-hab capcom resume"
			}
			s.cfg.Record.Update(func(r *Record) { r.Stopped = reason })
			s.mu.Lock()
			s.setLocked(StateError, reason)
			s.mu.Unlock()
			s.cfg.Note("Capcom stopped: its credential was refused twice")
			s.cfg.Notice("Capcom stopped", reason)
			break
		}
		s.mu.Lock()
		s.setLocked(StateError, fmt.Sprintf("the credential was refused; gff-hab reads it again in %s", cfg.Limits.TokenRetry))
		s.retry = s.cfg.After(cfg.Limits.TokenRetry, func() { s.start(context.Background()) })
		s.mu.Unlock()
	default:
		now := s.cfg.Now().UTC()
		var n int
		s.cfg.Record.Update(func(r *Record) {
			kept := r.Crashes[:0]
			for _, t := range r.Crashes {
				if now.Sub(t) < cfg.Limits.CrashWindow {
					kept = append(kept, t)
				}
			}
			r.Crashes = append(kept, now)
			n = len(r.Crashes)
		})
		line := LastLine(filepath.Join(s.cfg.Dir, LogFile))
		if n >= cfg.Limits.CrashLimit {
			reason := fmt.Sprintf("the engine crashed %d times in %s; gff-hab capcom resume starts it again", n, cfg.Limits.CrashWindow)
			if line != "" {
				reason = fmt.Sprintf("the engine crashed %d times in %s; last line: %s; gff-hab capcom resume starts it again", n, cfg.Limits.CrashWindow, line)
			}
			s.cfg.Record.Update(func(r *Record) { r.Stopped = reason })
			s.mu.Lock()
			s.setLocked(StateError, reason)
			s.mu.Unlock()
			s.cfg.Note("Capcom stopped after repeated crashes")
			s.cfg.Logf("capcom: %s", reason)
			break
		}
		s.cfg.Logf("capcom: the engine exited unasked (%d in %s); restarting in %s", n, cfg.Limits.CrashWindow, cfg.Limits.RestartDelay)
		s.cfg.Note("Capcom restarted")
		s.mu.Lock()
		s.setLocked(StateError, strings.TrimSuffix("the engine exited: "+line, ": "))
		s.retry = s.cfg.After(cfg.Limits.RestartDelay, func() { s.start(context.Background()) })
		s.mu.Unlock()
	}
	s.report()
}

// EngineError takes an error the engine reported (4.4): a refused credential, or the start
// check. Either way the exit that follows is no crash.
func (s *Supervisor) EngineError(kind, text string, exiting bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case ErrKindAuth:
		s.authFails++
		s.planned = "auth"
		s.cfg.Logf("capcom: the model provider refused the credential (%d)", s.authFails)
	case ErrKindStart:
		s.planned = "start"
		s.cfg.Logf("capcom: the engine's start check: %s", text)
	default:
		if text != "" {
			s.cfg.Logf("capcom: the engine reported: %s", text)
		}
	}
}

// TurnEnded hears each turn's end. A good turn clears the count of refused credentials.
func (s *Supervisor) TurnEnded(result string) {
	if result != ResultSuccess {
		return
	}
	s.mu.Lock()
	s.authFails = 0
	s.mu.Unlock()
}

// Ready hears that the engine answered ready. An adopted engine of another build is drained
// now: it finishes its turn and exits, and this build's engine starts (3.2).
func (s *Supervisor) Ready() {
	s.mu.Lock()
	drain := s.drainNew
	s.drainNew = false
	s.mu.Unlock()
	if drain {
		s.cfg.Logf("capcom: the running engine is of an earlier build; it finishes its turn first")
		s.stopEngine("build")
	}
}

// stopEngine asks the engine to finish its turn and exit, with why as the planned reason. With
// no connected engine the job is ended at once.
func (s *Supervisor) stopEngine(why string) {
	s.mu.Lock()
	p := s.proc
	if p != nil {
		s.planned = why
	}
	if s.retry != nil {
		s.retry()
		s.retry = nil
	}
	s.mu.Unlock()
	if p == nil {
		return
	}
	if !s.cfg.Link.Shutdown() {
		go p.Stop(s.cfg.Grace)
	}
}

// Pause finishes the turn and stops the engine's use of the SDK session: the engine exits after
// its turn and is not started until Resume (11). The thread still takes messages.
func (s *Supervisor) Pause() {
	s.cfg.Record.Update(func(r *Record) { r.Paused = true })
	s.mu.Lock()
	running := s.proc != nil
	if !running {
		s.setLocked(StatePaused, "")
	}
	s.mu.Unlock()
	s.stopEngine("pause")
	s.report()
}

// Resume clears a pause, the crash count and a stop, reads the credential again, and starts
// the engine.
func (s *Supervisor) Resume(ctx context.Context) {
	s.cfg.Record.Update(func(r *Record) { r.Paused, r.Stopped, r.Crashes = false, "", nil })
	s.mu.Lock()
	s.authFails = 0
	if s.retry != nil {
		s.retry()
		s.retry = nil
	}
	running := s.proc != nil
	if running && s.planned == "pause" {
		s.planned = "resume" // it had its shutdown and exits after its turn: start it again then
	}
	s.mu.Unlock()
	if running {
		return
	}
	s.start(ctx)
}

// Lose stops the engine after its turn: this hab lost the Capcom mark (3.2).
func (s *Supervisor) Lose() { s.stopEngine("lose") }

// Close ends the engine job for good, after its turn: gff-hab uninstall. gff-hab's own stop never
// calls it, since the engine outlives gff-hab (3.2).
func (s *Supervisor) Close() { s.stopEngine("stop") }

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeEnvFile writes KEY=value lines, mode 0600, through a temporary file and a rename.
func writeEnvFile(path string, envs []string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-env-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Chmod(0o600)
	for _, e := range envs {
		fmt.Fprintln(tmp, e)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readTokens reads the engine route token and the pane route token from the job's environment
// file.
func readTokens(path string) (token, paneToken string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "GFF_HAB="); ok {
			if i := strings.LastIndex(v, "/hooks/"); i >= 0 {
				paneToken = v[i+len("/hooks/"):]
			}
		}
		if v, ok := strings.CutPrefix(sc.Text(), TokenVar+"="); ok {
			token = v
		}
	}
	return token, paneToken
}
