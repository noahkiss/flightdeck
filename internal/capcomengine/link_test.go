package capcomengine

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// fakeClock is a clock with timers the test fires by moving time.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at   time.Time
	f    func()
	done bool
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) After(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.done
		t.done = true
		return was
	}
}

// Advance moves time and fires each timer due, outside the lock.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.done && !t.at.After(c.t) {
			t.done = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// fakeEngine is the engine end of one connection.
type fakeEngine struct {
	t   *testing.T
	c   net.Conn
	r   *bufio.Reader
	seq uint64
	in  chan Message
}

func (e *fakeEngine) send(m Message) {
	e.t.Helper()
	e.seq++
	m.Seq = e.seq
	b, _ := json.Marshal(m)
	if _, err := e.c.Write(append(b, '\n')); err != nil {
		e.t.Fatal(err)
	}
}

// next returns the next message of the hab that is not an ack.
func (e *fakeEngine) next() Message {
	e.t.Helper()
	select {
	case m, ok := <-e.in:
		if !ok {
			e.t.Fatal("the hab closed the connection")
		}
		return m
	case <-time.After(5 * time.Second):
		e.t.Fatal("no message from the hab")
	}
	return Message{}
}

// acked waits for the hab's ack of seq n.
func (e *fakeEngine) acked(n uint64) {
	e.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			e.t.Fatalf("no ack of %d", n)
		default:
		}
		if e.lastAck() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

var ackMu sync.Mutex

func (e *fakeEngine) lastAck() uint64 { ackMu.Lock(); defer ackMu.Unlock(); return acks[e] }

var acks = map[*fakeEngine]uint64{}

type harnessT struct {
	t      *testing.T
	clock  *fakeClock
	thread *capcomthread.Store
	rec    *Store
	link   *Link
	errs   chan string
}

func newHarness(t *testing.T) *harnessT { return newHarnessWith(t, nil) }

// newHarnessWith lets a test change the link's config before the link starts.
func newHarnessWith(t *testing.T, edit func(*LinkConfig)) *harnessT {
	t.Helper()
	dir := t.TempDir()
	clock := newClock()
	th, _, err := capcomthread.Open(dir+"/thread", clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harnessT{t: t, clock: clock, thread: th, rec: rec, errs: make(chan string, 10)}
	cfg := LinkConfig{Thread: th, Record: rec, Now: clock.Now, After: clock.After,
		Hello: func() (Message, bool) {
			return Message{Credential: &Credential{Kind: "oauth_token", Value: "planted-token"}, Device: "dev-hab", Settings: &Settings{MaxTurns: 50}}, true
		},
		ApprovalMaxAge: func() time.Duration { return 2 * time.Minute },
		OnError:        func(kind, text string, exiting bool) { h.errs <- kind + ":" + text },
	}
	if edit != nil {
		edit(&cfg)
	}
	h.link = NewLink(cfg)
	return h
}

// connect serves one connection and returns the engine end after hello.
func (h *harnessT) connect() (*fakeEngine, Message) {
	h.t.Helper()
	a, b := net.Pipe()
	go h.link.Serve(a)
	e := &fakeEngine{t: h.t, c: b, r: bufio.NewReader(b), in: make(chan Message, 100)}
	go func() {
		defer close(e.in)
		for {
			line, err := e.r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m Message
			json.Unmarshal(line, &m)
			if m.Type == TAck {
				ackMu.Lock()
				acks[e] = m.Ack
				ackMu.Unlock()
				continue
			}
			e.in <- m
		}
	}()
	hello := e.next()
	if hello.Type != THello {
		h.t.Fatalf("first message %+v", hello)
	}
	return e, hello
}

func (h *harnessT) user(text string) capcomthread.Item {
	h.t.Helper()
	it, _, err := h.thread.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorUser, Device: "d1"}, Text: text})
	if err != nil {
		h.t.Fatal(err)
	}
	return it
}

// claudeCaps are the Claude engine's capabilities (4.1).
var claudeCaps = []string{CapImages, CapInterrupt, CapMidTurnInput, CapApprovalCallback, CapCompactionEvents, CapUsageEvents, CapResumeByID}

func readyMsg() Message {
	return Message{Type: TReady, Engine: "claude", Version: "test", Capabilities: claudeCaps}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func inputID(m Message) string {
	var in InputItem
	json.Unmarshal(m.Item, &in)
	return in.ID
}

// Two messages sent during a turn are handed in order and each marked read when taken; a reply
// is stored before its ack, and a repeated reply is dropped (4.3, 7.3).
func TestLinkDeliveryOrderReadAndReplies(t *testing.T) {
	h := newHarness(t)
	early := h.user("before the engine")
	e, hello := h.connect()
	if hello.Credential == nil || hello.Credential.Value != "planted-token" || hello.LastItem != early.ID {
		t.Fatalf("hello %+v", hello)
	}
	if got := hello.Credential.String(); strings.Contains(got, "planted") {
		t.Fatalf("Credential.String shows the value: %s", got)
	}
	// Nothing is sent before ready.
	select {
	case m := <-e.in:
		t.Fatalf("sent before ready: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
	e.send(readyMsg())
	if m := e.next(); m.Type != TInput || inputID(m) != early.ID {
		t.Fatalf("first input %+v", m)
	}
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user", Items: []string{early.ID}})
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + early.ID + `"`)})
	a, b := h.user("one"), h.user("two")
	if m := e.next(); inputID(m) != a.ID {
		t.Fatalf("got %s, want %s", inputID(m), a.ID)
	}
	if m := e.next(); inputID(m) != b.ID {
		t.Fatalf("got %s, want %s", inputID(m), b.ID)
	}
	waitFor(t, "working and typing", func() bool { s := h.link.Status(); return s.Turn == "t1" && s.Typing })
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + a.ID + `"`)})
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + b.ID + `"`)})
	e.send(Message{Type: TReply, Turn: StrTurn("t1"), Part: 0, Text: "hello"})
	e.send(Message{Type: TReply, Turn: StrTurn("t1"), Part: 0, Text: "hello"}) // a resend
	e.send(Message{Type: TTurnEnded, Turn: StrTurn("t1"), Result: ResultSuccess})
	e.acked(e.seq)
	for _, id := range []string{early.ID, a.ID, b.ID} {
		if it, _ := h.thread.Get(id); it.Read.IsZero() {
			t.Fatalf("%s not read", id)
		}
	}
	if h.rec.Get().Taken != b.ID {
		t.Fatalf("taken %s", h.rec.Get().Taken)
	}
	page, _ := h.thread.Page("", 0)
	replies := 0
	for _, it := range page {
		if it.Kind == capcomthread.KindMessage && it.Author.Kind == capcomthread.AuthorCapcom {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("%d replies stored", replies)
	}
	if s := h.link.Status(); s.Turn != "" || s.Typing {
		t.Fatalf("after the turn: %+v", s)
	}
}

// After a drop, the new connection gets every item past the last taken one again; items the
// engine took are not sent again.
func TestLinkReconnectResendsUntaken(t *testing.T) {
	h := newHarness(t)
	e, _ := h.connect()
	e.send(readyMsg())
	a := h.user("a")
	if m := e.next(); inputID(m) != a.ID {
		t.Fatal("a not sent")
	}
	e.send(Message{Type: TInputTaken, Item: json.RawMessage(`"` + a.ID + `"`)})
	b := h.user("b")
	if m := e.next(); inputID(m) != b.ID {
		t.Fatal("b not sent")
	}
	e.acked(e.seq)
	e.c.Close()
	waitFor(t, "offline", func() bool { return !h.link.Status().Connected })
	e2, hello := h.connect()
	if hello.LastItem != b.ID {
		t.Fatalf("hello last item %s", hello.LastItem)
	}
	e2.send(Message{Type: TReady, Engine: "claude", Capabilities: claudeCaps, Turn: json.RawMessage(`{"turn":"t9","origin":"user"}`)})
	if m := e2.next(); inputID(m) != b.ID {
		t.Fatalf("resend got %s, want %s", inputID(m), b.ID)
	}
	waitFor(t, "the running turn from ready", func() bool { return h.link.Status().Turn == "t9" })
}

// An approval opens one card; the first answer wins and reaches the engine; one left alone
// expires at capcom.approval_max_age and is denied with "the user did not answer" (5.3).
func TestLinkApprovals(t *testing.T) {
	h := newHarness(t)
	e, _ := h.connect()
	e.send(readyMsg())
	e.send(Message{Type: TApprovalRequest, Request: "r1", Tool: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com/x"}`), Reason: "Capcom wants to open example.com/x"})
	e.send(Message{Type: TApprovalRequest, Request: "r1", Tool: "WebFetch"}) // asked again
	e.acked(e.seq)
	st := h.link.Status()
	if st.Approval == nil || st.Approval.Request != "r1" || !st.Approval.Expires.Equal(h.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("approval %+v", st.Approval)
	}
	if err := h.link.Answer("r1", ChoiceAllow, "dev1"); err != nil {
		t.Fatal(err)
	}
	if m := e.next(); m.Type != TApprovalAnswer || m.Request != "r1" || m.Allow == nil || !*m.Allow {
		t.Fatalf("answer %+v", m)
	}
	if err := h.link.Answer("r1", ChoiceDeny, "dev2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second answer: %v", err)
	}
	e.send(Message{Type: TApprovalRequest, Request: "r2", Tool: "WebFetch"})
	e.acked(e.seq)
	h.clock.Advance(119 * time.Second)
	if h.link.Status().Approval == nil {
		t.Fatal("expired early")
	}
	h.clock.Advance(time.Second)
	m := e.next()
	if m.Type != TApprovalAnswer || m.Request != "r2" || *m.Allow || m.Message != "the user did not answer" {
		t.Fatalf("expiry %+v", m)
	}
	cards := 0
	page, _ := h.thread.Page("", 0)
	for _, it := range page {
		if it.Kind == capcomthread.KindCard {
			cards++
			if it.Card.Ref == "r2" && (it.Answer == nil || it.Answer.Choice != "expired") {
				t.Fatalf("r2 card %+v", it.Answer)
			}
		}
	}
	if cards != 2 {
		t.Fatalf("%d cards", cards)
	}
	// The engine asks again for an answered request after a reconnect: it gets the answer.
	e.send(Message{Type: TApprovalRequest, Request: "r1", Tool: "WebFetch"})
	if m := e.next(); m.Type != TApprovalAnswer || m.Request != "r1" || !*m.Allow {
		t.Fatalf("re-ask %+v", m)
	}
}

// "Allow this site" (5.3): a WebFetch card with a URL offers it; the answer allows the fetch,
// keeps the site, hands the engine the list before the answer, and the next hello carries it.
// A card with no URL does not offer it; RemoveSite takes it off and tells the engine.
func TestLinkAllowSite(t *testing.T) {
	h := newHarness(t)
	e, hello := h.connect()
	if len(hello.Sites) != 0 {
		t.Fatalf("hello sites %v", hello.Sites)
	}
	e.send(readyMsg())
	e.send(Message{Type: TApprovalRequest, Request: "r1", Tool: "WebFetch", Input: json.RawMessage(`{"url":"https://WWW.Example.com:8443/menu"}`)})
	e.send(Message{Type: TApprovalRequest, Request: "r2", Tool: "WebFetch", Input: json.RawMessage(`{"prompt":"x"}`)})
	e.acked(e.seq)
	choices := map[string][]string{}
	page, _ := h.thread.Page("", 0)
	for _, it := range page {
		if it.Card != nil {
			choices[it.Card.Ref] = it.Card.Choices
		}
	}
	if got := strings.Join(choices["r1"], ","); got != "allow,allow_site,deny" {
		t.Fatalf("r1 choices %s", got)
	}
	if got := strings.Join(choices["r2"], ","); got != "allow,deny" {
		t.Fatalf("r2 choices %s", got)
	}
	if err := h.link.Answer("r2", ChoiceAllowSite, "dev1"); !errors.Is(err, ErrChoice) {
		t.Fatalf("allow_site on a card with no URL: %v", err)
	}
	if err := h.link.Answer("r1", "maybe", "dev1"); !errors.Is(err, ErrChoice) {
		t.Fatalf("unknown choice: %v", err)
	}
	if err := h.link.Answer("r1", ChoiceAllowSite, "dev1"); err != nil {
		t.Fatal(err)
	}
	if m := e.next(); m.Type != TSites || strings.Join(m.Sites, ",") != "example.com" {
		t.Fatalf("sites %+v", m)
	}
	if m := e.next(); m.Type != TApprovalAnswer || m.Request != "r1" || !*m.Allow {
		t.Fatalf("answer %+v", m)
	}
	if got := h.link.Sites(); strings.Join(got, ",") != "example.com" {
		t.Fatalf("kept %v", got)
	}
	e.c.Close()
	waitFor(t, "offline", func() bool { return !h.link.Status().Connected })
	e2, hello2 := h.connect()
	if strings.Join(hello2.Sites, ",") != "example.com" {
		t.Fatalf("hello after reconnect %v", hello2.Sites)
	}
	e2.send(readyMsg())
	if err := h.link.RemoveSite("www.EXAMPLE.com"); err != nil {
		t.Fatal(err)
	}
	for {
		m := e2.next()
		if m.Type == TSites {
			if len(m.Sites) != 0 {
				t.Fatalf("after remove %v", m.Sites)
			}
			break
		}
	}
	// The engine names the site (an IDN in punycode): that is what is kept.
	e2.send(Message{Type: TApprovalRequest, Request: "r3", Tool: "WebFetch", Site: "xn--bcher-kva.de", Input: json.RawMessage(`{"url":"https://bücher.de/x"}`)})
	waitFor(t, "r3 open", func() bool { a := h.link.Status().Approval; return a != nil && a.Request == "r3" })
	if err := h.link.Answer("r3", ChoiceAllowSite, "dev1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the engine's site kept", func() bool { return strings.Join(h.link.Sites(), ",") == "xn--bcher-kva.de" })
	if err := h.link.RemoveSite("xn--bcher-kva.de"); err != nil {
		t.Fatal(err)
	}
	if err := h.link.RemoveSite("example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	if len(h.rec.Get().Sites) != 0 {
		t.Fatal("record keeps the site")
	}
}

func TestLinkInterruptShutdownAndErrors(t *testing.T) {
	h := newHarness(t)
	if err := h.link.Interrupt(); !errors.Is(err, ErrNoEngine) {
		t.Fatal(err)
	}
	e, _ := h.connect()
	e.send(Message{Type: TReady, Engine: "claude"})
	e.acked(e.seq)
	if err := h.link.Interrupt(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("no interrupt capability: %v", err)
	}
	e.send(readyMsg())
	e.acked(e.seq)
	if err := h.link.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if m := e.next(); m.Type != TInterrupt {
		t.Fatalf("%+v", m)
	}
	if !h.link.Shutdown() {
		t.Fatal("shutdown not sent")
	}
	if m := e.next(); m.Type != TShutdown {
		t.Fatalf("%+v", m)
	}
	e.send(Message{Type: TError, Kind: ErrKindAuth, Text: "refused", Exiting: true})
	if got := <-h.errs; got != "auth:refused" {
		t.Fatal(got)
	}
	e.send(Message{Type: TSessionStarted, Session: "s1", Why: "first"})
	e.send(Message{Type: TSessionStarted, Session: "s2", Why: "new"})
	e.acked(e.seq)
	r := h.rec.Get()
	if len(r.Sessions) != 2 || r.Current() != "s2" || r.Sessions[0].End.IsZero() {
		t.Fatalf("sessions %+v", r.Sessions)
	}
	page, _ := h.thread.Page("", 0)
	if len(page) != 1 || page[0].Kind != capcomthread.KindDivider || page[0].Text != "New conversation" {
		t.Fatalf("divider %+v", page)
	}
}

// Typing stops after typing_timeout with no engine message (7.3).
func TestLinkTypingTimeout(t *testing.T) {
	h := newHarness(t)
	e, _ := h.connect()
	e.send(readyMsg())
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t1"), Origin: "user"})
	e.acked(e.seq)
	if !h.link.Status().Typing {
		t.Fatal("not typing")
	}
	h.clock.Advance(9 * time.Minute)
	e.send(Message{Type: TTool, Turn: StrTurn("t1"), Name: "WebSearch", Summary: "searched the web"})
	e.acked(e.seq)
	h.clock.Advance(9 * time.Minute)
	if !h.link.Status().Typing {
		t.Fatal("an engine message did not restart the timeout")
	}
	h.clock.Advance(time.Minute)
	if h.link.Status().Typing {
		t.Fatal("typing past its timeout")
	}
	// An event turn shows no typing.
	e.send(Message{Type: TTurnStarted, Turn: StrTurn("t2"), Origin: "event"})
	e.send(Message{Type: TReply, Turn: StrTurn("t2"), Text: "wrote first"})
	e.acked(e.seq)
	if h.link.Status().Typing {
		t.Fatal("typing in an event turn")
	}
	page, _ := h.thread.Page("", 1)
	if !page[0].Event {
		t.Fatal("an event turn's reply is not marked")
	}
}
