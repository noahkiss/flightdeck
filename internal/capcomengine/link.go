package capcomengine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// Status is what the link knows of the engine now. The session record's agent state comes
// from it (7.6: idle, working, blocked, offline, error).
type Status struct {
	Connected    bool
	Ready        bool
	Engine       string
	Version      string
	SDKVersion   string
	Capabilities []string
	Turn         string // the running turn; "": idle
	Origin       string // user or event
	Typing       bool
	Approval     *Approval // the open approval, or nil
	Draining     bool      // shutdown was sent
}

// Has reports whether the engine declared a capability.
func (s Status) Has(c string) bool { return slices.Contains(s.Capabilities, c) }

// Approval is an open approval request from canUseTool (5.3).
type Approval struct {
	Request string
	Tool    string
	Input   json.RawMessage
	Reason  string
	Item    string // the card item in the thread
	Opened  time.Time
	Expires time.Time
}

// LinkConfig wires a link.
type LinkConfig struct {
	Thread *capcomthread.Store
	Record *Store
	// Hello builds the hello message: the credential, the device, the settings. ok false
	// refuses the connection (no credential).
	Hello func() (m Message, ok bool)
	// ApprovalMaxAge is capcom.approval_max_age (19.1), read when a request opens.
	ApprovalMaxAge func() time.Duration
	// TypingTimeout is [capcom] typing_timeout (7.3).
	TypingTimeout func() time.Duration
	// After runs f after d; the returned stop cancels it. Nil: time.AfterFunc.
	After func(d time.Duration, f func()) (stop func() bool)
	Now   func() time.Time
	// OnStatus hears every status change, with no lock held.
	OnStatus func(Status)
	// OnError hears each error the engine reports (an auth refusal, the start check).
	OnError func(kind, text string, exiting bool)
	// OnReady hears each ready; OnTurnEnded each turn's result.
	OnReady     func()
	OnTurnEnded func(result string)
	Logf        func(string, ...any)
}

// Link is gff-hab's side of one engine: one connection at a time, the delivery of thread items,
// and what the engine sends back.
type Link struct {
	cfg LinkConfig

	mu       sync.Mutex
	cur      *conn
	st       Status
	approval map[string]*openApproval
	typingT  func() bool
	kick     chan struct{}
}

type openApproval struct {
	Approval
	stop func() bool
}

type conn struct {
	rw    io.ReadWriteCloser
	wmu   sync.Mutex
	w     *bufio.Writer
	seq   uint64
	sent  string // the newest item id sent on this connection
	ready bool
	done  chan struct{}
}

// NewLink returns a link.
func NewLink(cfg LinkConfig) *Link {
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.ApprovalMaxAge == nil {
		cfg.ApprovalMaxAge = func() time.Duration { return 10 * time.Minute }
	}
	if cfg.TypingTimeout == nil {
		cfg.TypingTimeout = func() time.Duration { return 10 * time.Minute }
	}
	l := &Link{cfg: cfg, approval: map[string]*openApproval{}, kick: make(chan struct{}, 1)}
	cfg.Thread.Watch(func(it capcomthread.Item) {
		if it.ForModel() {
			l.wake()
		}
	})
	go l.pump()
	return l
}

func (l *Link) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Status returns the engine's status.
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.statusLocked()
}

func (l *Link) statusLocked() Status {
	s := l.st
	s.Capabilities = slices.Clone(l.st.Capabilities)
	for _, a := range l.approval {
		c := a.Approval
		s.Approval = &c
		break
	}
	return s
}

func (l *Link) changed() {
	if l.cfg.OnStatus != nil {
		l.cfg.OnStatus(l.Status())
	}
}

// ErrNoEngine means no engine is connected.
var ErrNoEngine = errors.New("capcom: no engine is connected")

// ErrUnsupported means the engine lacks the capability a call needs.
var ErrUnsupported = errors.New("capcom: the engine lacks that capability")

// Serve runs one engine connection until it closes. A newer connection replaces it. The hello
// goes first; nothing else is sent until the engine answers ready.
func (l *Link) Serve(rw io.ReadWriteCloser) error {
	hello, ok := l.cfg.Hello()
	if !ok {
		rw.Close()
		return errors.New("capcom: no credential to hand the engine")
	}
	c := &conn{rw: rw, w: bufio.NewWriter(rw), done: make(chan struct{})}
	l.mu.Lock()
	old := l.cur
	l.cur = c
	l.st.Connected, l.st.Ready, l.st.Draining = true, false, false
	l.mu.Unlock()
	if old != nil {
		old.rw.Close()
	}
	l.changed()
	rec := l.cfg.Record.Get()
	hello.Type = THello
	hello.LastItem = l.cfg.Thread.Last()
	hello.SDKSession = rec.Current()
	err := l.send(c, hello)
	if err == nil {
		err = l.read(c)
	}
	rw.Close()
	close(c.done)
	l.mu.Lock()
	if l.cur == c {
		l.cur = nil
		l.st = Status{}
		if l.typingT != nil {
			l.typingT()
			l.typingT = nil
		}
	}
	l.mu.Unlock()
	l.changed()
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}

func (l *Link) send(c *conn, m Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if m.Type != TAck {
		c.seq++
		m.Seq = c.seq
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

func (l *Link) read(c *conn) error {
	r := bufio.NewReaderSize(c.rw, 64<<10)
	for {
		line, err := readLine(r)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("capcom engine: a line that is not JSON: %v", err)
		}
		if m.Type == TAck {
			continue
		}
		l.handle(c, m)
		if m.Seq > 0 {
			if err := l.send(c, Message{Type: TAck, Ack: m.Seq}); err != nil {
				return err
			}
		}
	}
}

// readLine reads one line of at most MaxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		frag, err := r.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > MaxLine {
			return nil, fmt.Errorf("capcom engine: a line over %d bytes", MaxLine)
		}
		if err == nil {
			return buf, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}

// handle takes one engine message. Each is stored before its ack goes back (4.3).
func (l *Link) handle(c *conn, m Message) {
	th := l.cfg.Thread
	switch m.Type {
	case TReady:
		l.mu.Lock()
		if l.cur != c {
			l.mu.Unlock()
			return
		}
		l.st.Ready = true
		l.st.Engine, l.st.Version, l.st.SDKVersion, l.st.Capabilities = m.Engine, m.Version, m.SDKVersion, slices.Clone(m.Capabilities)
		if t := m.RunningTurn(); t != nil {
			l.st.Turn, l.st.Origin = t.Turn, t.Origin
		}
		c.ready = true
		l.mu.Unlock()
		l.changed()
		l.wake()
		if l.cfg.OnReady != nil {
			l.cfg.OnReady()
		}
	case TSessionStarted:
		if m.Session == "" {
			return
		}
		now := l.cfg.Now().UTC()
		var divider string
		l.cfg.Record.Update(func(r *Record) {
			if r.Current() == m.Session {
				return
			}
			if n := len(r.Sessions); n > 0 && r.Sessions[n-1].End.IsZero() {
				r.Sessions[n-1].End = now
			}
			r.Sessions = append(r.Sessions, SDKSession{ID: m.Session, Start: now, Why: m.Why})
			switch m.Why {
			case "reseed":
				divider = "Earlier messages summarised"
			case "new":
				divider = "New conversation"
			}
		})
		if divider != "" {
			summary := l.cfg.Record.Get().Summary
			if m.Why == "new" {
				summary = ""
			}
			th.Add(capcomthread.Item{Kind: capcomthread.KindDivider, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: divider, Summary: summary})
		}
	case TTurnStarted:
		l.mu.Lock()
		l.st.Turn, l.st.Origin = m.TurnID(), m.Origin
		l.typingLocked(m.Origin == "user")
		l.mu.Unlock()
		l.changed()
	case TInputTaken:
		id := m.ItemID()
		if id == "" {
			return
		}
		if _, ok := th.Get(id); !ok {
			return
		}
		th.MarkRead(id, l.cfg.Now())
		l.cfg.Record.Update(func(r *Record) {
			if id > r.Taken {
				r.Taken = id
			}
		})
		l.touch()
	case TReply:
		l.mu.Lock()
		event := l.st.Origin == "event" && l.st.Turn == m.TurnID()
		l.mu.Unlock()
		if _, _, err := th.Add(capcomthread.Item{Kind: capcomthread.KindMessage, Author: capcomthread.Author{Kind: capcomthread.AuthorCapcom},
			Turn: m.TurnID(), Part: m.Part, Text: m.Text, Event: event}); err != nil {
			l.cfg.Logf("capcom: store a reply: %v", err)
		}
		l.touch()
	case TTool:
		l.mu.Lock()
		event := l.st.Origin == "event"
		l.mu.Unlock()
		if _, err := th.AddTool(m.TurnID(), capcomthread.ToolCall{Name: m.Name, Summary: m.Summary}, event); err != nil {
			l.cfg.Logf("capcom: store a tool line: %v", err)
		}
		l.touch()
	case TApprovalRequest:
		l.openApproval(m)
	case TTurnEnded:
		l.mu.Lock()
		if l.st.Turn == m.TurnID() || m.TurnID() == "" {
			l.st.Turn, l.st.Origin = "", ""
		}
		l.typingLocked(false)
		l.mu.Unlock()
		if m.Result == ResultError {
			th.Add(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
				Text: "Capcom could not finish that turn; send the message again"})
		}
		l.changed()
		if l.cfg.OnTurnEnded != nil {
			l.cfg.OnTurnEnded(m.Result)
		}
	case TSummary:
		l.cfg.Record.Update(func(r *Record) { r.Summary = m.Text })
	case TCompacted:
		th.Add(capcomthread.Item{Kind: capcomthread.KindDivider, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
			Text: "Earlier messages summarised", Summary: l.cfg.Record.Get().Summary})
	case TError:
		if l.cfg.OnError != nil {
			l.cfg.OnError(m.Kind, m.Text, m.Exiting)
		}
	}
}

// typingLocked turns typing on or off. On, it stops after typing_timeout with no engine
// message (7.3). The caller holds l.mu.
func (l *Link) typingLocked(on bool) {
	if l.typingT != nil {
		l.typingT()
		l.typingT = nil
	}
	l.st.Typing = on
	if on {
		l.typingT = l.cfg.After(l.cfg.TypingTimeout(), func() {
			l.mu.Lock()
			l.st.Typing = false
			l.typingT = nil
			l.mu.Unlock()
			l.changed()
		})
	}
}

// touch restarts the typing timeout on an engine message during a typing turn.
func (l *Link) touch() {
	l.mu.Lock()
	if l.st.Typing {
		l.typingLocked(true)
	}
	l.mu.Unlock()
}

// pump sends the items for the model the engine has not had on its connection, in thread
// order (4.3). It runs for the link's life.
func (l *Link) pump() {
	for range l.kick {
		l.mu.Lock()
		c := l.cur
		ready := c != nil && c.ready
		l.mu.Unlock()
		if !ready {
			continue
		}
		after := c.sent
		if after == "" {
			after = l.cfg.Record.Get().Taken
		}
		for _, it := range l.cfg.Thread.After(after, capcomthread.Item.ForModel) {
			if err := l.send(c, Message{Type: TInput, Item: inputJSON(it)}); err != nil {
				break
			}
			c.sent = it.ID
		}
	}
}

func inputJSON(it capcomthread.Item) json.RawMessage {
	in := InputItem{ID: it.ID, Kind: it.Kind, Time: it.Time, TZ: it.TZ, Text: it.Text, Images: it.Images}
	switch it.Author.Kind {
	case capcomthread.AuthorAgent:
		in.Author = it.Author.Address
	default:
		in.Author = it.Author.Kind
	}
	if in.Kind == capcomthread.KindRound {
		in.Kind = capcomthread.KindAgent
	}
	for _, b := range it.Context {
		in.Context = append(in.Context, ContextBlock{Kind: b.Kind, Title: b.Title, Text: b.Text})
	}
	b, _ := json.Marshal(in)
	return b
}

// openApproval keeps one card per request id, and expires it after capcom.approval_max_age
// (5.3).
func (l *Link) openApproval(m Message) {
	if m.Request == "" {
		return
	}
	l.mu.Lock()
	if _, open := l.approval[m.Request]; open {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	th := l.cfg.Thread
	key := "approval:" + m.Request
	card, _, err := th.Add(capcomthread.Item{Kind: capcomthread.KindCard, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Key: key, Text: approvalText(m), Card: &capcomthread.Card{Type: "approval", Ref: m.Request, Choices: []string{"allow", "deny"}}})
	if err != nil {
		l.cfg.Logf("capcom: store an approval card: %v", err)
		return
	}
	if card.Answer != nil {
		// Answered before a restart; the engine asked again: hand it the answer.
		l.answerEngine(m.Request, card.Answer.Choice == "allow", answerMessage(card.Answer.Choice))
		return
	}
	// The age counts from the card: a request asked again after a gff-hab restart keeps the
	// time it first opened.
	opened := card.Time
	age := l.cfg.ApprovalMaxAge()
	left := max(0, opened.Add(age).Sub(l.cfg.Now()))
	a := &openApproval{Approval: Approval{Request: m.Request, Tool: m.Tool, Input: m.Input, Reason: m.Reason, Item: card.ID, Opened: opened, Expires: opened.Add(age)}}
	l.mu.Lock()
	if _, open := l.approval[m.Request]; open {
		l.mu.Unlock()
		return
	}
	l.approval[m.Request] = a
	a.stop = l.cfg.After(left, func() { l.expire(m.Request) })
	l.mu.Unlock()
	l.changed()
}

func approvalText(m Message) string {
	if m.Reason != "" {
		return m.Reason
	}
	return "Capcom wants to use " + m.Tool
}

func answerMessage(choice string) string {
	switch choice {
	case "allow":
		return ""
	case "expired":
		return "the user did not answer"
	}
	return "the user denied this"
}

func (l *Link) expire(request string) {
	if _, err := l.resolve(request, "expired", ""); err != nil && !errors.Is(err, capcomthread.ErrConflict) {
		l.cfg.Logf("capcom: expire approval %s: %v", request, err)
	}
}

// Answer answers an open approval: allow or deny. The first answer wins; a later one, or one
// for a request that is not open, gets capcomthread.ErrConflict or ErrNotFound.
func (l *Link) Answer(request string, allow bool, by string) error {
	choice := "deny"
	if allow {
		choice = "allow"
	}
	_, err := l.resolve(request, choice, by)
	return err
}

// ErrNotFound means no approval with that id is open.
var ErrNotFound = errors.New("capcom: no such approval is open")

func (l *Link) resolve(request, choice, by string) (capcomthread.Item, error) {
	l.mu.Lock()
	a := l.approval[request]
	if a == nil {
		l.mu.Unlock()
		return capcomthread.Item{}, ErrNotFound
	}
	delete(l.approval, request)
	if a.stop != nil {
		a.stop()
	}
	l.mu.Unlock()
	it, err := l.cfg.Thread.AnswerCard(a.Item, choice, by)
	if err == nil {
		l.answerEngine(request, choice == "allow", answerMessage(choice))
	}
	l.changed()
	return it, err
}

func (l *Link) answerEngine(request string, allow bool, msg string) {
	l.mu.Lock()
	c := l.cur
	l.mu.Unlock()
	if c == nil {
		return // the engine asks again after its next ready, and finds the answer on the card
	}
	if err := l.send(c, Message{Type: TApprovalAnswer, Request: request, Allow: &allow, Message: msg}); err != nil {
		l.cfg.Logf("capcom: answer approval %s: %v", request, err)
	}
}

// Drop forgets every open approval with its card marked expired: the engine that asked is gone
// (a crash, a new engine).
func (l *Link) Drop() {
	l.mu.Lock()
	ids := make([]string, 0, len(l.approval))
	for id := range l.approval {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		l.expire(id)
	}
}

// Interrupt stops the current turn (7.4 capcom.interrupt).
func (l *Link) Interrupt() error {
	l.mu.Lock()
	c, st := l.cur, l.st
	l.mu.Unlock()
	switch {
	case c == nil || !st.Ready:
		return ErrNoEngine
	case !st.Has(CapInterrupt):
		return ErrUnsupported
	}
	return l.send(c, Message{Type: TInterrupt})
}

// Shutdown asks the engine to finish its turn and exit. It reports false with no engine.
func (l *Link) Shutdown() bool {
	l.mu.Lock()
	c := l.cur
	if c != nil {
		l.st.Draining = true
	}
	l.mu.Unlock()
	if c == nil {
		return false
	}
	l.changed()
	return l.send(c, Message{Type: TShutdown}) == nil
}

// Reseed asks the engine for a fresh SDK session (reason new, limit, quiet, restart_failed).
func (l *Link) Reseed(reason string) error {
	l.mu.Lock()
	c := l.cur
	l.mu.Unlock()
	if c == nil {
		return ErrNoEngine
	}
	return l.send(c, Message{Type: TReseed, Reason: reason})
}

// Close ends the current connection, if any.
func (l *Link) Close() {
	l.mu.Lock()
	c := l.cur
	l.mu.Unlock()
	if c != nil {
		c.rw.Close()
		<-c.done
	}
}
