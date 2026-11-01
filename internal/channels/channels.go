// Package channels carries text from one agent to another, on any host (mesh.md 6). A send
// resolves an address, makes a message with a random id, and puts it in the outbox for the
// target hab, or straight in the inbox when the target is on this hab. Outboxes are handed over
// with mail.push and mail.pull over peer links; the receiver drops a repeated id. The inbox
// delivers each message into its agent's PTY, wrapped in an envelope, only when the agent may
// take it. Every outbox, the inbox and the ledger of ids are on disk, so a gff-hab restart loses
// nothing.
package channels

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

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Capability is the capability word of a hab that serves channels.
const Capability = "channels"

// Limits and times (mesh.md 6.5).
const (
	MaxText     = 64 << 10           // bytes of text in one message
	MaxOutbox   = 1000               // items in one target's outbox
	MaxInbox    = 1000               // messages waiting for one session
	Keep        = 7 * 24 * time.Hour // records, ids and outbox items
	Batch       = 100                // items in one mail.push or mail.pull
	DefaultWait = 180 * time.Second  // channel.wait with no timeout
	MaxWait     = 600 * time.Second  // the longest channel.wait or channel.status wait
	// NoStateGrace is how long a message waits for a pane with no agent state at all before it
	// is refused: a hab that just restarted learns the states again from hooks and reports.
	NoStateGrace = 30 * time.Second
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	pullRounds   = 20 // pulls in one exchange while items keep coming
)

// Why a delivery did not happen. Local.Deliver wraps one of these; any other error refuses the
// message with the error's text.
var (
	// ErrBusy holds the message: the agent is working, blocked, or still starting a turn.
	ErrBusy = errors.New("the agent is busy")
	// ErrNoState holds the message for NoStateGrace, then refuses it: the pane has no agent
	// state yet.
	ErrNoState = errors.New("no agent state")
	// ErrNotDelivered: the paste reached the prompt but Enter was not sent. Not retried.
	ErrNotDelivered = errors.New("not delivered")
)

// Link is one peer link this hab dialed.
type Link struct {
	Label    string
	DeviceID string
	Online   bool
	Caps     []string
	Caller   api.Caller // nil when the link is down
}

// Peers is the peer list and its links. gff-hab gives internal/peers.
type Peers interface {
	List() api.PeerList
	Links() []Link
}

// Local is this hab's own sessions and agents.
type Local interface {
	// HasSession reports whether a session with this name exists on this hab.
	HasSession(name string) bool
	// Deliver writes one message to the session's agent now, or returns why not: an error
	// wrapping ErrBusy, ErrNoState or ErrNotDelivered, or any other error to refuse it.
	Deliver(ctx context.Context, session, text string) error
	// Agent returns the state of the session's agent, with ok false when the session is gone,
	// and a channel that is closed at the agent's next change.
	Agent(session string) (st api.AgentStatus, ok bool, changed <-chan struct{})
}

// Config is what New needs. Self, Host and Local are required.
type Config struct {
	Self    string // this hab's device id
	Host    string // this hab's host id
	State   string // the state directory; "" keeps nothing on disk
	Peers   Peers  // nil: only sessions on this hab are reachable
	Catalog func() api.CatalogList
	Local   Local
	// Publish hears every change of a record this hab sent (channel.message).
	Publish func(api.ChannelRecord)
	Poll    time.Duration // the mail round; default 15 s
	Retry   time.Duration // the delivery round; default 1 s
	Now     func() time.Time
	Logf    func(string, ...any)
}

// Service is the channels of one hab.
type Service struct {
	cfg     Config
	started time.Time

	mu      sync.Mutex
	outbox  map[string][]outItem // by target device id
	records map[string]*record   // messages this hab sent, by id
	seen    map[string]seenID    // ids this hab received, by id
	inbox   []*inItem            // in arrival order
	acks    map[string][]api.MailResult
	fails   map[string]*failure // by link label
	online  map[string]bool     // by link label
	changed chan struct{}       // closed and replaced at every change a wait may want

	kick, poke chan struct{}
	done       chan struct{}
	wg         sync.WaitGroup
}

type outItem struct {
	Item api.MailItem `json:"item"`
	At   time.Time    `json:"at"`
}

type record struct {
	api.ChannelRecord
	ToDevice string    `json:"to_device"`
	Sent     time.Time `json:"sent"`
}

type seenID struct {
	At     time.Time      `json:"at"`
	Result api.MailResult `json:"result"`
}

type inItem struct {
	Message api.ChannelMessage `json:"message"`
	At      time.Time          `json:"at"`
	busy    bool               // a delivery or a wait has it now
}

type failure struct {
	n    int
	next time.Time
}

// New loads the saved state and starts the mail and delivery rounds.
func New(cfg Config) (*Service, error) {
	if cfg.Self == "" || cfg.Local == nil {
		return nil, errors.New("channels: Self and Local are required")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 15 * time.Second
	}
	if cfg.Retry <= 0 {
		cfg.Retry = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Service{
		cfg: cfg, started: cfg.Now(),
		outbox: map[string][]outItem{}, records: map[string]*record{}, seen: map[string]seenID{},
		acks: map[string][]api.MailResult{}, fails: map[string]*failure{}, online: map[string]bool{},
		changed: make(chan struct{}),
		kick:    make(chan struct{}, 1), poke: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.wg.Add(2)
	go s.mailLoop()
	go s.deliverLoop()
	return s, nil
}

// Close stops the rounds. Waits in progress end with their context.
func (s *Service) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	s.wg.Wait()
}

// Kick runs a mail round now: after a send, or when a peer's row changed.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Poke runs a delivery round now: an agent's state changed.
func (s *Service) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

func (s *Service) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// --- Send ---

// target is a resolved address.
type target struct {
	device  string
	session string
	address string
	label   string // the peer label; "" for this hab
}

// Send makes a message from p.From, a session on this hab, to p.To, and hands it to this hab's
// inbox or puts it in the target's outbox.
func (s *Service) Send(ctx context.Context, p api.ChannelSendParams) (api.ChannelSent, error) {
	switch {
	case p.From == "":
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "channel.send needs from: the sending session on this hab")
	case p.To == "":
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "channel.send needs to: label/name or a name")
	case strings.TrimSpace(p.Text) == "":
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "the message is empty")
	case len(p.Text) > MaxText:
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "the message is %d bytes; at most %d", len(p.Text), MaxText)
	}
	if !s.cfg.Local.HasSession(p.From) {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindNotFound, "no session named %q on this hab to send from", p.From)
	}
	t, err := s.resolve(p.To)
	if err != nil {
		return api.ChannelSent{}, err
	}
	now := s.cfg.Now()
	msg := api.ChannelMessage{
		ID:      newID(),
		From:    api.ChannelEnd{DeviceID: s.cfg.Self, Host: s.cfg.Host, Session: p.From},
		To:      api.ChannelEnd{DeviceID: t.device, Session: t.session},
		Text:    p.Text,
		ReplyTo: p.ReplyTo,
		At:      stamp(now),
	}
	rec := &record{ChannelRecord: api.ChannelRecord{ID: msg.ID, From: p.From, To: t.address, State: api.ChannelStateQueued,
		At: stamp(now), Created: stamp(now)}, ToDevice: t.device, Sent: now}
	var pub []api.ChannelRecord
	s.mu.Lock()
	if t.device != s.cfg.Self && len(s.outbox[t.device]) >= MaxOutbox {
		s.mu.Unlock()
		return api.ChannelSent{}, control.Errorf(api.ErrorKindConflict, "outbox full for %s: %d messages wait; send again once it is online", t.label, MaxOutbox)
	}
	s.records[msg.ID] = rec
	pub = append(pub, rec.ChannelRecord)
	if t.device == s.cfg.Self {
		res := s.acceptLocked(s.cfg.Self, msg, now)
		pub = append(pub, s.setStateLocked(msg.ID, t.device, res.State, res.Reason, now)...)
		s.saveInboxLocked()
	} else {
		s.outbox[t.device] = append(s.outbox[t.device], outItem{Item: api.MailItem{Kind: api.MailKindMessage, Message: &msg}, At: now})
		s.saveOutboxLocked(t.device)
	}
	s.saveLedgerLocked()
	state := s.records[msg.ID].State
	s.broadcastLocked()
	s.mu.Unlock()
	s.publish(pub)
	if t.device == s.cfg.Self {
		s.Poke()
	} else {
		s.Kick()
	}
	return api.ChannelSent{ID: msg.ID, To: t.address, State: state}, nil
}

// resolve turns an address into a target (mesh.md 6.5, Addresses and names).
func (s *Service) resolve(to string) (target, error) {
	var pl api.PeerList
	if s.cfg.Peers != nil {
		pl = s.cfg.Peers.List()
	}
	if label, name, ok := strings.Cut(to, "/"); ok {
		if label == "" || name == "" {
			return target{}, control.Errorf(api.ErrorKindInvalid, "address %q: want label/name or a name", to)
		}
		for _, p := range pl.Peers {
			if p.Label != label {
				continue
			}
			if p.DeviceID == "" {
				return target{}, control.Errorf(api.ErrorKindUnavailable, "peer %s has not been reached yet, so its device id is unknown; send once it has been online", label)
			}
			if p.DeviceID == s.cfg.Self {
				return target{device: s.cfg.Self, session: name, address: name}, nil
			}
			return target{device: p.DeviceID, session: name, address: to, label: label}, nil
		}
		if label == s.cfg.Host {
			return target{device: s.cfg.Self, session: name, address: name}, nil
		}
		return target{}, control.Errorf(api.ErrorKindNotFound, "no peer labelled %q in the peer list", label)
	}
	var found []target
	if s.cfg.Local.HasSession(to) {
		found = append(found, target{device: s.cfg.Self, session: to, address: to})
	}
	var unlisted []string
	if s.cfg.Catalog != nil {
		for _, e := range s.cfg.Catalog().Hosts {
			if e.Own || e.Catalog.DeviceID == s.cfg.Self || !slices.ContainsFunc(e.Catalog.Agents, func(a api.CatalogAgent) bool { return a.Session == to }) {
				continue
			}
			if e.Label == "" {
				unlisted = append(unlisted, e.Catalog.Host+"/"+to)
				continue
			}
			found = append(found, target{device: e.Catalog.DeviceID, session: to, address: e.Label + "/" + to, label: e.Label})
		}
	}
	switch {
	case len(found)+len(unlisted) > 1:
		var addrs []string
		for _, f := range found {
			addrs = append(addrs, f.address)
		}
		addrs = append(addrs, unlisted...)
		return target{}, control.Errorf(api.ErrorKindConflict, "%q names agents on several hosts: %s; send to one by label/name", to, strings.Join(addrs, ", "))
	case len(unlisted) == 1:
		host, _, _ := strings.Cut(unlisted[0], "/")
		return target{}, control.Errorf(api.ErrorKindUnavailable, "%q is on %s, which is not in the peer list: add %s as a peer", to, host, host)
	case len(found) == 0:
		return target{}, control.Errorf(api.ErrorKindNotFound, "no agent named %q on this hab or in any peer's catalog; send to label/name if the agent is new", to)
	}
	return found[0], nil
}

// ClashLabel returns where a name is in use on a peer: the label (or host id) of the first
// catalog copy, stale ones included, that lists an agent with that name; "" when none does.
func ClashLabel(l api.CatalogList, self, name string) string {
	for _, e := range l.Hosts {
		if e.Own || e.Catalog.DeviceID == self {
			continue
		}
		for _, a := range e.Catalog.Agents {
			if a.Session == name {
				if e.Label != "" {
					return e.Label
				}
				return e.Catalog.Host
			}
		}
	}
	return ""
}

// --- Records ---

func rank(s api.ChannelState) int {
	switch s {
	case api.ChannelStateQueued:
		return 0
	case api.ChannelStateHanded:
		return 1
	}
	return 2
}

// Final reports whether a state is final.
func Final(s api.ChannelState) bool { return rank(s) == 2 }

// setStateLocked moves a record of a message sent to device forward. It returns the record to
// publish, or nothing when the change does not apply.
func (s *Service) setStateLocked(id, device string, st api.ChannelState, reason string, now time.Time) []api.ChannelRecord {
	r := s.records[id]
	if r == nil || r.ToDevice != device || rank(st) <= rank(r.State) {
		return nil
	}
	r.State, r.Reason, r.At = st, reason, stamp(now)
	s.broadcastLocked()
	return []api.ChannelRecord{r.ChannelRecord}
}

func (s *Service) publish(recs []api.ChannelRecord) {
	if s.cfg.Publish == nil {
		return
	}
	for _, r := range recs {
		s.cfg.Publish(r)
	}
}

// Status returns a record, after waiting up to p.WaitMS for its final state.
func (s *Service) Status(ctx context.Context, p api.ChannelStatusParams) (api.ChannelStatusResult, error) {
	if p.ID == "" {
		return api.ChannelStatusResult{}, control.Errorf(api.ErrorKindInvalid, "channel.status needs an id")
	}
	wait := min(time.Duration(max(p.WaitMS, 0))*time.Millisecond, MaxWait)
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		s.mu.Lock()
		r := s.records[p.ID]
		ch := s.changed
		var rec api.ChannelRecord
		if r != nil {
			rec = r.ChannelRecord
		}
		s.mu.Unlock()
		if r == nil {
			return api.ChannelStatusResult{}, control.Errorf(api.ErrorKindNotFound, "no message %s was sent from this hab in the last 7 days", p.ID)
		}
		if wait == 0 || Final(rec.State) {
			return api.ChannelStatusResult{Record: rec}, nil
		}
		select {
		case <-ch:
		case <-t.C:
			return api.ChannelStatusResult{Record: rec}, nil
		case <-ctx.Done():
			return api.ChannelStatusResult{Record: rec}, nil
		}
	}
}

// --- Receive ---

// acceptLocked takes one message from the hab with device id from (mesh.md 6.5, Receive and
// dedup). A repeated id gets its first result again.
func (s *Service) acceptLocked(from string, m api.ChannelMessage, now time.Time) api.MailResult {
	if prev, ok := s.seen[m.ID]; ok {
		return prev.Result
	}
	res := api.MailResult{Kind: api.MailKindMessage, ID: m.ID, State: api.ChannelStateHanded}
	n := 0
	for _, it := range s.inbox {
		if it.Message.To.Session == m.To.Session {
			n++
		}
	}
	switch {
	case m.ID == "":
		return api.MailResult{Kind: api.MailKindMessage, State: api.ChannelStateRefused, Reason: "a message with no id"}
	case m.From.DeviceID != from:
		res.State, res.Reason = api.ChannelStateRefused, fmt.Sprintf("the message is from %s, the link is %s: habs hand over only their own mail", m.From.DeviceID, from)
	case m.To.DeviceID != s.cfg.Self:
		res.State, res.Reason = api.ChannelStateRefused, fmt.Sprintf("the message is for %s, not this hab", m.To.DeviceID)
	case !s.cfg.Local.HasSession(m.To.Session):
		res.State, res.Reason = api.ChannelStateRefused, fmt.Sprintf("no session named %s on this host", m.To.Session)
	case n >= MaxInbox:
		res.State, res.Reason = api.ChannelStateRefused, fmt.Sprintf("inbox full: %d messages wait for %s", MaxInbox, m.To.Session)
	default:
		s.inbox = append(s.inbox, &inItem{Message: m, At: now})
	}
	s.seen[m.ID] = seenID{At: now, Result: res}
	return res
}

// receive takes items from the hab with device id from, by push or pull, and returns one result
// per item.
func (s *Service) receive(from string, items []api.MailItem) []api.MailResult {
	now := s.cfg.Now()
	out := make([]api.MailResult, 0, len(items))
	var pub []api.ChannelRecord
	s.mu.Lock()
	for _, it := range items {
		switch {
		case it.Kind == api.MailKindMessage && it.Message != nil:
			out = append(out, s.acceptLocked(from, *it.Message, now))
		case it.Kind == api.MailKindStatus && it.Status != nil:
			st := it.Status
			pub = append(pub, s.setStateLocked(st.ID, from, st.State, st.Reason, now)...)
			out = append(out, api.MailResult{Kind: api.MailKindStatus, ID: st.ID, State: st.State})
		default:
			id := ""
			if it.Message != nil {
				id = it.Message.ID
			} else if it.Status != nil {
				id = it.Status.ID
			}
			out = append(out, api.MailResult{Kind: it.Kind, ID: id, State: api.ChannelStateRefused, Reason: fmt.Sprintf("unknown item kind %q", it.Kind)})
		}
	}
	s.saveInboxLocked()
	s.saveLedgerLocked()
	s.broadcastLocked()
	s.mu.Unlock()
	s.publish(pub)
	s.Poke()
	return out
}

// Push takes items a peer link handed over (mail.push).
func (s *Service) Push(from api.PeerHello, p api.MailPushParams) (api.MailPushResult, error) {
	if err := s.checkLink(from, len(p.Items)); err != nil {
		return api.MailPushResult{}, err
	}
	return api.MailPushResult{Results: s.receive(from.DeviceID, p.Items)}, nil
}

// Pull applies the results of the caller's last pull and returns the items waiting for it
// (mail.pull).
func (s *Service) Pull(from api.PeerHello, p api.MailPullParams) (api.MailPullResult, error) {
	if err := s.checkLink(from, len(p.Done)); err != nil {
		return api.MailPullResult{}, err
	}
	now := s.cfg.Now()
	s.mu.Lock()
	pub := s.applyResultsLocked(from.DeviceID, p.Done, now)
	items := s.headLocked(from.DeviceID)
	s.mu.Unlock()
	s.publish(pub)
	return api.MailPullResult{Items: items}, nil
}

func (s *Service) checkLink(from api.PeerHello, n int) error {
	switch {
	case from.DeviceID == "":
		return control.Errorf(api.ErrorKindInvalid, "mail needs a peer link: client.kind hab with the hello's peer field")
	case from.DeviceID == s.cfg.Self:
		return control.Errorf(api.ErrorKindInvalid, "the peer link claims this hab's own device id")
	case n > Batch:
		return control.Errorf(api.ErrorKindInvalid, "%d items; at most %d in one call", n, Batch)
	}
	return nil
}

// headLocked returns up to Batch items from the front of device's outbox.
func (s *Service) headLocked(device string) []api.MailItem {
	q := s.outbox[device]
	out := make([]api.MailItem, 0, min(len(q), Batch))
	for _, it := range q[:min(len(q), Batch)] {
		out = append(out, it.Item)
	}
	return out
}

// applyResultsLocked removes the items device answered for, and moves the records of the
// messages among them. It saves what changed.
func (s *Service) applyResultsLocked(device string, results []api.MailResult, now time.Time) []api.ChannelRecord {
	if len(results) == 0 {
		return nil
	}
	var pub []api.ChannelRecord
	q := s.outbox[device]
	for _, r := range results {
		i := slices.IndexFunc(q, func(it outItem) bool { return it.Item.Kind == r.Kind && itemID(it.Item) == r.ID })
		if i < 0 {
			continue // answered already, by push and pull both
		}
		q = slices.Delete(q, i, i+1)
		if r.Kind == api.MailKindMessage && r.ID != "" {
			pub = append(pub, s.setStateLocked(r.ID, device, r.State, r.Reason, now)...)
		}
	}
	if len(q) == 0 {
		delete(s.outbox, device)
	} else {
		s.outbox[device] = q
	}
	s.saveOutboxLocked(device)
	s.saveLedgerLocked()
	return pub
}

func itemID(it api.MailItem) string {
	switch {
	case it.Message != nil:
		return it.Message.ID
	case it.Status != nil:
		return it.Status.ID
	}
	return ""
}

// --- Delivery ---

// fromAddress is the sender as this hab would address it: label/name with this hab's label for
// the sender's device, else host/name; a bare name for a sender on this hab.
func (s *Service) fromAddress(m api.ChannelMessage) string {
	if m.From.DeviceID == s.cfg.Self {
		return m.From.Session
	}
	if s.cfg.Peers != nil {
		for _, p := range s.cfg.Peers.List().Peers {
			if p.DeviceID == m.From.DeviceID && p.Label != "" {
				return p.Label + "/" + m.From.Session
			}
		}
	}
	host := m.From.Host
	if host == "" {
		host = m.From.DeviceID
	}
	return host + "/" + m.From.Session
}

// Envelope wraps text from an agent at address from, so the receiving agent knows it is not
// from the owner (mesh.md 6.5, The envelope). It is one paste.
func Envelope(from, text string) string {
	return fmt.Sprintf("[Flightdeck: message from agent %s, not from the owner. Reply with: gff-hab send %s \"your reply\"]\n%s", from, from, text)
}

func (s *Service) deliverLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Retry)
	defer t.Stop()
	for {
		s.deliverRound()
		select {
		case <-s.done:
			return
		case <-t.C:
		case <-s.poke:
		}
	}
}

// deliverRound tries the oldest message of each session that has none in flight.
func (s *Service) deliverRound() {
	s.mu.Lock()
	var heads []*inItem
	taken := map[string]bool{}
	for _, it := range s.inbox {
		sess := it.Message.To.Session
		if taken[sess] {
			continue
		}
		taken[sess] = true
		if !it.busy {
			it.busy = true
			heads = append(heads, it)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, it := range heads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.deliver(it)
		}()
	}
	wg.Wait()
}

func (s *Service) deliver(it *inItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := it.Message
	err := s.cfg.Local.Deliver(ctx, m.To.Session, Envelope(s.fromAddress(m), m.Text))
	now := s.cfg.Now()
	var st api.ChannelState
	reason := ""
	switch {
	case err == nil:
		st = api.ChannelStateDelivered
	case errors.Is(err, ErrBusy):
	case errors.Is(err, ErrNoState):
		from := it.At
		if s.started.After(from) {
			from = s.started
		}
		if now.Sub(from) >= NoStateGrace {
			st, reason = api.ChannelStateRefused, "agent not running: "+err.Error()
		}
	case errors.Is(err, ErrNotDelivered):
		st, reason = api.ChannelStateNotDelivered, err.Error()
	default:
		st, reason = api.ChannelStateRefused, err.Error()
	}
	s.mu.Lock()
	it.busy = false
	var pub []api.ChannelRecord
	if st != "" {
		pub = s.finishLocked(it, st, reason, now)
		s.cfg.Logf("channels: message %s for %s: %s %s", m.ID, m.To.Session, st, reason)
	} else {
		s.broadcastLocked() // a wait that found it busy may take it now
	}
	s.mu.Unlock()
	s.publish(pub)
	if st != "" {
		s.Poke() // the next message for the session
	}
}

// finishLocked removes an inbox item with its final state and sends the state to the sender.
func (s *Service) finishLocked(it *inItem, st api.ChannelState, reason string, now time.Time) []api.ChannelRecord {
	i := slices.Index(s.inbox, it)
	if i < 0 {
		return nil
	}
	s.inbox = slices.Delete(s.inbox, i, i+1)
	s.saveInboxLocked()
	m := it.Message
	var pub []api.ChannelRecord
	if m.From.DeviceID == s.cfg.Self {
		pub = s.setStateLocked(m.ID, s.cfg.Self, st, reason, now)
		s.saveLedgerLocked()
	} else {
		dev := m.From.DeviceID
		s.outbox[dev] = append(s.outbox[dev], outItem{
			Item: api.MailItem{Kind: api.MailKindStatus, Status: &api.ChannelStatus{ID: m.ID, State: st, Reason: reason, At: stamp(now)}},
			At:   now,
		})
		s.saveOutboxLocked(dev)
		s.Kick()
	}
	s.broadcastLocked()
	return pub
}

// --- Wait ---

// Wait waits for a message to p.Session, or for p.Agent to reach a state in p.For (mesh.md
// 6.4). A message it returns counts as delivered.
func (s *Service) Wait(ctx context.Context, p api.ChannelWaitParams) (api.ChannelWaitResult, error) {
	if p.Session == "" && p.Agent == "" {
		return api.ChannelWaitResult{}, control.Errorf(api.ErrorKindInvalid, "channel.wait needs a session, an agent, or both")
	}
	if p.Session != "" && !s.cfg.Local.HasSession(p.Session) {
		return api.ChannelWaitResult{}, control.Errorf(api.ErrorKindNotFound, "no session named %q on this hab", p.Session)
	}
	wait := DefaultWait
	if p.TimeoutMS > 0 {
		wait = min(time.Duration(p.TimeoutMS)*time.Millisecond, MaxWait)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	var first api.AgentStatus
	firstOK := false
	for i := 0; ; i++ {
		s.mu.Lock()
		ch := s.changed
		if p.Session != "" {
			for _, it := range s.inbox {
				if it.Message.To.Session != p.Session {
					continue
				}
				if it.busy {
					break // the oldest is being written to the PTY; keep the order
				}
				m := it.Message
				pub := s.finishLocked(it, api.ChannelStateDelivered, "", s.cfg.Now())
				s.mu.Unlock()
				s.publish(pub)
				return api.ChannelWaitResult{Message: &api.ChannelInbound{ID: m.ID, From: s.fromAddress(m), Text: m.Text, ReplyTo: m.ReplyTo, At: m.At}}, nil
			}
		}
		s.mu.Unlock()
		var agentCh <-chan struct{}
		if p.Agent != "" {
			st, ok, changed := s.cfg.Local.Agent(p.Agent)
			if !ok {
				st = api.AgentStatusExited
			}
			if i == 0 {
				first, firstOK = st, ok
			}
			done := slices.Contains(p.For, st) || (len(p.For) == 0 && i > 0 && (st != first || ok != firstOK))
			if done {
				return api.ChannelWaitResult{Agent: &api.ChannelAgent{Session: p.Agent, State: st}}, nil
			}
			if ok {
				agentCh = changed
			}
		}
		select {
		case <-ch:
		case <-agentCh:
		case <-t.C:
			return api.ChannelWaitResult{TimedOut: true}, nil
		case <-ctx.Done():
			return api.ChannelWaitResult{TimedOut: true}, nil
		}
	}
}

// --- Mail rounds ---

func (s *Service) mailLoop() {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	t := time.NewTicker(s.cfg.Poll)
	defer t.Stop()
	for {
		s.expire()
		s.mailRound(ctx)
		select {
		case <-s.done:
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// mailRound pushes and pulls over every online link with channels.
func (s *Service) mailRound(ctx context.Context) {
	if s.cfg.Peers == nil {
		return
	}
	now := s.cfg.Now()
	var wg sync.WaitGroup
	for _, l := range s.cfg.Peers.Links() {
		up := l.Online && l.Caller != nil && l.DeviceID != "" && l.DeviceID != s.cfg.Self && slices.Contains(l.Caps, Capability)
		s.mu.Lock()
		if up && !s.online[l.Label] {
			delete(s.fails, l.Label) // back online: try at once
		}
		s.online[l.Label] = up
		f := s.fails[l.Label]
		s.mu.Unlock()
		if !up || (f != nil && now.Before(f.next)) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.exchange(ctx, l)
		}()
	}
	wg.Wait()
}

// exchange pushes the link's outbox, then pulls until nothing comes.
func (s *Service) exchange(ctx context.Context, l Link) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cl := api.Client{Caller: l.Caller}
	for {
		s.mu.Lock()
		items := s.headLocked(l.DeviceID)
		s.mu.Unlock()
		if len(items) == 0 {
			break
		}
		res, err := cl.MailPush(ctx, api.MailPushParams{Items: items})
		if err == nil && len(res.Results) != len(items) {
			err = fmt.Errorf("%d results for %d items", len(res.Results), len(items))
		}
		if err != nil {
			s.failed(l, "push to", err)
			return
		}
		s.mu.Lock()
		pub := s.applyResultsLocked(l.DeviceID, res.Results, s.cfg.Now())
		s.mu.Unlock()
		s.publish(pub)
		if len(items) < Batch {
			break
		}
	}
	for range pullRounds {
		s.mu.Lock()
		done := s.acks[l.Label]
		s.mu.Unlock()
		res, err := cl.MailPull(ctx, api.MailPullParams{Done: done})
		if err != nil {
			s.failed(l, "pull from", err)
			return
		}
		s.mu.Lock()
		delete(s.acks, l.Label)
		s.mu.Unlock()
		if len(res.Items) == 0 {
			break
		}
		results := s.receive(l.DeviceID, res.Items)
		s.mu.Lock()
		s.acks[l.Label] = results
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.fails[l.Label] != nil {
		s.cfg.Logf("channels: %s answers again", l.Label)
		delete(s.fails, l.Label)
	}
	s.mu.Unlock()
}

// failed records a failed exchange: the link waits 1 s, doubled for each failure in a row up to
// a minute. Only the first failure in a row is logged.
func (s *Service) failed(l Link, what string, err error) {
	s.mu.Lock()
	f := s.fails[l.Label]
	if f == nil {
		f = &failure{}
		s.fails[l.Label] = f
		s.cfg.Logf("channels: %s %s: %v", what, l.Label, err)
	}
	f.n++
	f.next = s.cfg.Now().Add(min(minBackoff<<min(f.n-1, 16), maxBackoff))
	s.mu.Unlock()
}

// expire drops outbox items, records and ids older than Keep. A dropped message's record turns
// expired.
func (s *Service) expire() {
	now := s.cfg.Now()
	var pub []api.ChannelRecord
	s.mu.Lock()
	ledger := false
	for dev, q := range s.outbox {
		keep := q[:0]
		for _, it := range q {
			if now.Sub(it.At) < Keep {
				keep = append(keep, it)
				continue
			}
			if it.Item.Kind == api.MailKindMessage && it.Item.Message != nil {
				pub = append(pub, s.setStateLocked(it.Item.Message.ID, dev, api.ChannelStateExpired, "not handed over within 7 days", now)...)
				ledger = true
			}
		}
		if len(keep) != len(q) {
			if len(keep) == 0 {
				delete(s.outbox, dev)
			} else {
				s.outbox[dev] = keep
			}
			s.saveOutboxLocked(dev)
		}
	}
	for id, r := range s.records {
		if now.Sub(r.Sent) >= Keep && !slices.ContainsFunc(s.outbox[r.ToDevice], func(it outItem) bool { return itemID(it.Item) == id }) {
			delete(s.records, id)
			ledger = true
		}
	}
	for id, v := range s.seen {
		if now.Sub(v.At) >= Keep {
			delete(s.seen, id)
			ledger = true
		}
	}
	if ledger {
		s.saveLedgerLocked()
	}
	s.mu.Unlock()
	s.publish(pub)
}
