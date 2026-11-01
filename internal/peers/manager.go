package peers

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// Capability is the capability a hab with peer links sends (mesh.md 4.2).
const Capability = "mesh"

// DefaultPoll is how often the Manager checks the peer file for a change.
const DefaultPoll = 2 * time.Second

// Config is what Open needs. Path and Self are required.
type Config struct {
	Path  string // the peer file
	Self  string // this hab's device id
	Host  string // this hab's host id
	Build string // this hab's build, sent as the client version
	// Publish, when set, gets each peer whose state changed: the peer.updated event.
	Publish func(api.Peer)
	// Poll is how often the peer file is checked for a change. Default DefaultPoll.
	Poll time.Duration
	// Conn adjusts each peer's habconn.Config before it starts: backoff in tests.
	Conn func(*habconn.Config)
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Manager keeps the peer list and one link per peer.
type Manager struct {
	cfg  Config
	set  *hosts.Set
	done chan struct{}
	wg   sync.WaitGroup

	mu        sync.Mutex
	problems  []string
	specs     []hosts.Spec         // the list as last read, this hab's own entries left out
	pins      map[string]string    // label -> device id pinned in the file, as last read
	self      map[string]bool      // labels whose endpoint answered with this hab's device id
	seen      map[string]time.Time // label -> when last online
	inbound   map[uint64]api.PeerHello
	published map[string]api.Peer
	stamp     fileStamp
}

type fileStamp struct {
	ok    bool
	size  int64
	mtime time.Time
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{ok: true, size: fi.Size(), mtime: fi.ModTime()}
}

// Open reads the peer file and starts one link per peer. A missing file is no peers; a file
// that does not parse is reported in List's problems, and the Manager waits for a fix.
func Open(cfg Config) (*Manager, error) {
	if cfg.Path == "" || cfg.Self == "" {
		return nil, errors.New("peers: no peer file or no device id")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	m := &Manager{
		cfg:       cfg,
		done:      make(chan struct{}),
		pins:      map[string]string{},
		self:      map[string]bool{},
		seen:      map[string]time.Time{},
		inbound:   map[uint64]api.PeerHello{},
		published: map[string]api.Peer{},
	}
	m.stamp = stampOf(cfg.Path)
	set, err := hosts.Open(hosts.Config{
		Source: source{m},
		Client: api.ClientInfo{Name: "gff-hab", Version: cfg.Build, Kind: api.ClientKindHab},
		Label:  cfg.Host,
		Conn: func(c *habconn.Config) {
			c.Peer = &api.PeerHello{DeviceID: cfg.Self, Host: cfg.Host}
			c.NoSubscribe = true
			if cfg.Conn != nil {
				cfg.Conn(c)
			}
		},
		Now:  cfg.Now,
		Logf: cfg.Logf,
	})
	if err != nil {
		return nil, err
	}
	m.set = set
	m.wg.Add(2)
	go m.pump()
	go m.watch()
	return m, nil
}

// source is the hosts.Source over the peer file.
type source struct{ m *Manager }

// Hosts reads the peer file and leaves out this hab's own entries. A file that does not parse
// keeps the last good list.
func (s source) Hosts() ([]hosts.Spec, error) {
	m := s.m
	specs, problems, err := Load(m.cfg.Path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.problems = []string{err.Error()}
		m.cfg.Logf("peers: %v", err)
		return slices.Clone(m.specs), nil
	}
	m.problems = problems
	for _, p := range problems {
		m.cfg.Logf("peers: %s", p)
	}
	out := specs[:0]
	pins := map[string]string{}
	for _, sp := range specs {
		if sp.DeviceID == m.cfg.Self || m.self[sp.Label] {
			continue
		}
		pins[sp.Label] = sp.DeviceID
		out = append(out, sp)
	}
	m.pins = pins
	m.specs = slices.Clone(out)
	return out, nil
}

// reload reads the peer file again and applies it.
func (m *Manager) reload() {
	m.mu.Lock()
	m.stamp = stampOf(m.cfg.Path)
	m.mu.Unlock()
	if err := m.set.Reload(); err != nil {
		m.cfg.Logf("peers: reload: %v", err)
	}
	m.publishAll()
}

// watch reloads the peer file when its size or modification time changes.
func (m *Manager) watch() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Poll)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
		}
		st := stampOf(m.cfg.Path)
		m.mu.Lock()
		changed := st != m.stamp
		m.mu.Unlock()
		if changed {
			m.reload()
		}
	}
}

// pump follows every peer's link.
func (m *Manager) pump() {
	defer m.wg.Done()
	for u := range m.set.Updates() {
		if u.Removed {
			m.mu.Lock()
			delete(m.published, u.Host)
			m.mu.Unlock()
			continue
		}
		if u.Kind == habconn.UpdateOnline {
			if m.online(u.Host, u.Welcome) {
				continue
			}
		}
		m.publish(u.Host)
	}
}

// online handles a link that came up. It reports true when the peer turned out to be this hab
// and was dropped.
func (m *Manager) online(label string, w api.Welcome) bool {
	if w.DeviceID == m.cfg.Self {
		m.cfg.Logf("peers: %s is this hab; skipped", label)
		m.mu.Lock()
		m.self[label] = true
		m.mu.Unlock()
		m.reload()
		return true
	}
	m.mu.Lock()
	m.seen[label] = m.cfg.Now()
	pinned := m.pins[label]
	m.mu.Unlock()
	if pinned == "" && w.DeviceID != "" {
		// First contact: pin the peer's device id in the file (mesh.md 3.2).
		if err := SetPin(m.cfg.Path, label, w.DeviceID); err != nil {
			m.cfg.Logf("peers: pin %s: %v", label, err)
		} else {
			m.cfg.Logf("peers: pinned %s as %s", label, w.DeviceID)
			m.reload()
		}
	}
	return false
}

// Retry cuts every offline peer's backoff short: a wake from sleep or a network change.
func (m *Manager) Retry() { m.set.Retry() }

// Close ends every link.
func (m *Manager) Close() {
	select {
	case <-m.done:
		return
	default:
	}
	close(m.done)
	m.set.Close()
	m.wg.Wait()
}

// Linked records a link that another hab opened to this one.
func (m *Manager) Linked(id uint64, p api.PeerHello) {
	if p.DeviceID == m.cfg.Self {
		return
	}
	m.mu.Lock()
	m.inbound[id] = p
	m.mu.Unlock()
	m.publishAll()
}

// Unlinked records the end of a link from another hab.
func (m *Manager) Unlinked(id uint64) {
	m.mu.Lock()
	p, ok := m.inbound[id]
	delete(m.inbound, id)
	m.mu.Unlock()
	if ok {
		m.markSeen(p.DeviceID)
		m.publishAll()
	}
}

// markSeen records now as last seen for the peers pinned to id.
func (m *Manager) markSeen(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for label, pin := range m.pins {
		if pin == id {
			m.seen[label] = m.cfg.Now()
		}
	}
}

// List returns the peers with their state, the habs linked to this one that the list lacks,
// and the file's problems.
func (m *Manager) List() api.PeerList {
	hs := m.set.Hosts()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := api.PeerList{Peers: []api.Peer{}, Unlisted: []api.LinkedPeer{}, Problems: slices.Clone(m.problems)}
	listed := map[string]bool{}
	for _, h := range hs {
		p := m.peerLocked(h)
		out.Peers = append(out.Peers, p)
		if p.DeviceID != "" {
			listed[p.DeviceID] = true
		}
	}
	shown := map[string]bool{}
	for _, in := range m.inbound {
		if !listed[in.DeviceID] && !shown[in.DeviceID] {
			shown[in.DeviceID] = true
			out.Unlisted = append(out.Unlisted, api.LinkedPeer{DeviceID: in.DeviceID, Host: in.Host})
		}
	}
	slices.SortFunc(out.Unlisted, func(a, b api.LinkedPeer) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return out
}

// Accept pins the device id that a refused peer presented (transport.md 6.3).
func (m *Manager) Accept(label string) (api.PeerAccepted, error) {
	h, ok := m.set.Host(label)
	if !ok {
		return api.PeerAccepted{}, control.Errorf(api.ErrorKindNotFound, "no peer %q in %s", label, m.cfg.Path)
	}
	var dc *habconn.DeviceChangedError
	if !errors.As(h.Err, &dc) {
		return api.PeerAccepted{}, control.Errorf(api.ErrorKindConflict, "peer %q presents no other device id; nothing to accept", label)
	}
	if err := SetPin(m.cfg.Path, label, dc.Got); err != nil {
		return api.PeerAccepted{}, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	m.cfg.Logf("peers: accepted %s: %s, was %s", label, dc.Got, dc.Pinned)
	m.reload()
	return api.PeerAccepted{Label: label, Old: dc.Pinned, New: dc.Got}, nil
}

// publish sends peer.updated for one peer when its row changed.
func (m *Manager) publish(label string) {
	h, ok := m.set.Host(label)
	if !ok {
		return
	}
	m.mu.Lock()
	p := m.peerLocked(h)
	prev, had := m.published[label]
	m.published[label] = p
	m.mu.Unlock()
	if (!had || !samePeer(prev, p)) && m.cfg.Publish != nil {
		m.cfg.Publish(p)
	}
}

func (m *Manager) publishAll() {
	for _, h := range m.set.Hosts() {
		m.publish(h.Label)
	}
}

// samePeer compares two rows, ignoring last seen, which moves while a peer is online.
func samePeer(a, b api.Peer) bool {
	a.LastSeen, b.LastSeen = "", ""
	return a == b
}

// peerLocked makes the row of one peer (mesh.md 4.3). The caller holds m.mu.
func (m *Manager) peerLocked(h hosts.Host) api.Peer {
	p := api.Peer{Label: h.Label, Endpoint: h.Endpoint, DeviceID: m.pins[h.Label], Host: h.Welcome.Host.ID}
	if p.DeviceID == "" {
		p.DeviceID = h.Welcome.DeviceID
	}
	if p.DeviceID != "" {
		for _, in := range m.inbound {
			if in.DeviceID == p.DeviceID {
				p.Inbound = true
				if p.Host == "" {
					p.Host = in.Host
				}
			}
		}
	}
	var dc *habconn.DeviceChangedError
	var ce *control.Error
	switch {
	case h.Status == hosts.Failed && errors.As(h.Err, &dc):
		p.State, p.OfferedID = api.PeerStateRefused, dc.Got
		p.Reason = fmt.Sprintf("different hab at %s: pinned %s, it presents %s; run gff-hab peers accept %s", h.Endpoint, dc.Pinned, dc.Got, h.Label)
		p.Inbound = false
	case h.Status == hosts.Failed && errors.As(h.Err, &ce) && ce.Data.Kind == api.ErrorKindRevisionUnsupported:
		p.State, p.Reason = api.PeerStateUpdateNeeded, "no API revision fits; update gff-hab on one side"
	case h.Status == hosts.Online && !slices.Contains(h.Welcome.Capabilities, Capability):
		p.State, p.Reason = api.PeerStateUpdateNeeded, "the peer's gff-hab has no peer links; update it"
	case h.Status == hosts.Online || p.Inbound:
		p.State = api.PeerStateOnline
	case h.Status == hosts.Connecting && h.Err == nil:
		p.State = api.PeerStateConnecting
	default:
		p.State = api.PeerStateOffline
		if h.Err != nil {
			p.Reason = reason(h.Err)
		}
	}
	if p.State == api.PeerStateOnline {
		m.seen[h.Label] = m.cfg.Now()
	}
	if t, ok := m.seen[h.Label]; ok {
		p.LastSeen = t.UTC().Format(time.RFC3339)
	}
	return p
}

// reason is an error in a few words for a peer row: its last line, at most 200 bytes.
func reason(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
