package merge

import (
	"sync"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// Kind says what an Event carries.
type Kind int

const (
	// KindResync: the host is online with a fresh snapshot. Every object of this host is
	// replaced by the model's; objects of other hosts are untouched. NewEpoch says the hab
	// restarted, so the client also drops what it assumed about this host (owners, sizes).
	KindResync Kind = iota + 1
	// KindObject: one event from the host's hab, in its seq order. Key names the object.
	KindObject
	// KindHost: the host's status changed (offline, retrying, failed). Its objects stay in
	// the model, stale, until its next KindResync; a failed host's objects leave it.
	KindHost
	// KindRemoved: the host left the list. Its objects leave the model.
	KindRemoved
)

func (k Kind) String() string {
	switch k {
	case KindResync:
		return "resync"
	case KindObject:
		return "object"
	case KindHost:
		return "host"
	case KindRemoved:
		return "removed"
	}
	return "unknown"
}

// Key names an object across hosts: the host label, the kind of object and its id on that hab.
// The kind is "project", "session", "pane" or "agent" (an agent state's id is its pane's), or
// the event type for an event the client keeps no copy of (pane.output, chat.message, ...).
type Key struct {
	Host   string
	Object string
	ID     string
}

// Event is one change in the merged stream.
type Event struct {
	// Seq numbers the merged stream from 1, with no gaps. It is the client's own order and is
	// never compared with a hab's seq.
	Seq  uint64
	Kind Kind
	Key  Key // Host is always set; Object and ID only for KindObject
	// Event is the hab's event, for KindObject. Changed says whether it changed the host's
	// structure copy (an older revision, or an ephemeral event, does not).
	Event   api.Event
	Changed bool
	// NewEpoch is set on KindResync when the hab restarted since the client last saw it.
	NewEpoch bool
	// Status is the host's status after the event; Err why it is not online.
	Status hosts.Status
	Err    error
}

// Merger reads every update of a hosts.Set and hands them on as one ordered stream, and keeps
// the merged model in step with it. It takes over the Set's Updates channel: nothing else may
// read it. Its Events channel closes after the Set's Updates channel does.
type Merger struct {
	set    *hosts.Set
	events *queue[Event]

	mu    sync.Mutex
	seq   uint64
	model Model
	done  chan struct{}
}

// New starts a Merger over s.
func New(s *hosts.Set) *Merger {
	m := &Merger{set: s, events: newQueue[Event](), done: make(chan struct{})}
	m.model = Build(s.Hosts())
	go m.pump()
	return m
}

// Events delivers every merged event in seq order, without loss. When the client reads one,
// Model already reflects it (and may reflect later ones).
func (m *Merger) Events() <-chan Event { return m.events.out }

// Model returns the merged view as of the last event applied. Model.Seq says which.
func (m *Merger) Model() Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.model
}

// Set returns the hosts.Set the Merger reads: calls, data streams, Reload and Retry go there.
func (m *Merger) Set() *hosts.Set { return m.set }

// Done closes once the Set's updates have ended and every event is queued.
func (m *Merger) Done() <-chan struct{} { return m.done }

func (m *Merger) pump() {
	defer close(m.done)
	defer m.events.close()
	for u := range m.set.Updates() {
		ev, ok := convert(u)
		if !ok {
			continue
		}
		// An event that changed no copy (pane.output, chat.message, an old revision) keeps the
		// model; anything else rebuilds it from the Set's models: a handful of hosts, each a
		// copy the Set already holds. A Set ahead of this event is harmless; the next event
		// says so.
		rebuild := ev.Kind != KindObject || ev.Changed
		var hs []hosts.Host
		if rebuild {
			hs = m.set.Hosts()
		}
		m.mu.Lock()
		m.seq++
		ev.Seq = m.seq
		if rebuild {
			m.model = Build(hs)
		}
		m.model.Seq = m.seq
		if h, ok := m.model.Host(u.Host); ok {
			ev.Status = h.Status
			if ev.Err == nil {
				ev.Err = h.Err
			}
		}
		m.events.push(ev)
		m.mu.Unlock()
	}
}

// convert maps one host update to a merged event. Every update is kept: a retry while already
// offline still carries the error the host's row shows.
func convert(u hosts.Update) (Event, bool) {
	ev := Event{Key: Key{Host: u.Host}}
	if u.Removed {
		ev.Kind = KindRemoved
		return ev, true
	}
	switch u.Kind {
	case habconn.UpdateOnline:
		ev.Kind, ev.NewEpoch = KindResync, u.NewEpoch
	case habconn.UpdateEvent:
		ev.Kind, ev.Event, ev.Changed = KindObject, u.Event, u.Changed
		ev.Key.Object, ev.Key.ID = objectOf(u.Event.Type), u.Event.ID
	case habconn.UpdateOffline, habconn.UpdateRetrying, habconn.UpdateFailed:
		ev.Kind, ev.Err = KindHost, u.Err
	default:
		return Event{}, false
	}
	return ev, true
}

// objectOf names the kind of object an event is about.
func objectOf(t api.EventType) string {
	switch t {
	case api.EventProjectUpdated, api.EventProjectDeleted:
		return "project"
	case api.EventSessionUpdated, api.EventSessionDeleted:
		return "session"
	case api.EventPaneUpdated, api.EventPaneDeleted, api.EventPaneOwner:
		return "pane"
	case api.EventAgentState:
		return "agent"
	}
	return string(t)
}

// queue hands values to a channel in order without ever blocking the sender.
type queue[T any] struct {
	out  chan T
	mu   sync.Mutex
	q    []T
	wake chan struct{}
	done bool
}

func newQueue[T any]() *queue[T] {
	q := &queue[T]{out: make(chan T), wake: make(chan struct{}, 1)}
	go q.pump()
	return q
}

func (q *queue[T]) push(v T) {
	q.mu.Lock()
	if !q.done {
		q.q = append(q.q, v)
	}
	q.mu.Unlock()
	q.signal()
}

func (q *queue[T]) close() {
	q.mu.Lock()
	q.done = true
	q.mu.Unlock()
	q.signal()
}

func (q *queue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue[T]) pump() {
	defer close(q.out)
	for {
		q.mu.Lock()
		batch, done := q.q, q.done
		q.q = nil
		q.mu.Unlock()
		for _, v := range batch {
			q.out <- v
		}
		if done {
			if len(batch) == 0 {
				return
			}
			continue
		}
		<-q.wake
	}
}
