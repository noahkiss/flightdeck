// Package hosts keeps one connection to every configured hab at once, for gff's sidebar, tabs
// and status board (docs/specs/gff-client.md 4, 6, 7).
//
// Each host is one habconn.Conn started with habconn.Start, so a hab that is down is a host
// that is offline, never an error: the Conn keeps trying and the host comes online when its
// hab does, with no restart of the client. The Set reports every update of every host on one
// channel, in order per host, and keeps a model of each host: its status, since when, the last
// error, the last welcome and the structure copy the Conn holds. Structure stays per host and
// is merged by device id above it (packages merge and board); seq is never compared across
// hosts (hab-logic-api.md 7.9).
//
// Which hosts to connect to comes from a Source. This stage reads gff's own config file and
// flags (Static). The mesh peer list (Part 3) plugs in as another Source; Reload picks up a
// changed list.
package hosts

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
)

// Spec is one configured host. Its shape is a mesh peer entry (mesh.md 3.1): a label local to
// this client, one or more endpoints, and an optional pinned device id. The device id is the
// host's identity everywhere; the label is only this client's name for it.
type Spec struct {
	// Label is the owner's name for the host, unique in the list. Rows and frames show it.
	Label string
	// Endpoints reach the host's hab, in the owner's order.
	Endpoints []Endpoint
	// DeviceID, when set, is the device id pinned for this host (gff-client.md 11.4).
	DeviceID string
	// Transport, when set, is used instead of the endpoints: tests and the single-hab flags.
	Transport habconn.Transport
}

// Source lists the hosts to connect to.
type Source interface {
	Hosts() ([]Spec, error)
}

// Static is a fixed list of hosts: gff's config file and flags.
type Static []Spec

// Hosts returns the list.
func (s Static) Hosts() ([]Spec, error) { return slices.Clone(s), nil }

// Status is where a host's connection stands.
type Status int

const (
	// Connecting: no connection has come up yet since the host was added.
	Connecting Status = iota + 1
	// Online: a control connection is up and its snapshot applied.
	Online
	// Offline: the connection ended; the Conn keeps trying.
	Offline
	// Failed: the Conn stopped for good (the API revision does not fit, the device id
	// changed). Only a change on the owner's side helps.
	Failed
)

func (s Status) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Online:
		return "online"
	case Offline:
		return "offline"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Host is the model of one host at one moment.
type Host struct {
	Label  string
	Status Status
	// Since is when Status began, on this machine's clock.
	Since time.Time
	// Err is why the host is not online: the last failed attempt, the drop, or the failure.
	// Nil while online.
	Err error
	// Welcome is the last welcome; zero before the first connection.
	Welcome api.Welcome
	// Structure is the structure copy the Conn holds. It stays while the host is offline, so
	// its rows can stay, dimmed (gff-client.md 4.2), and is replaced in one swap by the next
	// snapshot.
	Structure api.Structure
	// Conn reaches the hab: calls, data streams. Calls fail with habconn.ErrOffline while the
	// host is not online.
	Conn *habconn.Conn
	// Endpoint is the endpoint in use while online, else the one tried last. Empty when the
	// spec gave a Transport.
	Endpoint string
}

// Online reports whether the host has a live control connection.
func (h Host) Online() bool { return h.Status == Online }

// DeviceID is the host's identity: the device id from its last welcome, empty before one.
func (h Host) DeviceID() string { return h.Welcome.DeviceID }

// Problem says in a few words why a host is not online, for its row: "connecting", "offline",
// "update needed" (no API revision fits), "host key changed" (the device id changed), or
// "failed".
func (h Host) Problem() string {
	switch h.Status {
	case Online:
		return ""
	case Connecting:
		return "connecting"
	case Offline:
		return "offline"
	}
	var dc *habconn.DeviceChangedError
	var ce *control.Error
	switch {
	case errors.As(h.Err, &dc):
		return "host key changed"
	case errors.As(h.Err, &ce) && ce.Data.Kind == api.ErrorKindRevisionUnsupported:
		return "update needed"
	}
	return "failed"
}

// Update is one update of one host, in order per host.
type Update struct {
	Host string // Spec.Label
	habconn.Update
	// Removed: the host left the list (Reload). No further update names it.
	Removed bool
}

// Config is what Open needs. Source and Client are required.
type Config struct {
	Source Source
	Client api.ClientInfo
	Label  string // this client's label on every hab
	// Conn adjusts each host's habconn.Config before it starts: backoff and liveness in tests.
	Conn func(*habconn.Config)
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Set is every configured host.
type Set struct {
	cfg     Config
	updates *queue[Update]

	reloading sync.Mutex // held by Reload for its whole run

	mu     sync.Mutex
	order  []string // host names in the Source's order
	hosts  map[string]*entry
	closed bool
	wg     sync.WaitGroup
}

type entry struct {
	conn    *habconn.Conn
	spec    Spec
	eps     *endpoints // nil when the spec gave a Transport
	model   Host
	gone    bool // removed or replaced by Reload; its pump drops what is left
	wrapped bool
}

// ErrSpecChanged is the Err of the update that reports a host replaced by Reload: its endpoints,
// pin or transport changed, so its old connection closed and a new one starts.
var ErrSpecChanged = errors.New("hosts: the host's settings changed; connecting again")

// sameSpec reports whether two specs reach the same hab under the same pin.
func sameSpec(a, b Spec) bool {
	return a.Label == b.Label && a.DeviceID == b.DeviceID && slices.Equal(a.Endpoints, b.Endpoints) &&
		reflect.DeepEqual(a.Transport, b.Transport)
}

// Open starts one Conn per host and returns at once: every host starts Connecting. A host
// whose transport is missing, or a name given twice, is an error.
func Open(cfg Config) (*Set, error) {
	if cfg.Source == nil {
		return nil, errors.New("hosts: no source")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	specs, err := cfg.Source.Hosts()
	if err != nil {
		return nil, fmt.Errorf("hosts: %w", err)
	}
	if err := check(specs); err != nil {
		return nil, err
	}
	s := &Set{cfg: cfg, updates: newQueue[Update](), hosts: map[string]*entry{}}
	for _, sp := range specs {
		if err := s.add(sp); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Wrap makes a Set of one host over a Conn that is already connected, for gff's single-hab
// view. The Set takes over reading the Conn's updates and closes it on Close.
func Wrap(name string, c *habconn.Conn, now func() time.Time) *Set {
	if now == nil {
		now = time.Now
	}
	s := &Set{cfg: Config{Now: now, Logf: func(string, ...any) {}}, updates: newQueue[Update](), hosts: map[string]*entry{}}
	e := &entry{conn: c, wrapped: true, model: Host{Label: name, Status: Online, Since: now(), Conn: c}}
	if w, ok := c.Welcome(); ok {
		e.model.Welcome = w
		e.model.Structure = c.Structure()
	} else {
		e.model.Status = Offline
	}
	s.order = []string{name}
	s.hosts[name] = e
	s.wg.Add(1)
	go s.pump(name, e)
	return s
}

func check(specs []Spec) error {
	seen := map[string]bool{}
	for _, sp := range specs {
		switch {
		case sp.Label == "":
			return errors.New("hosts: a host has no label")
		case sp.Transport == nil && len(sp.Endpoints) == 0:
			return fmt.Errorf("hosts: host %q has no endpoint", sp.Label)
		case seen[sp.Label]:
			return fmt.Errorf("hosts: host %q is listed twice", sp.Label)
		}
		seen[sp.Label] = true
	}
	return nil
}

// add starts the Conn of one host and appends it to the order. It refuses a label the Set
// already holds. The caller holds no lock.
func (s *Set) add(sp Spec) error {
	e, err := s.start(sp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		e.conn.Close()
		return errors.New("hosts: closed")
	}
	if _, ok := s.hosts[sp.Label]; ok {
		s.mu.Unlock()
		e.conn.Close()
		return fmt.Errorf("hosts: host %q is already started", sp.Label)
	}
	s.order = append(s.order, sp.Label)
	s.hosts[sp.Label] = e
	s.wg.Add(1)
	s.mu.Unlock()
	go s.pump(sp.Label, e)
	return nil
}

// start makes the entry of one host with its Conn started, not yet in the Set.
func (s *Set) start(sp Spec) (*entry, error) {
	tr := sp.Transport
	var eps *endpoints
	if tr == nil {
		var err error
		if eps, err = newEndpoints(sp.Endpoints); err != nil {
			return nil, fmt.Errorf("hosts: %s: %w", sp.Label, err)
		}
		tr = eps
	}
	hc := habconn.Config{Transport: tr, Client: s.cfg.Client, Label: s.cfg.Label, DeviceID: sp.DeviceID,
		Logf: func(f string, a ...any) { s.cfg.Logf(sp.Label+": "+f, a...) }}
	if s.cfg.Conn != nil {
		s.cfg.Conn(&hc)
	}
	c, err := habconn.Start(hc)
	if err != nil {
		return nil, fmt.Errorf("hosts: %s: %w", sp.Label, err)
	}
	return &entry{conn: c, spec: sp, eps: eps, model: Host{Label: sp.Label, Status: Connecting, Since: s.cfg.Now(), Conn: c}}, nil
}

// pump applies one host's updates to its model, then hands them on.
func (s *Set) pump(name string, e *entry) {
	defer s.wg.Done()
	for u := range e.conn.Updates() {
		s.mu.Lock()
		if e.gone {
			s.mu.Unlock()
			continue
		}
		m := &e.model
		now := s.cfg.Now()
		if e.eps != nil {
			m.Endpoint = e.eps.current().String()
		}
		switch u.Kind {
		case habconn.UpdateOnline:
			m.Status, m.Since, m.Err = Online, now, nil
			m.Welcome = u.Welcome
			m.Structure = u.Structure
		case habconn.UpdateEvent:
			if u.Changed {
				m.Structure = e.conn.Structure()
			}
		case habconn.UpdateOffline:
			m.Status, m.Since, m.Err = Offline, now, u.Err
		case habconn.UpdateRetrying:
			if m.Status != Connecting && m.Status != Offline {
				m.Status, m.Since = Offline, now
			}
			m.Err = u.Err
		case habconn.UpdateFailed:
			m.Status, m.Since, m.Err = Failed, now, u.Err
		}
		s.updates.push(Update{Host: name, Update: u})
		s.mu.Unlock()
	}
}

// Updates delivers every host's updates, in order per host, without loss. When the client
// reads one, Hosts already shows it. It closes after Close.
func (s *Set) Updates() <-chan Update { return s.updates.out }

// Hosts returns every host's model, in the Source's order.
func (s *Set) Hosts() []Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Host, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.hosts[n].model)
	}
	return out
}

// Host returns one host's model.
func (s *Set) Host(name string) (Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.hosts[name]
	if !ok {
		return Host{}, false
	}
	return e.model, true
}

// Retry cuts every offline host's backoff short: for a wake from sleep or a network change
// (gff-client.md 11.5).
func (s *Set) Retry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.hosts {
		e.conn.Retry()
	}
}

// Reload reads the Source again: new hosts start, hosts no longer listed close and report
// Removed. A host whose spec is unchanged keeps its connection. A host listed under the same
// label with other endpoints, another pin or another transport is replaced in place: its old
// Conn closes, a new one starts Connecting with the old one's last welcome and structure, and
// one update with Kind UpdateOffline and Err ErrSpecChanged reports it. Reloads run one at a
// time: two at once would both see a new host and start it twice.
func (s *Set) Reload() error {
	s.reloading.Lock()
	defer s.reloading.Unlock()
	specs, err := s.cfg.Source.Hosts()
	if err != nil {
		return fmt.Errorf("hosts: %w", err)
	}
	if err := check(specs); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, sp := range specs {
		want[sp.Label] = true
	}
	s.mu.Lock()
	var drop []*entry
	for _, n := range s.order {
		if !want[n] {
			e := s.hosts[n]
			e.gone = true
			drop = append(drop, e)
			delete(s.hosts, n)
			s.updates.push(Update{Host: n, Removed: true})
		}
	}
	s.order = slices.DeleteFunc(s.order, func(n string) bool { return !want[n] })
	var add, replace []Spec
	for _, sp := range specs {
		e, ok := s.hosts[sp.Label]
		switch {
		case !ok:
			add = append(add, sp)
		case !e.wrapped && !sameSpec(e.spec, sp):
			replace = append(replace, sp)
		}
	}
	s.mu.Unlock()
	for _, e := range drop {
		e.conn.Close()
	}
	for _, sp := range replace {
		if err := s.replace(sp); err != nil {
			return err
		}
	}
	for _, sp := range add {
		if err := s.add(sp); err != nil {
			return err
		}
	}
	return nil
}

// replace starts a new Conn for a listed host whose spec changed and closes the old one. The
// host keeps its place in the order.
func (s *Set) replace(sp Spec) error {
	e, err := s.start(sp)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old, ok := s.hosts[sp.Label]
	if s.closed || !ok {
		s.mu.Unlock()
		e.conn.Close()
		return nil
	}
	old.gone = true
	// The new Conn starts Connecting with the old one's last welcome and structure, so the
	// host's rows stay, stale and with input off, until the new connection's snapshot
	// replaces them (gff-client.md 4.2: no flash). Its Err says why it is connecting again.
	e.model.Welcome, e.model.Structure, e.model.Err = old.model.Welcome, old.model.Structure, ErrSpecChanged
	s.hosts[sp.Label] = e
	s.wg.Add(1)
	s.updates.push(Update{Host: sp.Label, Update: habconn.Update{Kind: habconn.UpdateOffline, Err: ErrSpecChanged}})
	s.mu.Unlock()
	old.conn.Close()
	go s.pump(sp.Label, e)
	return nil
}

// Close closes every host's Conn and, once their updates are handed on, the Updates channel.
func (s *Set) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]*habconn.Conn, 0, len(s.hosts))
	for _, e := range s.hosts {
		conns = append(conns, e.conn)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	go func() {
		s.wg.Wait()
		s.updates.close()
	}()
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
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue[T]) close() {
	q.mu.Lock()
	q.done = true
	q.mu.Unlock()
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
			continue // a close signal may have been taken already: look again, never wait
		}
		<-q.wake
	}
}
