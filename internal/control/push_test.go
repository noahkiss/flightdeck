package control

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakePush records the calls the push methods make.
type fakePush struct {
	enabled bool
	mu      sync.Mutex
	calls   []string
}

func (f *fakePush) Enabled() bool { return f.enabled }
func (f *fakePush) record(s string) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	return nil
}
func (f *fakePush) Register(client string, p api.PushRegisterParams) error {
	return f.record("register " + p.DeviceID + " " + p.Token)
}
func (f *fakePush) RegisterActivity(client string, p api.PushActivityParams) error {
	return f.record("activity " + p.Session)
}
func (f *fakePush) Unregister(client string, p api.PushUnregisterParams) error {
	if p.Token == "unknown" {
		return Errorf(api.ErrorKindNotFound, "no such token")
	}
	return f.record("unregister " + p.Token)
}
func (f *fakePush) Focus(client string, p api.SessionFocusParams) error {
	if p.Active {
		return f.record("focus " + p.Session)
	}
	return f.record("blur " + p.Session)
}
func (f *fakePush) Raise(client string, p api.NoticeRaiseParams) (api.NoticeRaised, error) {
	f.record("raise " + p.Title)
	return api.NoticeRaised{ID: "n1", Pushed: 2}, nil
}

func errKindOf(err error) api.ErrorKind {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

// TestPushMethods: without push config the push.* methods answer unavailable and name the
// setup guide, while session.focus and notice.raise work; with it, calls reach Push after the
// word sets are checked.
func TestPushMethods(t *testing.T) {
	fp := &fakePush{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Push = fp })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	reg := api.PushRegisterParams{DeviceID: "d1", Token: "abcd", Environment: "production", Kinds: []api.PushKind{"permission"}}
	_, err := cl.PushRegister(ctx(t), reg)
	var ce *Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable || !strings.Contains(ce.Data.Hint, "docs/push.md") {
		t.Fatalf("push.register without config: %v", err)
	}
	if _, err := cl.SessionFocus(ctx(t), api.SessionFocusParams{Session: "s1", Active: true}); err != nil {
		t.Fatalf("session.focus without config: %v", err)
	}
	res, err := cl.NoticeRaise(ctx(t), api.NoticeRaiseParams{Title: "Backup done"})
	if err != nil || res.ID != "n1" || res.Pushed != 2 {
		t.Fatalf("notice.raise: %+v %v", res, err)
	}

	fp.enabled = true
	if _, err := cl.PushRegister(ctx(t), reg); err != nil {
		t.Fatalf("push.register: %v", err)
	}
	bad := reg
	bad.Environment = "staging"
	if _, err := cl.PushRegister(ctx(t), bad); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("bad environment: %v", err)
	}
	bad = reg
	bad.Kinds = []api.PushKind{"everything"}
	if _, err := cl.PushRegister(ctx(t), bad); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("bad kind: %v", err)
	}
	if _, err := cl.PushRegisterActivity(ctx(t), api.PushActivityParams{DeviceID: "d1", Session: "s1", Token: "beef"}); err != nil {
		t.Errorf("push.register_activity: %v", err)
	}
	if _, err := cl.PushRegisterActivity(ctx(t), api.PushActivityParams{DeviceID: "d1", Token: "beef"}); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("activity with no session: %v", err)
	}
	if _, err := cl.PushUnregister(ctx(t), api.PushUnregisterParams{DeviceID: "d1", Token: "unknown"}); errKindOf(err) != api.ErrorKindNotFound {
		t.Errorf("unregister keeps the kind: %v", err)
	}
	if _, err := cl.SessionFocus(ctx(t), api.SessionFocusParams{}); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("focus with no session: %v", err)
	}
	if _, err := cl.NoticeRaise(ctx(t), api.NoticeRaiseParams{Title: "x", Level: "loud"}); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("bad level: %v", err)
	}
	if _, err := cl.NoticeRaise(ctx(t), api.NoticeRaiseParams{}); errKindOf(err) != api.ErrorKindInvalid {
		t.Errorf("no title: %v", err)
	}
	fp.mu.Lock()
	got := strings.Join(fp.calls, "; ")
	fp.mu.Unlock()
	if want := "focus s1; raise Backup done; register d1 abcd; activity s1"; got != want {
		t.Errorf("calls %q, want %q", got, want)
	}
}

// TestPushWithoutService: a hab with no Push answers unavailable.
func TestPushWithoutService(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	if _, err := cl.SessionFocus(ctx(t), api.SessionFocusParams{Session: "s"}); errKindOf(err) != api.ErrorKindUnavailable {
		t.Errorf("session.focus: %v", err)
	}
}

// TestClientPresence: Connected and ClientLabel follow a client's connections.
func TestClientPresence(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	c := dial(t, h.path)
	w, err := c.Hello(ctx(t), api.ClientInfo{Name: "Flightdeck", Version: "1", Kind: api.ClientKindApp}, "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	if !h.srv.Connected(w.ClientID) || h.srv.ClientLabel(w.ClientID) != "phone" {
		t.Fatalf("connected %v label %q", h.srv.Connected(w.ClientID), h.srv.ClientLabel(w.ClientID))
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for h.srv.Connected(w.ClientID) {
		if time.Now().After(deadline) {
			t.Fatal("still connected after close")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.srv.ClientLabel(w.ClientID) != "" {
		t.Error("a closed client kept its label")
	}
}

// TestNoticeScope: a notice about a session reaches the subscriptions that see the session; a
// notice about no session reaches every subscription.
func TestNoticeScope(t *testing.T) {
	f := newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s1"}})
	ev := func(session string) structure.Event {
		return structure.Event{Type: EventNoticeRaised, ID: "n", Data: api.Notice{ID: "n", Title: "t", Level: "active", Session: session}}
	}
	if !f.pass(ev("s1")) || f.pass(ev("s2")) || !f.pass(ev("")) {
		t.Error("notice scope")
	}
	if !structure.Ephemeral(EventNoticeRaised) {
		t.Error("notice.raised is not ephemeral")
	}
}
