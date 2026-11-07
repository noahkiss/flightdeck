package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/service"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// capcomRoute is the route id the engine's token resolves to on the hook socket. It is no pane:
// the structure maps it to the service session (capcom-service.md 3.3), so every tool call of
// the engine's gff-hab mcp is the Capcom session's call.
const capcomRoute = "capcom-engine"

// capcomServiceName is the service session's name, and so its channel name (2.4).
const capcomServiceName = "capcom"

// capcomEngine is the Capcom service on this hab (capcom-service.md): the thread, the engine
// link and its supervisor, and the service session record. It runs only while [capcom] engine
// is set (12.4).
type capcomEngine struct {
	dir      string // <state>/capcom
	store    *structure.Store
	svc      *capcomService
	thread   *capcomthread.Store
	rec      *capcomengine.Store
	link     *capcomengine.Link
	sup      *capcomengine.Supervisor
	config   func() capcom.ServiceConfig
	publish  func(structure.Event)
	notify   func(title, body string)
	message  func(item capcomthread.Item, session string)         // pushes a Capcom message (7.6); nil: none
	attached func(session string, ids []string) ([]string, error) // attachment ids to stored image paths
	device   string
	logf     func(string, ...any)
	// agents carries the service session's agent state on the engine route (7.6, 5.3), so the
	// approvals area, push and the board see Capcom as they see a pane's agent. Nil in tests
	// that need no state.
	agents *harness.Store
	// memory is Capcom's memory in Logbook (6.4); nil on a hab without Logbook.
	memory *capcomMemory

	mu      sync.Mutex
	session string // the service session's id, while this hab holds the mark
	typing  bool

	markMu sync.Mutex // one start or lose at a time, each acting on the mark as it is then

	stateMu sync.Mutex // orders syncState's applies
	stateT  int64      // the last applied update's clock (ms)
	shown   bool       // an agent state was applied for the route since the last exit
}

// capcomEngineSetup is what newCapcomEngine needs from serve.
type capcomEngineSetup struct {
	state      string
	configPath string
	exe        string
	hookSocket string
	device     string
	installed  bool
	job        capcomengine.Job // nil: a plain process (ProcessJob)
	store      *structure.Store
	agents     *harness.Store
	svc        *capcomService
	logbook    *lbsvc.Service // nil: no Logbook
	host       string
	approval   func() time.Duration
	publish    func(structure.Event)
	notify     func(title, body string)
	message    func(item capcomthread.Item, session string)
	logf       func(string, ...any)
}

// pushesMessage reports whether a thread item is a Capcom message, the one kind that pushes
// (7.6): Capcom's own text, not a card, note, tools line or the user's message.
func pushesMessage(it capcomthread.Item) bool {
	return it.Kind == capcomthread.KindMessage && it.Author.Kind == capcomthread.AuthorCapcom && strings.TrimSpace(it.Text) != ""
}

// newCapcomEngine opens the thread and the engine record. It starts nothing: start does, when
// this hab holds the mark.
func newCapcomEngine(s capcomEngineSetup) (*capcomEngine, error) {
	dir := filepath.Join(s.state, capcomFolders)
	once := capcom.NewOnce(s.logf)
	config := func() capcom.ServiceConfig {
		cfg, problems, err := capcom.LoadServiceConfig(s.configPath)
		for _, p := range problems {
			once.Log("capcom: " + p)
		}
		if err != nil {
			once.Log("capcom: " + err.Error())
		}
		return cfg
	}
	th, problems, err := capcomthread.Open(filepath.Join(dir, "thread"), nil)
	if err != nil {
		return nil, err
	}
	for _, p := range problems {
		s.logf("%s", p)
		if s.notify != nil {
			s.notify("Capcom thread file set aside", p.String())
		}
	}
	rec, err := capcomengine.OpenStore(dir)
	if err != nil {
		if rec == nil {
			return nil, err
		}
		s.logf("capcom: %s was not readable and was set aside: %v", capcomengine.StateFile, err)
	}
	e := &capcomEngine{dir: dir, store: s.store, svc: s.svc, thread: th, rec: rec, config: config, publish: s.publish,
		notify: s.notify, message: s.message, device: s.device, logf: s.logf, agents: s.agents}
	if s.logbook != nil {
		e.memory = &capcomMemory{svc: s.logbook, host: s.host}
	}
	th.Watch(func(it capcomthread.Item) {
		e.publish(structure.Event{Type: api.EventCapcomItem, ID: it.ID, Data: apiItem(it)})
		e.publishChat(it)
		// A Capcom message is a communication push (7.6). The thread notifies a capcom
		// message once, when it is added: a repeated reply is dropped before it.
		if e.message != nil && pushesMessage(it) {
			if session := e.Session(); session != "" {
				e.message(it, session)
			}
		}
	})
	e.link = capcomengine.NewLink(capcomengine.LinkConfig{
		Thread: th, Record: rec,
		Hello:          e.hello,
		ApprovalMaxAge: s.approval,
		TypingTimeout:  func() time.Duration { return config().Limits.TypingTimeout },
		OnStatus:       e.linkStatus,
		OnError: func(kind, text string, exiting bool) {
			e.sup.EngineError(kind, text, exiting)
		},
		OnReady:     func() { e.sup.Ready() },
		OnTurnEnded: func(result string) { e.sup.TurnEnded(result) },
		Logf:        s.logf,
		Limits:      func() capcomengine.ContextLimits { return contextLimits(config().Limits) },
		Logbook:     e.logbookPart,
		EngineKind:  func() string { return config().Engine },
	})
	job := s.job
	if job == nil {
		job = capcomengine.ProcessJob{PidFile: filepath.Join(dir, capcomengine.PidFile), Match: engineCommand}
	}
	e.sup = capcomengine.NewSupervisor(capcomengine.SupervisorConfig{
		Dir: dir, Job: job,
		Link: e.link, Record: rec, Config: config, Getenv: os.Getenv, Installed: s.installed,
		RouteAddr:  func(token string) string { return hook.Addr(s.hookSocket, token) },
		EngineArgv: engineArgv(s.exe),
		BaseEnv:    engineBaseEnv,
		Build:      func() string { return engineBuild(s.exe) },
		Prepare: func(dir string) error {
			return capcom.WriteServiceFolder(dir, s.exe, s.svc.readScope())
		},
		RunSecret: func(ctx context.Context, argv []string, timeout time.Duration) (string, error) {
			var out string
			err := push.RunSecret(ctx, "capcom credential command", argv, timeout, 64<<10, func(b []byte) error {
				out = string(b)
				return nil
			})
			return out, err
		},
		OnStatus: func(capcomengine.SupStatus) {
			e.syncState()
			e.svc.poke()
		},
		Note: func(text string) {
			th.Add(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: text})
		},
		Notice: s.notify,
		Logf:   s.logf,
	})
	return e, nil
}

// engineArgv finds Node.js and the engine: $GFF_HAB_CAPCOM_ENGINE, else beside gff-hab
// (libexec/capcom/ of a release, capcom-engine/dist/ of a checkout's build). A file that ends in
// .mjs or .js runs with node; any other runs as it is (a test's stub engine).
func engineArgv(exe string) func() ([]string, error) {
	return func() ([]string, error) {
		file := os.Getenv("GFF_HAB_CAPCOM_ENGINE")
		if file == "" {
			base := filepath.Dir(exe)
			for _, c := range []string{
				filepath.Join(base, "..", "libexec", "capcom", "gff-hab-capcom.mjs"),
				filepath.Join(base, "capcom", "gff-hab-capcom.mjs"),
				filepath.Join(base, "capcom-engine", "dist", "gff-hab-capcom.mjs"),
			} {
				if _, err := os.Stat(c); err == nil {
					file = c
					break
				}
			}
		}
		if file == "" {
			return nil, errors.New("the Capcom engine (gff-hab-capcom.mjs) is not beside gff-hab; set $GFF_HAB_CAPCOM_ENGINE")
		}
		if !strings.HasSuffix(file, ".mjs") && !strings.HasSuffix(file, ".js") {
			return []string{file}, nil
		}
		node, err := nodePath()
		if err != nil {
			return nil, err
		}
		return []string{node, file}, nil
	}
}

// nodeFloor is the SDK's Node.js floor (its engines field).
var nodeFloor = [3]int{18, 14, 0}

// nodePath finds node on PATH (or $GFF_HAB_NODE) and checks its version against the floor.
func nodePath() (string, error) {
	node := os.Getenv("GFF_HAB_NODE")
	if node == "" {
		p, err := exec.LookPath("node")
		if err != nil {
			return "", fmt.Errorf("Node.js not found: install Node.js %d.%d or later and put node on PATH", nodeFloor[0], nodeFloor[1])
		}
		node = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("Node.js at %s does not run: %v", node, err)
	}
	v := strings.TrimSpace(string(out))
	if !nodeAtLeast(v, nodeFloor) {
		return "", fmt.Errorf("Node.js %s is too old: the Capcom engine needs %d.%d or later", v, nodeFloor[0], nodeFloor[1])
	}
	return node, nil
}

// nodeAtLeast reports whether a "v20.1.0" version is at or past floor.
func nodeAtLeast(v string, floor [3]int) bool {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	for i := 0; i < 3; i++ {
		n := 0
		if i < len(parts) {
			n, _ = strconv.Atoi(strings.TrimFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' }))
		}
		if n != floor[i] {
			return n > floor[i]
		}
	}
	return true
}

// engineBaseEnv is the environment an engine start begins from: gff-hab's own, with no
// credential variable and no GFF_HAB of gff-hab's own (10.2).
func engineBaseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY", "GFF_HAB", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
			"CLAUDECODE", "CODEX_THREAD_ID", "GFF_HAB_CAPCOM_KIND", "GFF_HAB_CAPCOM_TOKEN":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// engineBuild names the engine build: its file and the file's size and time.
func engineBuild(exe string) string {
	argv, err := engineArgv(exe)()
	if err != nil || len(argv) == 0 {
		return ""
	}
	f := argv[len(argv)-1]
	fi, err := os.Stat(f)
	if err != nil {
		return f
	}
	return fmt.Sprintf("%s %d %d", f, fi.Size(), fi.ModTime().UnixNano())
}

// hello builds the hello message: the credential (in memory only), this hab's device, the
// settings.
func (e *capcomEngine) hello() (capcomengine.Message, bool) {
	cred, ok := e.sup.Credential()
	if !ok {
		return capcomengine.Message{}, false
	}
	cfg := e.config()
	return capcomengine.Message{Credential: &cred, Device: e.device, Settings: &capcomengine.Settings{
		Model: cfg.Model, SmallModel: cfg.SmallModel, MaxTurns: cfg.Limits.MaxTurns, MaxTurnUSD: cfg.Limits.MaxTurnUSD,
		WakeRetryMS: cfg.Limits.WakeRetry.Milliseconds(), RouteBackoffMaxMS: cfg.Limits.RouteBackoffMax.Milliseconds(),
		SummaryMaxChars: cfg.Limits.SummaryMaxChars, PrecompactTimeoutMS: cfg.Limits.PrecompactTimeout.Milliseconds(),
	}}, true
}

// contextLimits are the context manager's [capcom] keys (6, 19.2).
func contextLimits(l capcom.Limits) capcomengine.ContextLimits {
	return capcomengine.ContextLimits{SummaryMaxChars: l.SummaryMaxChars, SummaryInputChars: l.SummaryInputChars,
		SummaryQuiet: l.SummaryQuiet, ReseedAt: l.ReseedAt, ReseedEarlyAt: l.ReseedEarlyAt, ReseedQuiet: l.ReseedQuiet, ReseedQuietAt: l.ReseedQuietAt,
		SeedMessages: l.SeedMessages, SeedMessagesChars: l.SeedMessagesChars, SeedLogbookBytes: l.SeedLogbookBytes}
}

func (e *capcomEngine) linkStatus(st capcomengine.Status) {
	e.mu.Lock()
	changed := st.Typing != e.typing
	e.typing = st.Typing
	session := e.session
	e.mu.Unlock()
	if changed && session != "" {
		e.publish(structure.Event{Type: api.EventCapcomTyping, ID: session, Data: api.CapcomTyping{On: st.Typing}})
	}
	e.syncState()
	e.svc.poke()
}

// capcomHarness is the harness name of the service session's agent state.
const capcomHarness = "capcom"

// syncState applies the service session's agent state to the engine route from the
// supervisor and the link (5.3, 7.6): blocked:permission while an approval card is open, working
// during a turn, idle between turns and while paused, error when the engine cannot run, none
// (offline) while no engine is connected, exited once this hab lost the mark. The source is
// server: a hook-like source that may open and close the request.
func (e *capcomEngine) syncState() {
	if e.agents == nil {
		return
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	u := e.stateUpdate()
	if u.State == api.AgentStatusExited && !e.shown {
		return
	}
	e.shown = u.State != api.AgentStatusExited
	t := max(e.stateT+1, time.Now().UnixMilli())
	e.stateT = t
	e.agents.Apply(capcomRoute, capcomHarness, api.StateSourceServer, t, u)
}

func (e *capcomEngine) stateUpdate() harness.Update {
	if e.Session() == "" {
		return harness.Update{State: api.AgentStatusExited}
	}
	sup, ls := e.sup.Status(), e.link.Status()
	switch {
	case sup.State == capcomengine.StateError:
		return harness.Update{State: api.AgentStatusError, Failure: sup.Reason}
	case sup.State == capcomengine.StatePaused && ls.Approval == nil && ls.Turn == "":
		return harness.Update{State: api.AgentStatusIdle}
	case sup.State != capcomengine.StateRunning && sup.State != capcomengine.StatePaused:
		return harness.Update{State: api.AgentStatusNone}
	case !ls.Connected || !ls.Ready:
		return harness.Update{State: api.AgentStatusNone}
	case ls.Approval != nil:
		a := ls.Approval
		req := &api.PermissionRequest{ID: a.Request, Tool: a.Tool, Title: a.Reason, ExpiresAt: a.Expires.UTC().Format(time.RFC3339)}
		if req.Title == "" {
			req.Title = "Capcom wants to use " + a.Tool
		}
		if len(a.Input) <= 64<<10 {
			req.Input = a.Input
		} else {
			req.InputTruncated = true
		}
		return harness.Update{State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission, Request: req, Turn: ls.Turn}
	case ls.Turn != "":
		return harness.Update{State: api.AgentStatusWorking, Turn: ls.Turn}
	}
	return harness.Update{State: api.AgentStatusIdle}
}

// holds reports whether this hab holds the Capcom mark now.
func (e *capcomEngine) holds() bool { return e.svc.Home() == e.device }

// Session returns the service session's id, or "" while this hab does not run the service.
func (e *capcomEngine) Session() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session
}

// start makes sure the service session exists and runs the engine: this hab holds the mark.
//
// Mark changes call start and lose from goroutines, in any order: each takes markMu and acts
// on whether this hab holds the mark now, so two changes close together end in the right state.
func (e *capcomEngine) start(ctx context.Context) {
	e.markMu.Lock()
	defer e.markMu.Unlock()
	if !e.holds() {
		return
	}
	id, err := e.ensureSession()
	if err != nil {
		e.logf("capcom: the service session: %v", err)
		return
	}
	e.mu.Lock()
	e.session = id
	e.mu.Unlock()
	e.store.SetRoute(capcomRoute, id)
	e.syncState()
	e.sup.Ensure(ctx)
}

// lose stops the engine after its turn and renames the record capcom-earlier-<n>: this hab lost
// the mark (2.4). Its thread stays here, read-only.
func (e *capcomEngine) lose() {
	e.markMu.Lock()
	defer e.markMu.Unlock()
	if e.holds() {
		return
	}
	e.sup.Lose()
	e.mu.Lock()
	id := e.session
	e.session = ""
	e.mu.Unlock()
	e.syncState() // exited: the approvals area and push drop the route
	e.store.SetRoute(capcomRoute, "")
	if id == "" {
		return
	}
	for n := 1; ; n++ {
		name := fmt.Sprintf("capcom-earlier-%d", n)
		if _, taken := e.store.SessionByName(name); taken {
			continue
		}
		if _, err := e.store.RenameSession(id, name); err != nil {
			e.logf("capcom: rename the service session: %v", err)
		}
		return
	}
}

// ensureSession finds the service session (kind capcom, no pane, named capcom) or files it. A
// pane-era record that holds the name is renamed capcom-term-<n> first (2.4, 12.3).
func (e *capcomEngine) ensureSession() (string, error) {
	if ss, ok := e.store.SessionByName(capcomServiceName); ok {
		if ss.Kind == api.SessionKindCapcom && ss.Pane == "" {
			e.svc.addServiceRecord(ss.ID, capcomengine.EngineDir(e.dir))
			return ss.ID, nil
		}
		for n := 1; ; n++ {
			name := fmt.Sprintf("capcom-term-%d", n)
			if _, taken := e.store.SessionByName(name); taken {
				continue
			}
			if _, err := e.store.RenameSession(ss.ID, name); err != nil {
				return "", err
			}
			break
		}
	}
	ss, err := e.store.CreateSession(structure.SessionSpec{Name: capcomServiceName, NoPane: true, Index: -1,
		Kind: string(api.SessionKindCapcom), Dir: capcomengine.EngineDir(e.dir)})
	if err != nil {
		return "", err
	}
	e.svc.addServiceRecord(ss.ID, capcomengine.EngineDir(e.dir))
	return ss.ID, nil
}

// Accept reports whether a token is the running engine's (3.3), compared in constant time.
func (e *capcomEngine) Accept(token string) bool {
	cur := e.sup.Token()
	return cur != "" && len(token) == len(cur) && subtle.ConstantTimeCompare([]byte(token), []byte(cur)) == 1
}

// acceptPane reports whether a token is the running engine's pane route token, the one in its
// GFF_HAB: tool calls of its gff-hab mcp, as the Capcom session. It never opens the engine route.
func (e *capcomEngine) acceptPane(token string) bool {
	cur := e.sup.PaneToken()
	return cur != "" && len(token) == len(cur) && subtle.ConstantTimeCompare([]byte(token), []byte(cur)) == 1
}

// Serve runs one engine connection.
func (e *capcomEngine) Serve(c io.ReadWriteCloser) error { return e.link.Serve(c) }

// Pane resolves a route token: the engine's token names capcomRoute; any other goes to the pane
// tokens.
type engineTokens struct {
	panes hook.Resolver
	eng   *capcomEngine
}

func (t engineTokens) Pane(token string) (string, bool) {
	if t.eng != nil && t.eng.acceptPane(token) {
		return capcomRoute, true
	}
	return t.panes.Pane(token)
}

// Send stores a user message in the thread: delivered at once, read when the engine takes it
// (7.3). It is never refused as busy (7.4).
func (e *capcomEngine) Send(client string, p api.SessionSendParams) error {
	var images []string
	if len(p.Attachments) > 0 {
		if e.attached == nil {
			return control.Errorf(api.ErrorKindUnavailable, "session.send with attachments needs the attachment store, which this hab is running without")
		}
		var err error
		if images, err = e.attached(p.Session, p.Attachments); err != nil {
			return err
		}
	}
	key := ""
	if p.IdempotencyKey != "" {
		key = "send:" + p.IdempotencyKey
	}
	device := ""
	if client != "" {
		device = client
	}
	_, _, err := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorUser, Device: device},
		Text: p.Text, Images: images, Attachments: p.Attachments, Key: key})
	if err != nil {
		return control.Errorf(api.ErrorKindUnavailable, "capcom thread: %v", err)
	}
	return nil
}

// DeliverAgent stores a channel message to capcom as an agent item; the engine gets it as an
// input (2.4, 9.1).
func (e *capcomEngine) DeliverAgent(text string) error {
	_, _, err := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindAgent, Author: capcomthread.Author{Kind: capcomthread.AuthorAgent}, Text: text})
	return err
}

// Answer answers a Capcom approval card (5.3).
func (e *capcomEngine) Answer(client string, p api.ApprovalAnswerParams) error {
	switch p.Choice {
	case api.ApprovalChoiceAllow, api.ApprovalChoiceAllowSite, api.ApprovalChoiceDeny:
	default:
		return control.Errorf(api.ErrorKindInvalid, "a Capcom approval takes allow, allow_site or deny")
	}
	err := e.link.Answer(p.Request, string(p.Choice), client)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, capcomengine.ErrChoice):
		return control.Errorf(api.ErrorKindInvalid, "Capcom approval %s does not offer %s: allow_site is for a web fetch card with a URL", p.Request, p.Choice)
	case errors.Is(err, capcomengine.ErrNotFound), errors.Is(err, capcomthread.ErrConflict):
		return control.Errorf(api.ErrorKindConflict, "Capcom approval %s is not open: answered already, or expired", p.Request)
	}
	return control.Errorf(api.ErrorKindUnavailable, "Capcom approval %s: %v", p.Request, err)
}

// openApproval reports whether request is the engine's open approval.
func (e *capcomEngine) openApproval(request string) bool {
	a := e.link.Status().Approval
	return request != "" && a != nil && a.Request == request
}

// ThreadPage serves capcom.thread.page.
func (e *capcomEngine) ThreadPage(p api.CapcomThreadPageParams) (api.CapcomThreadPage, error) {
	items, more := e.thread.Page(p.Before, p.Limit)
	out := api.CapcomThreadPage{Items: make([]api.CapcomItem, 0, len(items)), More: more}
	for _, it := range items {
		out.Items = append(out.Items, apiItem(it))
	}
	return out, nil
}

// Interrupt serves capcom.interrupt.
func (e *capcomEngine) Interrupt() error {
	switch err := e.link.Interrupt(); {
	case errors.Is(err, capcomengine.ErrNoEngine):
		return control.Errorf(api.ErrorKindUnavailable, "no Capcom engine is connected")
	case errors.Is(err, capcomengine.ErrUnsupported):
		return control.Errorf(api.ErrorKindUnavailable, "the Capcom engine cannot interrupt a turn")
	case err != nil:
		return control.Errorf(api.ErrorKindUnavailable, "capcom.interrupt: %v", err)
	}
	return nil
}

// ServiceStatus serves capcom.status.
func (e *capcomEngine) ServiceStatus() (api.CapcomServiceStatus, error) {
	st, ls, rec := e.sup.Status(), e.link.Status(), e.rec.Get()
	out := api.CapcomServiceStatus{State: st.State, Reason: st.Reason, Session: e.Session(), Connected: ls.Connected && ls.Ready,
		Engine: ls.Engine, Version: ls.Version, Turn: ls.Turn, Folder: capcomengine.EngineDir(e.dir), SDKSessions: []api.CapcomSDKSession{}, Sites: rec.Sites}
	for _, s := range rec.Sessions {
		as := api.CapcomSDKSession{ID: s.ID, Start: s.Start.UTC().Format(time.RFC3339), Why: s.Why}
		if !s.End.IsZero() {
			as.End = s.End.UTC().Format(time.RFC3339)
		}
		out.SDKSessions = append(out.SDKSessions, as)
	}
	return out, nil
}

// RemoveSite serves capcom.site.remove (5.3).
func (e *capcomEngine) RemoveSite(site string) (api.CapcomServiceStatus, error) {
	if err := e.link.RemoveSite(site); err != nil {
		if errors.Is(err, capcomengine.ErrNotFound) {
			return api.CapcomServiceStatus{}, control.Errorf(api.ErrorKindNotFound, "Capcom has no allowed site %q", site)
		}
		return api.CapcomServiceStatus{}, control.Errorf(api.ErrorKindUnavailable, "capcom.site.remove: %v", err)
	}
	return e.ServiceStatus()
}

// Pause serves capcom.pause.
func (e *capcomEngine) Pause() (api.CapcomServiceStatus, error) {
	if e.Session() == "" {
		return api.CapcomServiceStatus{}, control.Errorf(api.ErrorKindUnavailable, "this hab does not hold the Capcom mark")
	}
	e.sup.Pause()
	return e.ServiceStatus()
}

// Resume serves capcom.resume.
func (e *capcomEngine) Resume(ctx context.Context) (api.CapcomServiceStatus, error) {
	if e.Session() == "" {
		return api.CapcomServiceStatus{}, control.Errorf(api.ErrorKindUnavailable, "this hab does not hold the Capcom mark")
	}
	e.sup.Resume(ctx)
	return e.ServiceStatus()
}

var _ control.CapcomThread = (*capcomEngine)(nil)

// apiItem is a thread item as the API sends it.
func apiItem(it capcomthread.Item) api.CapcomItem {
	out := api.CapcomItem{ID: it.ID, Kind: api.CapcomItemKind(it.Kind), Time: it.Time.UTC().Format(time.RFC3339Nano),
		Author: api.CapcomAuthor{Kind: api.CapcomAuthorKind(it.Author.Kind), Device: it.Author.Device, Address: it.Author.Address},
		Text:   it.Text, Images: it.Images, Attachments: it.Attachments, Turn: it.Turn, Event: it.Event, Summary: it.Summary}
	for _, c := range it.Context {
		out.Context = append(out.Context, api.CapcomContextItem{Kind: api.CapcomContextKind(c.Kind), Title: c.Title})
	}
	for _, t := range it.Tools {
		out.Tools = append(out.Tools, api.CapcomToolCall{Name: t.Name, Summary: t.Summary})
	}
	if it.Card != nil {
		out.Card = &api.CapcomCard{Type: it.Card.Type, Ref: it.Card.Ref, Choices: append([]string{}, it.Card.Choices...)}
	}
	if it.Answer != nil {
		out.Answer = &api.CapcomAnswer{Choice: it.Answer.Choice, By: it.Answer.By, Time: it.Answer.Time.UTC().Format(time.RFC3339)}
	}
	if !it.Read.IsZero() {
		out.Read = it.Read.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// engineAgents routes session.send and approval.answer for the service session to the thread
// and the engine; every other call goes on.
type engineAgents struct {
	control.Agents
	eng *capcomEngine
}

func (a engineAgents) Send(ctx context.Context, client string, p api.SessionSendParams) error {
	if id := a.eng.Session(); id != "" && p.Session == id {
		return a.eng.Send(client, p)
	}
	return a.Agents.Send(ctx, client, p)
}

func (a engineAgents) Answer(ctx context.Context, client string, p api.ApprovalAnswerParams) error {
	if p.Pane == capcomRoute || (p.Pane == "" && a.eng.openApproval(p.Request)) {
		return a.eng.Answer(client, p)
	}
	return a.Agents.Answer(ctx, client, p)
}

// SendsContext passes the question on to a Capcom wrapper underneath.
func (a engineAgents) SendsContext(session string, keyed bool) bool {
	cs, ok := a.Agents.(control.ContextSender)
	return ok && cs.SendsContext(session, keyed)
}

// engineJob picks the engine job for the init system (3.2): a transient unit of its own under
// systemd, so a restart of gff-hab's unit never ends it; a plain process in its own process
// group otherwise, which launchd's logic job leaves running (checked on macOS 27: a child in its
// own process group outlives launchctl kickstart -k and bootout of the job that started it).
func engineJob(sf *serviceFlags, state string) capcomengine.Job {
	if *sf.init != string(service.Systemd) {
		return engineJobFor(service.Launchd, "", state)
	}
	prefix := *sf.prefix
	if prefix == "" {
		prefix = os.Getenv("GFF_SERVICE_PREFIX")
	}
	if prefix == "" {
		prefix = service.DefaultPrefix
	}
	return engineJobFor(service.Systemd, prefix, state)
}

// engineJobFor is the engine job of an install: init system, job prefix, state directory.
func engineJobFor(in service.Init, prefix, state string) capcomengine.Job {
	if in != service.Systemd {
		return capcomengine.ProcessJob{PidFile: filepath.Join(state, capcomFolders, capcomengine.PidFile), Match: engineCommand}
	}
	return capcomengine.SystemdJob{Unit: prefix + "-gff-hab-capcom.service", EnvFile: filepath.Join(state, capcomFolders, capcomengine.EnvFile),
		Keep: []string{"PATH", "HOME", "LANG", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME"}}
}

// stopEngine ends the Capcom engine an install's gff-hab started, once the logic job is gone
// (gff-hab uninstall): the engine outlives gff-hab by design (3.2), so nothing else would. A
// plain process is adopted only when its command line is the engine's (engineCommand), so a pid
// reused since the pid file was written is left alone.
func stopEngine(ctx context.Context, in service.Init, prefix, state string, out io.Writer) {
	p, ok := engineJobFor(in, prefix, state).Adopt(ctx)
	if !ok {
		return
	}
	if err := p.Stop(10 * time.Second); err != nil {
		fmt.Fprintf(out, "the Capcom engine (pid %d) did not stop: %v\n", p.Pid(), err)
		return
	}
	fmt.Fprintf(out, "stopped the Capcom engine (pid %d)\n", p.Pid())
}

// engineCommand reports whether pid's command line names the Capcom engine: its file under
// capcom-engine/ or libexec/capcom/, or $GFF_HAB_CAPCOM_ENGINE.
func engineCommand(pid int) bool {
	b, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	cmd := string(b)
	if v := os.Getenv("GFF_HAB_CAPCOM_ENGINE"); v != "" && strings.Contains(cmd, v) {
		return true
	}
	return strings.Contains(cmd, "capcom-engine") || strings.Contains(cmd, "/capcom/")
}
