package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// pushService serves the push methods, session.focus and notice.raise (app.md 8, 17). With no
// push config, tokens and notifier are nil: the push.* methods answer unavailable, while focus
// reports and notices still work for live clients. With a config, push is on once the key
// command printed a key (keys); until then the service acts as with no config.
type pushService struct {
	store    *structure.Store
	agents   *harness.Store
	tokens   *push.Store
	notifier *push.Notifier
	keys     *push.Keyring // nil: the sender needs no key (tests)
	stop     func()        // ends the key loader
	publish  func(structure.Event)
	label    func(client string) string // the client's hello label
	now      func() time.Time
	notices  *noticeLog // the notices raised, for notice.get; nil: none kept
	denied   func(folder string) bool
}

var _ control.Push = (*pushService)(nil)

// pushSetup is what serve needs to build the push service.
type pushSetup struct {
	configPath string // the push config file; "" means <state>/push.json
	state      string
	hab        string // the hab's label
	self       string // the hab's device id
	// capcom returns the device id of the host that runs Capcom, or ""; nil: no Ask action
	// (capcom.md 8.5).
	capcom func() string
	// notices is the notice store; denied reports a folder the read scope denies. Both are set
	// before the notifier starts.
	notices   *noticeLog
	denied    func(folder string) bool
	getenv    func(string) string
	connected func(client string) bool
	label     func(client string) string
	publish   func(structure.Event)
	logf      func(string, ...any)
	// sender replaces the APNs client; for tests.
	sender push.Sender
}

// newPushService reads the push config and starts the notifier. Push not set up, or set up
// wrong, gives a service without push: the hab says so in its log and leaves the push
// capability out (app.md 8.4). It never stops gff-hab from starting, and never waits for the
// key command: that runs in the background, again with backoff while it fails, and push turns
// on when it prints a key.
func newPushService(ps pushSetup, store *structure.Store, agents *harness.Store) *pushService {
	s := &pushService{store: store, agents: agents, publish: ps.publish, label: ps.label, now: time.Now, notices: ps.notices, denied: ps.denied, stop: func() {}}
	path := ps.configPath
	if path == "" {
		path = filepath.Join(ps.state, push.ConfigFile)
	}
	cfg, configured, err := push.LoadConfig(path, ps.getenv)
	switch {
	case err != nil:
		ps.logf("push is off: %v (docs/push.md)", err)
		return s
	case !configured:
		ps.logf("push is not set up (no %s, no $%s): clients get live updates only (docs/push.md)", path, push.Env.KeyCommand)
		return s
	}
	sender := ps.sender
	if sender == nil {
		s.keys = push.NewKeyring(push.KeyCommand{Argv: cfg.KeyCommand}, nil, ps.logf)
		sender = push.NewClient(push.ClientConfig{Signer: push.NewSigner(nil, cfg.KeyID, cfg.TeamID, nil), Keys: s.keys, Topic: cfg.Topic})
	}
	tokens, err := push.OpenStore(filepath.Join(ps.state, "push"), nil)
	if err != nil {
		ps.logf("push is off: %v", err)
		return s
	}
	s.tokens = tokens
	s.notifier = push.NewNotifier(push.NotifierConfig{
		Sender: sender, Store: tokens, Hab: ps.hab, DefaultEnv: cfg.Environment,
		Where:     func(pane, session string) push.Where { return whereOf(store, pane, session) },
		Connected: ps.connected,
		Logf:      ps.logf,
		Capcom:    ps.capcom,
		Self:      ps.self,
		Keep:      s.keep,
	})
	on := func() {
		ps.logf("push is on: topic %s, %s gateway for tokens with no environment", cfg.Topic, cfg.Environment)
	}
	if s.keys == nil {
		on()
		return s
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.stop = func() { cancel(); <-done }
	go func() {
		defer close(done)
		for {
			if _, err := s.keys.Load(ctx); err == nil {
				on()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(max(s.keys.Next(), time.Second)):
			}
		}
	}()
	return s
}

// Close stops the key loader and the notifier; pushes it has not sent are dropped.
func (s *pushService) Close() {
	s.stop()
	if s.notifier != nil {
		s.notifier.Close()
	}
}

// forget tells the notifier a pane is gone.
func (s *pushService) forget(pane string) {
	if s != nil && s.notifier != nil {
		s.notifier.Forget(pane)
	}
}

// live returns the notifier while push is on, else nil.
func (s *pushService) live() *push.Notifier {
	if s == nil || !s.Enabled() {
		return nil
	}
	return s.notifier
}

// whereOf names the session and project of a pane, or of a session when pane is empty.
func whereOf(store *structure.Store, pane, session string) push.Where {
	var ss api.Session
	found := false
	if pane != "" {
		for _, s := range store.Snapshot(api.HostInfo{}).Sessions {
			if s.Pane == pane {
				ss, found = s, true
				break
			}
		}
	} else {
		ss, found = store.Session(session)
	}
	if !found {
		return push.Where{}
	}
	w := push.Where{Session: ss.ID, SessionName: ss.Name, SessionTitle: ss.Title}
	if p, ok := store.Project(ss.Project); ok {
		w.Project = p.Name
	}
	return w
}

// Enabled reports whether push is on: a valid config, and a key from the key command.
func (s *pushService) Enabled() bool {
	return s.notifier != nil && (s.keys == nil || s.keys.Key() != nil)
}

func (s *pushService) Register(client string, p api.PushRegisterParams) error {
	if !push.ValidToken(p.Token) {
		return control.Errorf(api.ErrorKindInvalid, "token is not an APNs token in hex")
	}
	kinds := make([]string, len(p.Kinds))
	for i, k := range p.Kinds {
		kinds[i] = string(k)
	}
	if err := s.tokens.RegisterWith(p.DeviceID, p.Token, string(p.Environment), kinds, p.HideCommands, p.Features); err != nil {
		return err
	}
	s.notifier.Bind(client, p.DeviceID)
	return nil
}

func (s *pushService) RegisterActivity(client string, p api.PushActivityParams) error {
	if !push.ValidToken(p.Token) {
		return control.Errorf(api.ErrorKindInvalid, "token is not an APNs token in hex")
	}
	if _, ok := s.store.Session(p.Session); !ok {
		return control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
	}
	if err := s.tokens.RegisterActivity(p.DeviceID, p.Session, p.Token); err != nil {
		return err
	}
	s.notifier.Bind(client, p.DeviceID)
	return nil
}

// Unregister drops the token. A token the hab does not hold is not an error: the app may
// unregister twice.
func (s *pushService) Unregister(client string, p api.PushUnregisterParams) error {
	_, err := s.tokens.Unregister(p.DeviceID, p.Token)
	return err
}

// Focus records the report for the presence tiers, and marks a done session seen.
func (s *pushService) Focus(client string, p api.SessionFocusParams) error {
	ss, ok := s.store.Session(p.Session)
	if !ok && p.Active {
		return control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
	}
	// active false for a session that is gone still clears the client's focus on it.
	if p.Active && ss.Pane != "" && s.agents != nil {
		s.agents.Seen(ss.Pane)
	}
	if n := s.live(); n != nil {
		n.Focus(client, p.Session, p.Active)
	}
	return nil
}

// Raise sends notice.raised to subscribers and, where push is set up, a push to each device
// the presence tiers let through.
func (s *pushService) Raise(client string, p api.NoticeRaiseParams) (api.NoticeRaised, error) {
	if p.Session != "" {
		if _, ok := s.store.Session(p.Session); !ok {
			return api.NoticeRaised{}, control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
		}
	}
	level := p.Level
	if level == "" {
		level = "active"
	}
	id := noticeID()
	n := api.Notice{ID: id, Title: p.Title, Body: p.Body, Level: level, Session: p.Session, At: s.now().UTC().Format(time.RFC3339)}
	if s.notices != nil {
		s.notices.add(n)
	}
	s.publish(structure.Event{Type: control.EventNoticeRaised, ID: id, Data: n})
	pushed := 0
	if n := s.live(); n != nil {
		pushed = n.Raise(push.Notice{ID: id, Title: p.Title, Body: p.Body, Level: push.Level(level), Session: p.Session})
	}
	return api.NoticeRaised{ID: id, Pushed: pushed}, nil
}

// raiseRound raises one round notification (rounds.md 10.2): the notice kept for notice.get
// and Capcom, a notice.raised event with the round named, and a push of kind round to the
// devices that registered it, collapsed by round id. The kept notice's body is full (with the
// run's result) when the read scope allows folder, else detail; empty ones fall back to the
// banner's body. It returns the notice id.
func (s *pushService) raiseRound(ev push.RoundEvent, detail, full, folder string) string {
	no := push.RoundNotice(ev)
	no.ID = noticeID()
	if full != "" && folder != "" && (s.denied == nil || !s.denied(folder)) {
		detail = full
	}
	if detail == "" {
		detail = no.Body
	}
	n := api.Notice{ID: no.ID, Title: no.Title, Body: detail, Level: api.NoticeLevel(no.Level), Session: no.Session,
		At:    s.now().UTC().Format(time.RFC3339),
		Round: &api.NoticeRound{Kind: api.RoundNoticeKind(ev.Kind), Round: ev.Round, Name: ev.Name, Run: ev.Run}}
	if s.notices != nil {
		s.notices.add(n)
	}
	s.publish(structure.Event{Type: control.EventNoticeRaised, ID: no.ID, Data: n})
	if n := s.live(); n != nil {
		n.Raise(no)
	}
	return no.ID
}

// noticeID is a new notice id: 16 hex digits.
func noticeID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// keep records an agent-state banner as a notice, so its Ask action names a notice that
// notice.get and Capcom's notification context resolve (capcom.md 8.5). A request whose folder
// the read scope denies is kept as hidden: no command. It raises no notice.raised event and no
// push of its own: the banner is the push, and clients have the agent state. It returns the
// notice id, or "" with no store.
func (s *pushService) keep(full, hidden push.Notice) string {
	if s.notices == nil {
		return ""
	}
	no := full
	if s.denied != nil && s.denied(full.Folder) {
		no = hidden
	}
	id := noticeID()
	s.notices.add(api.Notice{ID: id, Title: no.Title, Body: no.Body, Level: api.NoticeLevel(no.Level), Session: no.Session, At: s.now().UTC().Format(time.RFC3339)})
	return id
}

// answering tells the notifier an approval.answer is on its way, so the banner it withdraws
// says which client answered; done reports whether it went out.
func (s *pushService) answering(pane, request, client string, allow bool) (done func(ok bool)) {
	if s.live() == nil {
		return func(bool) {}
	}
	by := s.label(client)
	if by == "" {
		by = "another device"
	}
	s.notifier.Answering(pane, request, by, allow)
	return func(ok bool) {
		if ok {
			s.notifier.Answered(pane, request, allow)
		} else {
			s.notifier.Unanswering(pane, request)
		}
	}
}

// notifyCmd raises a notice through the local socket: gff-hab notify [flags] <title> [body].
func notifyCmd(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	level := fs.String("level", "active", "passive, active or time-sensitive")
	session := fs.String("session", "", "the session the notice is about")
	timeout := fs.Duration("timeout", 10*time.Second, "give up after this long")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("notify needs a title and an optional body")
	}
	p := api.NoticeRaiseParams{Title: fs.Arg(0), Body: fs.Arg(1), Level: api.NoticeLevel(*level), Session: *session}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, scriptClient, "", ""); err != nil {
		return err
	}
	res, err := api.Client{Caller: c}.NoticeRaise(ctx, p)
	if err != nil {
		return err
	}
	return printJSON(res)
}

var scriptClient = api.ClientInfo{Name: "gff-hab notify", Version: build, Kind: api.ClientKindScript}
