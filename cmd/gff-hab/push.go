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
// reports and notices still work for live clients.
type pushService struct {
	store    *structure.Store
	agents   *harness.Store
	tokens   *push.Store
	notifier *push.Notifier
	publish  func(structure.Event)
	label    func(client string) string // the client's hello label
	now      func() time.Time
}

var _ control.Push = (*pushService)(nil)

// pushSetup is what serve needs to build the push service.
type pushSetup struct {
	configPath string // the push config file; "" means <state>/push.json
	state      string
	hab        string // the hab's label
	getenv     func(string) string
	connected  func(client string) bool
	label      func(client string) string
	publish    func(structure.Event)
	logf       func(string, ...any)
	// sender replaces the APNs client; for tests.
	sender push.Sender
}

// newPushService reads the push config and starts the notifier. Push not set up, or set up
// wrong, gives a service without push: the hab says so in its log and leaves the push
// capability out (app.md 8.4). It never stops gff-hab from starting.
func newPushService(ps pushSetup, store *structure.Store, agents *harness.Store) *pushService {
	s := &pushService{store: store, agents: agents, publish: ps.publish, label: ps.label, now: time.Now}
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
		ps.logf("push is not set up (no %s, no $%s): clients get live updates only (docs/push.md)", path, push.Env.KeyFile)
		return s
	}
	sender := ps.sender
	if sender == nil {
		key, err := cfg.Key()
		if err != nil {
			ps.logf("push is off: %v (docs/push.md)", err)
			return s
		}
		sender = push.NewClient(push.ClientConfig{Signer: push.NewSigner(key, cfg.KeyID, cfg.TeamID, nil), Topic: cfg.Topic})
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
	})
	ps.logf("push is on: topic %s, %s gateway for tokens with no environment", cfg.Topic, cfg.Environment)
	return s
}

// Close stops the notifier; pushes it has not sent are dropped.
func (s *pushService) Close() {
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

func (s *pushService) Enabled() bool { return s.notifier != nil }

func (s *pushService) Register(client string, p api.PushRegisterParams) error {
	if !push.ValidToken(p.Token) {
		return control.Errorf(api.ErrorKindInvalid, "token is not an APNs token in hex")
	}
	kinds := make([]string, len(p.Kinds))
	for i, k := range p.Kinds {
		kinds[i] = string(k)
	}
	if err := s.tokens.Register(p.DeviceID, p.Token, string(p.Environment), kinds, p.HideCommands); err != nil {
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
	if !ok {
		return control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
	}
	if p.Active && ss.Pane != "" && s.agents != nil {
		s.agents.Seen(ss.Pane)
	}
	if s.notifier != nil {
		s.notifier.Focus(client, p.Session, p.Active)
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
	var b [8]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	n := api.Notice{ID: id, Title: p.Title, Body: p.Body, Level: level, Session: p.Session, At: s.now().UTC().Format(time.RFC3339)}
	s.publish(structure.Event{Type: control.EventNoticeRaised, ID: id, Data: n})
	pushed := 0
	if s.notifier != nil {
		pushed = s.notifier.Raise(push.Notice{ID: id, Title: p.Title, Body: p.Body, Level: push.Level(level), Session: p.Session})
	}
	return api.NoticeRaised{ID: id, Pushed: pushed}, nil
}

// answering tells the notifier an approval.answer is on its way, so the banner it withdraws
// says which client answered; done reports whether it went out.
func (s *pushService) answering(pane, request, client string, allow bool) (done func(ok bool)) {
	if s == nil || s.notifier == nil {
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
