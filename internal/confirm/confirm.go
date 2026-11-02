// Package confirm keeps the confirmations of one hab (capcom.md 6.1, mesh.md 7): the starts
// and set-ups an agent asks for. With confirm.start_elsewhere on, a start on another host, or
// any start Capcom asks for, opens a confirmation and waits for the user's answer; nothing runs
// until a client confirms it, and an open one expires after 60 minutes and never runs. With
// the setting off, the start runs at once and the confirmation is a record. Either way the hab,
// never the agent, runs the action, with the confirmation id as the idempotency key of the
// target's session.create. Confirmations live in memory: a gff-hab restart drops the open
// ones, which then never run.
package confirm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Capability is the capability word of a hab that serves confirmations.
const Capability = "confirm"

// Limits and times (capcom.md 6.1, 10 D12).
const (
	TTL      = 60 * time.Minute // an open confirmation expires after this
	MaxWait  = 120 * time.Second
	MaxSeed  = 8000 // characters in a seed
	RunTime  = 60 * time.Second
	KeepDone = 200 // closed confirmations kept for confirm.get
)

// The confirmation event types.
const (
	EventRequested api.EventType = "confirm.requested"
	EventClosed    api.EventType = "confirm.closed"
)

// ErrUnknown marks a run whose outcome the hab could not learn, such as a peer link that
// dropped mid-start (capcom.md 6.1). Runner.RunStart wraps it; any other error is a failure the
// target reported.
var ErrUnknown = errors.New("the outcome is unknown")

// Runner checks and runs actions. gff-hab implements it over the catalog, its own control
// server and its peer links.
type Runner interface {
	// CheckStart checks a start before it opens or runs, and after the user's edits: the host
	// is a known peer or this hab, the folder is absolute, the harness is known, the seed is
	// within bounds. It returns the start with the host's label, project and branch filled in,
	// and whether the target is this hab. An error that is a *control.Error keeps its kind.
	CheckStart(s api.ConfirmStart) (api.ConfirmStart, bool, error)
	// RunStart runs the start with id as the idempotency key of session.create on the target.
	// from is the requesting session's name on this hab, or "". An error wrapping ErrUnknown
	// leaves the outcome unknown; any other error is the target's refusal.
	RunStart(ctx context.Context, id, from string, s api.ConfirmStart) (api.ConfirmResult, error)
}

// Config is what New needs. Runner is required.
type Config struct {
	Runner Runner
	// Enabled reports confirm.start_elsewhere. Nil: off.
	Enabled func() bool
	// Capcom reports whether a session on this hab is a Capcom session: every start it asks
	// for needs a confirmation while the setting is on. Nil: none is.
	Capcom func(session string) bool
	// Publish hears confirm.requested and confirm.closed. It is called with no lock held.
	Publish func(structure.Event)
	// Notify sends a message from Flightdeck to a session on this hab: the answer to a request
	// whose wait ended first (capcom.md 6.1). Nil: none is sent.
	Notify func(session, text string) error
	TTL    time.Duration // default TTL
	Now    func() time.Time
	Logf   func(string, ...any)
}

// Service is one hab's confirmations.
type Service struct {
	cfg Config

	mu     sync.Mutex
	open   map[string]*entry
	order  []string // open ids in the order they opened
	closed []*entry // newest last, at most KeepDone
	stop   bool
}

type entry struct {
	c        api.Confirmation
	expires  time.Time
	timer    *time.Timer
	done     chan struct{} // closed when the confirmation closes
	waiters  int           // requests waiting for the answer now
	returned bool          // a request returned the closed confirmation
}

// New returns an empty service.
func New(cfg Config) (*Service, error) {
	if cfg.Runner == nil {
		return nil, errors.New("confirm: Runner is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = TTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Service{cfg: cfg, open: map[string]*entry{}}, nil
}

// Close stops the expiry timers. Open confirmations stay open and never run.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = true
	for _, e := range s.open {
		if e.timer != nil {
			e.timer.Stop()
		}
	}
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// CheckSeed refuses a seed over MaxSeed characters or one that is not UTF-8.
func CheckSeed(seed string) error {
	if !utf8.ValidString(seed) {
		return control.Errorf(api.ErrorKindInvalid, "the seed is not UTF-8 text")
	}
	if n := utf8.RuneCountInString(seed); n > MaxSeed {
		return control.Errorf(api.ErrorKindInvalid, "the seed is %d characters; at most %d", n, MaxSeed)
	}
	return nil
}

// Request opens a confirmation, or runs the start at once as a record (capcom.md 6.1).
func (s *Service) Request(ctx context.Context, p api.ConfirmRequestParams) (api.Confirmation, error) {
	switch p.Kind {
	case "start":
	case "setup":
		return api.Confirmation{}, control.Errorf(api.ErrorKindUnavailable,
			"project set-up is not built yet (mesh.md 7, set up elsewhere); set the project up on that host by hand, then start there")
	default:
		return api.Confirmation{}, control.Errorf(api.ErrorKindInvalid, "kind %q is not one of start, setup", p.Kind)
	}
	if p.Start == nil {
		return api.Confirmation{}, control.Errorf(api.ErrorKindInvalid, "a start confirmation needs start")
	}
	if p.WaitMS < 0 {
		return api.Confirmation{}, control.Errorf(api.ErrorKindInvalid, "wait_ms must not be negative")
	}
	wait := min(time.Duration(p.WaitMS)*time.Millisecond, MaxWait)
	st, self, err := s.cfg.Runner.CheckStart(*p.Start)
	if err != nil {
		return api.Confirmation{}, err
	}
	on := s.cfg.Enabled != nil && s.cfg.Enabled()
	capcom := p.From != "" && s.cfg.Capcom != nil && s.cfg.Capcom(p.From)
	ask := on && (!self || capcom)
	now := s.cfg.Now()
	e := &entry{done: make(chan struct{}), c: api.Confirmation{
		ID: newID(), Kind: "start", From: p.From, Start: &st, Created: stamp(now),
	}}
	if !ask {
		// The setting is off, or the start is on the agent's own host: it runs now, and the
		// card is a record (capcom.md 6.1).
		e.c.State, e.c.Record, e.returned = "running", true, true
		s.mu.Lock()
		s.open[e.c.ID] = e
		s.order = append(s.order, e.c.ID)
		c := e.c
		s.mu.Unlock()
		s.publish(EventRequested, c)
		return s.run(e), nil
	}
	e.c.State = "open"
	e.expires = now.Add(s.cfg.TTL)
	e.c.ExpiresAt = stamp(e.expires)
	s.mu.Lock()
	if s.stop {
		s.mu.Unlock()
		return api.Confirmation{}, control.Errorf(api.ErrorKindUnavailable, "the hab is stopping")
	}
	s.open[e.c.ID] = e
	s.order = append(s.order, e.c.ID)
	id := e.c.ID
	e.timer = time.AfterFunc(s.cfg.TTL, func() { s.expire(id) })
	c := e.c
	if wait > 0 {
		e.waiters++
	}
	s.mu.Unlock()
	s.publish(EventRequested, c)
	if wait == 0 {
		return c, nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.waiters--
	select {
	case <-e.done:
		e.returned = true
	default:
	}
	return e.c, nil
}

// Answer answers an open confirmation (capcom.md 6.1): cancel closes it; confirm runs the
// action with the user's edits and returns once it closed. The first answer wins; any later
// one, or one after the expiry, answers conflict.
func (s *Service) Answer(ctx context.Context, client string, p api.ConfirmAnswerParams) (api.Confirmation, error) {
	if p.Choice != "confirm" && p.Choice != "cancel" {
		return api.Confirmation{}, control.Errorf(api.ErrorKindInvalid, "choice %q is not one of confirm, cancel", p.Choice)
	}
	s.mu.Lock()
	e, err := s.answerableLocked(p.ID)
	if err != nil {
		s.mu.Unlock()
		return api.Confirmation{}, err
	}
	if p.Choice == "cancel" {
		e.c.AnsweredBy = client
		note := s.closeLocked(e, "cancelled", "cancelled by the user", nil)
		c := e.c
		s.mu.Unlock()
		s.after(c, note)
		return c, nil
	}
	start := *e.c.Start
	s.mu.Unlock()

	edited := false
	if ed := p.Edits; ed != nil {
		apply := func(dst *string, v string) {
			if v != "" && v != *dst {
				*dst, edited = v, true
			}
		}
		apply(&start.Host, ed.Host)
		if edited && ed.Folder == "" {
			return api.Confirmation{}, control.Errorf(api.ErrorKindInvalid, "a new host needs its folder: give folder with host")
		}
		apply(&start.Folder, ed.Folder)
		apply(&start.Harness, ed.Harness)
		apply(&start.Model, ed.Model)
		apply(&start.Seed, ed.Seed)
	}
	if edited {
		start.Project, start.Branch = "", ""
		var err error
		if start, _, err = s.cfg.Runner.CheckStart(start); err != nil {
			return api.Confirmation{}, err
		}
	}

	s.mu.Lock()
	if e, err = s.answerableLocked(p.ID); err != nil { // another answer won meanwhile
		s.mu.Unlock()
		return api.Confirmation{}, err
	}
	e.c.State, e.c.AnsweredBy, e.c.Edited, e.c.Start = "running", client, edited, &start
	if e.timer != nil {
		e.timer.Stop()
	}
	s.mu.Unlock()
	return s.run(e), nil
}

// answerableLocked returns the open confirmation with the id, or why it cannot be answered.
func (s *Service) answerableLocked(id string) (*entry, error) {
	e, ok := s.open[id]
	if !ok {
		for _, d := range s.closed {
			if d.c.ID == id {
				return nil, closedErr(d.c)
			}
		}
		return nil, control.Errorf(api.ErrorKindNotFound, "no confirmation %q on this hab", id)
	}
	if e.c.State != "open" {
		return nil, closedErr(e.c)
	}
	if !s.cfg.Now().Before(e.expires) {
		note := s.closeLocked(e, "expired", "no answer within 60 minutes; nothing ran", nil)
		c := e.c
		go s.after(c, note)
		return nil, closedErr(c)
	}
	return e, nil
}

func closedErr(c api.Confirmation) error {
	switch {
	case c.Record:
		return control.Errorf(api.ErrorKindConflict, "confirmation %s is a record: it ran at once", c.ID)
	case c.AnsweredBy != "":
		return control.Errorf(api.ErrorKindConflict, "confirmation %s was already answered by %s; it is %s", c.ID, c.AnsweredBy, c.State)
	}
	return control.Errorf(api.ErrorKindConflict, "confirmation %s is %s", c.ID, c.State)
}

// run runs a confirmation in state running and closes it with the outcome.
func (s *Service) run(e *entry) api.Confirmation {
	ctx, cancel := context.WithTimeout(context.Background(), RunTime)
	defer cancel()
	s.mu.Lock()
	id, from, start := e.c.ID, e.c.From, *e.c.Start
	s.mu.Unlock()
	res, err := s.cfg.Runner.RunStart(ctx, id, from, start)
	s.mu.Lock()
	var note string
	switch {
	case err == nil:
		note = s.closeLocked(e, "done", "", &res)
	case errors.Is(err, ErrUnknown):
		note = s.closeLocked(e, "unknown", errText(err)+"; check the target for a session before you retry", nil)
	default:
		note = s.closeLocked(e, "failed", errText(err), nil)
	}
	c := e.c
	s.mu.Unlock()
	s.after(c, note)
	return c
}

func errText(err error) string {
	return strings.TrimPrefix(err.Error(), "gff-hab: ")
}

// expire closes an open confirmation whose time ran out.
func (s *Service) expire(id string) {
	s.mu.Lock()
	e, ok := s.open[id]
	if !ok || e.c.State != "open" || s.stop {
		s.mu.Unlock()
		return
	}
	note := s.closeLocked(e, "expired", "no answer within 60 minutes; nothing ran", nil)
	c := e.c
	s.mu.Unlock()
	s.after(c, note)
}

// closeLocked closes e. It returns the message for the requesting session when no request
// will return the outcome: its wait ended first, or it asked for none.
func (s *Service) closeLocked(e *entry, state api.ConfirmState, reason string, res *api.ConfirmResult) string {
	e.c.State, e.c.Reason, e.c.Result = state, reason, res
	e.c.Closed = stamp(s.cfg.Now())
	if e.timer != nil {
		e.timer.Stop()
	}
	close(e.done)
	delete(s.open, e.c.ID)
	if i := slices.Index(s.order, e.c.ID); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
	s.closed = append(s.closed, e)
	if len(s.closed) > KeepDone {
		s.closed = slices.Delete(s.closed, 0, len(s.closed)-KeepDone)
	}
	if e.c.From == "" || e.waiters > 0 || e.returned {
		return ""
	}
	return Message(e.c)
}

// after publishes confirm.closed and sends the note, with no lock held.
func (s *Service) after(c api.Confirmation, note string) {
	s.publish(EventClosed, c)
	if note == "" || s.cfg.Notify == nil {
		return
	}
	if err := s.cfg.Notify(c.From, note); err != nil {
		s.cfg.Logf("confirm: %s: message to %s: %v", c.ID, c.From, err)
	}
}

func (s *Service) publish(t api.EventType, c api.Confirmation) {
	if s.cfg.Publish != nil {
		s.cfg.Publish(structure.Event{Type: t, ID: c.ID, Data: c})
	}
}

// Message is the text a requesting session gets when the answer came after its wait.
func Message(c api.Confirmation) string {
	where := "this host"
	if c.Start != nil && c.Start.Host != "" {
		where = c.Start.Host
	}
	switch c.State {
	case "done":
		name := ""
		if c.Result != nil {
			name = c.Result.Name
			if name == "" {
				name = c.Result.Session
			}
		}
		return fmt.Sprintf("Confirmation %s: the owner confirmed the start; session %s started on %s.", c.ID, name, where)
	case "cancelled":
		return fmt.Sprintf("Confirmation %s: the owner cancelled the start on %s. Nothing ran.", c.ID, where)
	case "expired":
		return fmt.Sprintf("Confirmation %s: no answer within 60 minutes, so it expired. Nothing ran.", c.ID)
	case "unknown":
		return fmt.Sprintf("Confirmation %s: the start on %s has an unknown outcome: %s.", c.ID, where, c.Reason)
	}
	return fmt.Sprintf("Confirmation %s: the start on %s failed: %s.", c.ID, where, c.Reason)
}

// Open returns the open confirmations, oldest first, for the host snapshot.
func (s *Service) Open() []api.Confirmation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.Confirmation
	for _, id := range s.order {
		if e := s.open[id]; e != nil && e.c.State == "open" {
			out = append(out, e.c)
		}
	}
	return out
}

// List returns the open and running confirmations, oldest first, and with all the closed ones
// kept, newest first.
func (s *Service) List(all bool) []api.Confirmation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Confirmation{}
	for _, id := range s.order {
		if e := s.open[id]; e != nil {
			out = append(out, e.c)
		}
	}
	if all {
		for i := len(s.closed) - 1; i >= 0; i-- {
			out = append(out, s.closed[i].c)
		}
	}
	return out
}

// Get returns one confirmation, open or closed.
func (s *Service) Get(id string) (api.Confirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.open[id]; ok {
		return e.c, nil
	}
	for _, e := range s.closed {
		if e.c.ID == id {
			return e.c, nil
		}
	}
	return api.Confirmation{}, control.Errorf(api.ErrorKindNotFound, "no confirmation %q on this hab", id)
}
