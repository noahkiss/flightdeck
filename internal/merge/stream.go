package merge

import (
	"errors"
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
	// Status is the host's status after the event, from the stream itself: Model may already
	// hold a later one. Err is the update's error: why the host is not online. Both are zero
	// on KindRemoved.
	Status hosts.Status
	Err    error
}

// Merger reads every update of a hosts.Set and hands them on as one ordered stream, and keeps
// the merged model in step with it. It takes over the Set's Updates channel: nothing else may
// read it. Its Events channel closes after the Set's Updates channel does, or at Close.
type Merger struct {
	set    *hosts.Set
	events *queue[Event]

	mu        sync.Mutex
	seq       uint64
	model     Model
	connected map[string]bool // per host: an update since its start said it had connected
	closed    bool
	pumped    chan struct{} // closes when pump has read the Set's updates to their end
	done      chan struct{}
}

// New starts a Merger over s. Call Close when done with it, or read Events to its end.
func New(s *hosts.Set) *Merger {
	m := &Merger{set: s, events: newQueue[Event](), connected: map[string]bool{},
		pumped: make(chan struct{}), done: make(chan struct{})}
	m.model = Build(s.Hosts())
	go m.pump()
	go func() {
		<-m.pumped
		<-m.events.ended
		close(m.done)
	}()
	return m
}

// Events delivers every merged event in seq order, without loss until Close. When the client
// reads one, Model already reflects it (and may reflect later ones). Read it to its end, or
// call Close: events nobody reads are held until then.
func (m *Merger) Events() <-chan Event { return m.events.out }

// Model returns the merged view as of the last event applied. Model.Seq says which.
func (m *Merger) Model() Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.model
}

// Set returns the hosts.Set the Merger reads: calls, data streams, Reload and Retry go there.
func (m *Merger) Set() *hosts.Set { return m.set }

// Close ends the Merger and closes its Set. Events closes at once; events not yet read are
// dropped. It returns at once; Done says when everything has stopped.
func (m *Merger) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.events.stop()
	m.set.Close()
}

// Done closes once the Merger has stopped: after Close, or after the Set's updates ended and
// Events was read to its end.
func (m *Merger) Done() <-chan struct{} { return m.done }

func (m *Merger) pump() {
	defer close(m.pumped)
	defer m.events.close()
	// After Close the Set's updates are still read to their end, so the Set can finish.
	for u := range m.set.Updates() {
		m.apply(u, m.set.Hosts)
	}
}

// apply turns one update into an event, rebuilds the model from current when the update
// changed a copy, and queues the event.
func (m *Merger) apply(u hosts.Update, current func() []hosts.Host) {
	ev, ok := convert(u)
	if !ok {
		return
	}
	// An event that changed no copy (pane.output, chat.message, an old revision) keeps the
	// model; anything else rebuilds it from the Set's models: a handful of hosts, each a copy
	// the Set already holds. A Set ahead of this event is harmless; the next event says so.
	rebuild := ev.Kind != KindObject || ev.Changed
	var hs []hosts.Host
	if rebuild {
		hs = current()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	// Status comes from the updates alone, never from the model, which may already hold a
	// later status: an offline event read after a fast reconnect still says offline.
	ev.Status = m.status(u)
	m.seq++
	ev.Seq = m.seq
	if rebuild {
		m.model = Build(hs)
	}
	m.model.Seq = m.seq
	m.events.push(ev)
}

// status is the host's status after u, by the rules of the Set's own model: a host is
// Connecting from its start (or its replacement by Reload) until it first connects, and
// Offline after that while it is not online.
func (m *Merger) status(u hosts.Update) hosts.Status {
	if u.Removed {
		delete(m.connected, u.Host)
		return 0
	}
	switch u.Kind {
	case habconn.UpdateOnline, habconn.UpdateEvent:
		m.connected[u.Host] = true
		return hosts.Online
	case habconn.UpdateOffline:
		if errors.Is(u.Err, hosts.ErrSpecChanged) {
			delete(m.connected, u.Host)
			return hosts.Connecting
		}
		m.connected[u.Host] = true
		return hosts.Offline
	case habconn.UpdateRetrying:
		if m.connected[u.Host] {
			return hosts.Offline
		}
		return hosts.Connecting
	case habconn.UpdateFailed:
		return hosts.Failed
	}
	return 0
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
	out   chan T
	ended chan struct{} // closes when the queue's goroutine has returned
	quit  chan struct{} // closed by stop
	mu    sync.Mutex
	q     []T
	wake  chan struct{}
	done  bool
	once  sync.Once
}

func newQueue[T any]() *queue[T] {
	q := &queue[T]{out: make(chan T), ended: make(chan struct{}), quit: make(chan struct{}), wake: make(chan struct{}, 1)}
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

// close closes out once every value pushed has been read.
func (q *queue[T]) close() {
	q.mu.Lock()
	q.done = true
	q.mu.Unlock()
	q.signal()
}

// stop closes out at once and drops every value not yet read, even when nobody reads.
func (q *queue[T]) stop() {
	q.mu.Lock()
	q.done = true
	q.q = nil
	q.mu.Unlock()
	q.once.Do(func() { close(q.quit) })
}

func (q *queue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue[T]) pump() {
	defer close(q.ended)
	defer close(q.out)
	for {
		q.mu.Lock()
		batch, done := q.q, q.done
		q.q = nil
		q.mu.Unlock()
		for _, v := range batch {
			select {
			case q.out <- v:
			case <-q.quit:
				return
			}
		}
		if done {
			if len(batch) == 0 {
				return
			}
			continue
		}
		select {
		case <-q.wake:
		case <-q.quit:
			return
		}
	}
}
