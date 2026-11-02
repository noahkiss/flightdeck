// Package channels carries text from one agent to another, on any host (mesh.md 6). A send
// resolves an address, makes a message with a random id, and puts it in the outbox for the
// target hab, or straight in the inbox when the target is on this hab. Outboxes are handed over
// with mail.push and mail.pull over peer links; the receiver drops a repeated id. The inbox
// delivers each message into its agent's PTY, wrapped in an envelope, only when the agent may
// take it. Outboxes, the inbox, the sent records and the received ids are one state, saved to
// disk as one file before any answer, so a gff-hab restart loses nothing and repeats nothing.
package channels

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// Capability is the capability word of a hab that serves channels.
const Capability = "channels"

// Limits and times (mesh.md 6.5).
const (
	MaxText       = 64 << 10           // bytes of text in one message
	MaxOutbox     = 1000               // messages in one target's outbox
	MaxInbox      = 1000               // messages waiting for one session
	MaxStoredText = 16 << 20           // bytes of text in every outbox and the inbox together
	MaxRecords    = 10000              // sent records kept
	MaxSeen       = 10000              // received ids kept
	Keep          = 7 * 24 * time.Hour // records, ids, outbox and inbox items
	Batch         = 100                // items in one mail.push or mail.pull
	DefaultWait   = 180 * time.Second  // channel.wait with no timeout, from a client
	PaneWait      = 110 * time.Second  // channel.wait with no timeout, from a pane: under a harness's 2 min tool limit
	MaxWait       = 600 * time.Second  // the longest channel.wait or channel.status wait
	// NoStateGrace is how long a message waits for a pane with no agent state at all before it
	// is refused: a hab that just restarted learns the states again from hooks and reports.
	NoStateGrace = 30 * time.Second
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	pullRounds   = 20 // pulls in one exchange while items keep coming
)

// NameRule is the charset of a session name that can send and receive channel mail
// (mesh.md 6.5). gff-hab gives every new or renamed session a name that fits it.
const NameRule = "1 to 64 of A-Z a-z 0-9 . _ -"

// SelfRefusal is the message of channel.send's refusal of a message from a session to itself.
// Callers that pick a target (hand_off, capcom.md 6.5) match it to skip the caller.
const SelfRefusal = "a session cannot send channel mail to itself"

var (
	validName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	validID   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// marker finds envelope markers inside a message's text, to neutralise them.
	// marker finds a begin or end line inside a message's text, after Envelope has dropped
	// invisible format characters (\p{Cf}). Unicode spaces (\p{Z}) count as spaces, and the
	// words may run together, so "[end\u00a0agent message" and "[endagentmessage" are caught.
	marker = regexp.MustCompile(`(?i)\[([\s\p{Z}]*)(begin|end)([\s\p{Z}]*(?:agent|start)[\s\p{Z}]*message)`)
)

// ValidName reports whether a session or host name fits NameRule.
func ValidName(s string) bool { return validName.MatchString(s) }

// Why a delivery did not happen. Local.Check and Local.Deliver wrap one of these; any other
// error refuses the message with the error's text.
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
	// Session returns the id of the session with this name on this hab.
	Session(name string) (id string, ok bool)
	// Check says whether the session's agent may take a message now, writing nothing: nil, or
	// an error wrapping ErrBusy or ErrNoState, or any other error to refuse the message.
	Check(sessionID string) error
	// Deliver writes one message to the session's agent now, or returns why not, as Check.
	Deliver(ctx context.Context, sessionID, text string) error
	// Agent returns the state of the named session's agent, with ok false when the session is
	// gone, and a channel that is closed at the agent's next change.
	Agent(name string) (st api.AgentStatus, ok bool, changed <-chan struct{})
}

// Config is what New needs. Self, Host and Local are required.
type Config struct {
	Self    string // this hab's device id
	Host    string // this hab's host id
	State   string // the state directory; "" keeps nothing on disk
	Peers   Peers  // nil: only sessions on this hab are reachable
	Catalog func() api.CatalogList
	Local   Local
	// Publish hears every change of a record this hab sent (channel.message), in order. It is
	// called with the service's lock held, so it must not call back into the service.
	Publish func(api.ChannelRecord)
	// Wake sends mesh.changed to every peer link that the hab with this device id dialed to
	// this one, and reports whether any link took it (mesh.md 6.5). Nil: no wake-ups, and a
	// peer that only pulls sees new mail at its next mail round.
	Wake  func(device string) bool
	Poll  time.Duration // the mail round; default 15 s
	Retry time.Duration // the delivery round; default 1 s
	Now   func() time.Time
	Logf  func(string, ...any)
	// write saves the state file; tests replace it to fail. Default writeSynced.
	write func(path string, b []byte) error
}

// Service is the channels of one hab.
type Service struct {
	cfg     Config
	started time.Time

	mu       sync.Mutex
	st       state
	lastGood []byte                      // the state as last saved; a failed save returns to it
	pending  []api.ChannelRecord         // record changes to publish once saved
	acks     map[string][]api.MailResult // by link label
	fails    map[string]*failure         // by link label
	online   map[string]bool             // by link label
	changed  chan struct{}               // closed and replaced at every change a wait may want
	// settle holds the final state of an inbox item whose write started but whose finish is not
	// saved yet, by message id: items found marked at load (not_delivered), and items whose
	// finish save failed (the outcome the write had). A delivery round saves them, and writes
	// none of them again. It lives in memory only, so a failed save cannot undo it.
	settle map[string]outcome

	// The mesh.changed wake-up (mesh.md 6.5). Sender side, by target device id: fresh holds
	// outboxes that gained an item not saved yet; woken holds devices told since their last
	// pull, so a burst sends one wake-up and an idle hab sends none; toWake is what the wake
	// loop sends next. Receiver side, by link label: wanted holds links whose peer said mail
	// waits; running holds links with an exchange in flight, and again those whose exchange
	// runs once more when it ends, so a link never has two pulls in flight.
	fresh, woken, toWake    map[string]bool
	wanted, running, again  map[string]bool
	kick, poke, wake, pulls chan struct{}
	done                    chan struct{}
	ctx                     context.Context // ends at Close
	stop                    context.CancelFunc
	wg                      sync.WaitGroup
}

// state is everything channels keep, saved whole as one file.
type state struct {
	Outbox  map[string][]outItem `json:"outbox"`  // by target device id
	Records map[string]*record   `json:"records"` // messages this hab sent, by id
	Seen    map[string]seenID    `json:"seen"`    // ids this hab received, by id
	Inbox   []*inItem            `json:"inbox"`   // in arrival order
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
	Message   api.ChannelMessage `json:"message"`
	At        time.Time          `json:"at"`
	SessionID string             `json:"session_id"` // the target session when it arrived
	// Attempting is saved before the PTY write: an item found with it set and no write in
	// flight was being written when the hab stopped, and finishes not_delivered.
	Attempting bool `json:"attempting,omitempty"`
	// Kind marks an item this hab made itself rather than a message from an agent: a seed
	// (kindSeed) or a note from Flightdeck (kindNote). It picks the envelope.
	Kind string `json:"kind,omitempty"`
	busy bool   // a delivery or a wait has it now
}

// The kinds of an inbox item this hab made itself (capcom.md 3.3, 6.1).
const (
	kindSeed = "seed" // a new session's first message, written on the owner's behalf
	kindNote = "note" // a message from Flightdeck itself
)

type outcome struct {
	state  api.ChannelState
	reason string
}

// stoppedWriting is the outcome of an item found marked at load.
var stoppedWriting = outcome{api.ChannelStateNotDelivered, "the hab stopped while writing it to the agent; not retried"}

type failure struct {
	n    int
	next time.Time
}

func newState() state {
	return state{Outbox: map[string][]outItem{}, Records: map[string]*record{}, Seen: map[string]seenID{}}
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
	if cfg.write == nil {
		cfg.write = writeSynced
	}
	s := &Service{
		cfg: cfg, started: cfg.Now(), st: newState(),
		acks: map[string][]api.MailResult{}, fails: map[string]*failure{}, online: map[string]bool{},
		changed: make(chan struct{}), settle: map[string]outcome{},
		kick: make(chan struct{}, 1), poke: make(chan struct{}, 1), done: make(chan struct{}),
		fresh: map[string]bool{}, woken: map[string]bool{}, toWake: map[string]bool{},
		wanted: map[string]bool{}, running: map[string]bool{}, again: map[string]bool{},
		wake: make(chan struct{}, 1), pulls: make(chan struct{}, 1),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.wg.Add(4)
	go s.mailLoop()
	go s.deliverLoop()
	go s.wakeLoop()
	go s.pullLoop()
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
	s.stop()
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

// storedTextLocked is the text held in every outbox and the inbox.
func (s *Service) storedTextLocked() int {
	n := 0
	for _, q := range s.st.Outbox {
		for _, it := range q {
			if it.Item.Message != nil {
				n += len(it.Item.Message.Text)
			}
		}
	}
	for _, it := range s.st.Inbox {
		n += len(it.Message.Text)
	}
	return n
}

// --- Send ---

// target is a resolved address.
type target struct {
	device  string
	session string
	address string
	label   string // the peer label; "" for this hab
}

// Send makes a message from p.From, a session on this hab, to p.To, and hands it to this hab's
// inbox or puts it in the target's outbox. Nothing is answered before the state is on disk.
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
	case p.ReplyTo != "" && !validID.MatchString(p.ReplyTo):
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "reply_to %q is not a message id", p.ReplyTo)
	case !ValidName(s.cfg.Host):
		return api.ChannelSent{}, control.Errorf(api.ErrorKindUnavailable, "this hab's host id %q is outside %s; start gff-hab with a -host-id that fits to send channel mail", s.cfg.Host, NameRule)
	}
	if _, ok := s.cfg.Local.Session(p.From); !ok {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindNotFound, "no session named %q on this hab to send from", p.From)
	}
	if !ValidName(p.From) {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "session name %q is outside %s; rename the session to send channel mail", p.From, NameRule)
	}
	t, err := s.resolve(p.To)
	if err != nil {
		return api.ChannelSent{}, err
	}
	if !ValidName(t.session) {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "%q is not a session name: use %s", t.session, NameRule)
	}
	if t.device == s.cfg.Self && t.session == p.From {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindInvalid, "%s", SelfRefusal)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.device != s.cfg.Self {
		if messagesIn(s.st.Outbox[t.device]) >= MaxOutbox {
			return api.ChannelSent{}, control.Errorf(api.ErrorKindConflict, "outbox full for %s: %d messages wait; send again once it is online", t.label, MaxOutbox)
		}
		if s.storedTextLocked()+len(p.Text) > MaxStoredText {
			return api.ChannelSent{}, control.Errorf(api.ErrorKindConflict, "outbox full: messages waiting on this hab hold %d MiB of text", MaxStoredText>>20)
		}
	}
	s.st.Records[msg.ID] = rec
	s.pending = append(s.pending, rec.ChannelRecord)
	if t.device == s.cfg.Self {
		res := s.acceptLocked(s.cfg.Self, msg, now)
		s.setStateLocked(msg.ID, t.device, res.State, res.Reason, now)
	} else {
		s.st.Outbox[t.device] = append(s.st.Outbox[t.device], outItem{Item: api.MailItem{Kind: api.MailKindMessage, Message: &msg}, At: now})
		s.freshLocked(t.device)
	}
	s.capRecordsLocked()
	s.capSeenLocked()
	if err := s.commitLocked(); err != nil {
		return api.ChannelSent{}, control.Errorf(api.ErrorKindUnavailable, "the message was not sent: %v", err)
	}
	if t.device == s.cfg.Self {
		s.Poke()
	} else {
		s.Kick()
	}
	return api.ChannelSent{ID: msg.ID, To: t.address, State: s.st.Records[msg.ID].State}, nil
}

// messagesIn counts the messages in an outbox; status items do not count toward MaxOutbox.
func messagesIn(q []outItem) int {
	n := 0
	for _, it := range q {
		if it.Item.Kind == api.MailKindMessage {
			n++
		}
	}
	return n
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
	if _, ok := s.cfg.Local.Session(to); ok {
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

// setStateLocked moves a record of a message sent to device forward, and queues it to publish
// once saved. A change for another device or an earlier state is ignored.
func (s *Service) setStateLocked(id, device string, st api.ChannelState, reason string, now time.Time) {
	r := s.st.Records[id]
	if r == nil || r.ToDevice != device || rank(st) <= rank(r.State) {
		return
	}
	r.State, r.Reason, r.At = st, reason, stamp(now)
	s.pending = append(s.pending, r.ChannelRecord)
}

// capRecordsLocked keeps at most MaxRecords records, dropping the oldest finished ones first,
// then the oldest.
func (s *Service) capRecordsLocked() {
	over := len(s.st.Records) - MaxRecords
	if over <= 0 {
		return
	}
	ids := make([]string, 0, len(s.st.Records))
	for id := range s.st.Records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.st.Records[ids[i]], s.st.Records[ids[j]]
		if fa, fb := Final(a.State), Final(b.State); fa != fb {
			return fa
		}
		return a.Sent.Before(b.Sent)
	})
	for _, id := range ids[:over] {
		delete(s.st.Records, id)
	}
}

// capSeenLocked keeps at most MaxSeen received ids, dropping refused ones first, then the
// oldest. A refused id sent again is refused again or taken once, so it is the cheapest to lose.
func (s *Service) capSeenLocked() {
	over := len(s.st.Seen) - MaxSeen
	if over <= 0 {
		return
	}
	ids := make([]string, 0, len(s.st.Seen))
	for id := range s.st.Seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.st.Seen[ids[i]], s.st.Seen[ids[j]]
		if ra, rb := a.Result.State == api.ChannelStateRefused, b.Result.State == api.ChannelStateRefused; ra != rb {
			return ra
		}
		return a.At.Before(b.At)
	})
	for _, id := range ids[:over] {
		delete(s.st.Seen, id)
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
		r := s.st.Records[p.ID]
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
	if !validID.MatchString(m.ID) {
		return api.MailResult{Kind: api.MailKindMessage, ID: m.ID, State: api.ChannelStateRefused, Reason: "the message id is not 32 hex digits"}
	}
	if prev, ok := s.st.Seen[m.ID]; ok {
		return prev.Result
	}
	res := api.MailResult{Kind: api.MailKindMessage, ID: m.ID, State: api.ChannelStateHanded}
	if s.findLocked(m.ID) != nil {
		return res // in the inbox already; the ledger lost it
	}
	refuse := func(format string, args ...any) {
		res.State, res.Reason = api.ChannelStateRefused, fmt.Sprintf(format, args...)
	}
	n := 0
	for _, it := range s.st.Inbox {
		if it.Message.To.Session == m.To.Session {
			n++
		}
	}
	sessionID, ok := s.cfg.Local.Session(m.To.Session)
	switch {
	case m.From.DeviceID != from:
		refuse("the message is from %s, the link is %s: habs hand over only their own mail", m.From.DeviceID, from)
	case m.To.DeviceID != s.cfg.Self:
		refuse("the message is for %s, not this hab", m.To.DeviceID)
	case !ValidName(m.From.Session) || !ValidName(m.From.Host):
		refuse("the sender's name %q on host %q is outside %s", m.From.Session, m.From.Host, NameRule)
	case strings.TrimSpace(m.Text) == "" || len(m.Text) > MaxText:
		refuse("the message is %d bytes; it must hold text, at most %d bytes", len(m.Text), MaxText)
	case !ok:
		refuse("no session named %s on this host", m.To.Session)
	case !ValidName(m.To.Session):
		refuse("session name %q is outside %s; rename it to receive channel mail", m.To.Session, NameRule)
	case n >= MaxInbox:
		refuse("inbox full: %d messages wait for %s", MaxInbox, m.To.Session)
	case s.storedTextLocked()+len(m.Text) > MaxStoredText:
		refuse("inbox full: messages waiting on this hab hold %d MiB of text", MaxStoredText>>20)
	default:
		s.st.Inbox = append(s.st.Inbox, &inItem{Message: m, At: now, SessionID: sessionID})
	}
	// A message from this hab is never sent again, so its id protects nothing; keeping it would
	// push remote ids, still retried, out of the capped list.
	if from != s.cfg.Self {
		s.st.Seen[m.ID] = seenID{At: now, Result: res}
	}
	return res
}

func (s *Service) findLocked(id string) *inItem {
	for _, it := range s.st.Inbox {
		if it.Message.ID == id {
			return it
		}
	}
	return nil
}

// receive takes items from the hab with device id from, by push or pull, and returns one result
// per item once they are on disk. A failed save fails the call, so the sender keeps the items.
func (s *Service) receive(from string, items []api.MailItem) ([]api.MailResult, error) {
	now := s.cfg.Now()
	out := make([]api.MailResult, 0, len(items))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		switch {
		case it.Kind == api.MailKindMessage && it.Message != nil:
			out = append(out, s.acceptLocked(from, *it.Message, now))
		case it.Kind == api.MailKindStatus && it.Status != nil:
			st := it.Status
			s.setStateLocked(st.ID, from, st.State, st.Reason, now)
			out = append(out, api.MailResult{Kind: api.MailKindStatus, ID: st.ID, State: st.State})
		default:
			out = append(out, api.MailResult{Kind: it.Kind, ID: itemID(it), State: api.ChannelStateRefused, Reason: fmt.Sprintf("unknown item kind %q", it.Kind)})
		}
	}
	s.capSeenLocked()
	if err := s.commitLocked(); err != nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "the mail was not saved: %v", err)
	}
	s.Poke()
	return out, nil
}

// Push takes items a peer link handed over (mail.push).
func (s *Service) Push(from api.PeerHello, p api.MailPushParams) (api.MailPushResult, error) {
	if err := s.checkLink(from, len(p.Items)); err != nil {
		return api.MailPushResult{}, err
	}
	res, err := s.receive(from.DeviceID, p.Items)
	if err != nil {
		return api.MailPushResult{}, err
	}
	return api.MailPushResult{Results: res}, nil
}

// Pull applies the results of the caller's last pull and returns the items waiting for it
// (mail.pull).
func (s *Service) Pull(from api.PeerHello, p api.MailPullParams) (api.MailPullResult, error) {
	if err := s.checkLink(from, len(p.Done)); err != nil {
		return api.MailPullResult{}, err
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// The caller pulls: an item queued for it from now on wakes it again (mesh.md 6.5).
	delete(s.woken, from.DeviceID)
	if s.applyResultsLocked(from.DeviceID, p.Done, now) > 0 {
		if err := s.commitLocked(); err != nil {
			return api.MailPullResult{}, control.Errorf(api.ErrorKindUnavailable, "the results were not saved: %v", err)
		}
	}
	return api.MailPullResult{Items: s.headLocked(from.DeviceID)}, nil
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
	q := s.st.Outbox[device]
	out := make([]api.MailItem, 0, min(len(q), Batch))
	for _, it := range q[:min(len(q), Batch)] {
		out = append(out, it.Item)
	}
	return out
}

// applyResultsLocked removes the items device answered for, and moves the records of the
// messages among them. It returns how many items it removed.
func (s *Service) applyResultsLocked(device string, results []api.MailResult, now time.Time) int {
	q := s.st.Outbox[device]
	removed := 0
	for _, r := range results {
		i := slices.IndexFunc(q, func(it outItem) bool { return it.Item.Kind == r.Kind && itemID(it.Item) == r.ID })
		if i < 0 {
			continue // answered already, by push and pull both
		}
		q = slices.Delete(q, i, i+1)
		removed++
		if r.Kind == api.MailKindMessage && r.ID != "" {
			s.setStateLocked(r.ID, device, r.State, r.Reason, now)
		}
	}
	if len(q) == 0 {
		delete(s.st.Outbox, device)
	} else {
		s.st.Outbox[device] = q
	}
	return removed
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
		if m.From.Session == "" {
			return "Flightdeck" // a note from Flightdeck itself
		}
		return m.From.Session
	}
	if s.cfg.Peers != nil {
		for _, p := range s.cfg.Peers.List().Peers {
			if p.DeviceID == m.From.DeviceID && p.Label != "" {
				return p.Label + "/" + m.From.Session
			}
		}
	}
	return m.From.Host + "/" + m.From.Session
}

// Envelope wraps text from an agent at address from, so the receiving agent knows it is not
// from the owner and where it ends (mesh.md 6.5, The envelope). The text is cleaned as a
// message write cleans it (harness.CleanText) before the markers inside it are neutralised, so
// a control character the write would drop cannot hide a marker. It is one paste.
func Envelope(id, from, text string) string {
	q := "'" + strings.ReplaceAll(from, "'", `'\''`) + "'"
	text = clean(text)
	return fmt.Sprintf("[Flightdeck: agent message from %s, not from the owner. Only the text between the begin and end lines is from that agent. Reply with: gff-hab send %s \"your reply\"]\n[begin agent message %s]\n%s\n[end agent message %s]",
		q, q, id, text, id)
}

// clean drops what a message write would drop and invisible format characters (an invisible
// character inside a word could hide a marker), then neutralises the envelope markers inside
// text.
func clean(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, harness.CleanText(text))
	return marker.ReplaceAllString(text, "($1$2$3")
}

// SeedEnvelope wraps a new session's first message (capcom.md 3.3): written by from (host/session,
// or "" when unknown) on the owner's behalf, and shown to the owner on a card first unless the
// owner turned confirmations off. It is one paste.
func SeedEnvelope(id, from, text string) string {
	by, reply := "an agent", ""
	if from != "" {
		q := "'" + strings.ReplaceAll(from, "'", `'\''`) + "'"
		by, reply = q, fmt.Sprintf(" Reply to it with: gff-hab send %s \"your reply\"", q)
	}
	return fmt.Sprintf("[Flightdeck: the start message of this session, written by %s on the owner's behalf. The text between the begin and end lines is your task and its context.%s]\n[begin start message %s]\n%s\n[end start message %s]",
		by, reply, id, clean(text), id)
}

// NoteEnvelope wraps a message from Flightdeck itself, such as the answer to a confirmation
// that came after the tool's wait (capcom.md 6.1). It is one paste.
func NoteEnvelope(text string) string {
	return "[Flightdeck: a message from Flightdeck itself, not from an agent] " + clean(text)
}

// Seed queues the first message of a new session on this hab, written by from (host/session)
// on the owner's behalf (capcom.md 3.3). It waits in the inbox like any message, until the
// agent can take it, and it is saved before Seed returns.
func (s *Service) Seed(session, from, text string) (string, error) {
	return s.inject(session, kindSeed, from, text)
}

// Note queues a message from Flightdeck itself to a session on this hab (capcom.md 6.1).
func (s *Service) Note(session, text string) (string, error) {
	return s.inject(session, kindNote, "", text)
}

func (s *Service) inject(session, kind, from, text string) (string, error) {
	sessionID, ok := s.cfg.Local.Session(session)
	switch {
	case !ok:
		return "", control.Errorf(api.ErrorKindNotFound, "no session named %q on this hab", session)
	case !ValidName(session):
		return "", control.Errorf(api.ErrorKindInvalid, "session name %q is outside %s", session, NameRule)
	case strings.TrimSpace(text) == "" || len(text) > MaxText:
		return "", control.Errorf(api.ErrorKindInvalid, "the message is %d bytes; it must hold text, at most %d bytes", len(text), MaxText)
	}
	now := s.cfg.Now()
	m := api.ChannelMessage{ID: newID(), From: api.ChannelEnd{DeviceID: s.cfg.Self, Host: s.cfg.Host, Session: from},
		To: api.ChannelEnd{DeviceID: s.cfg.Self, Session: session}, Text: text, At: stamp(now)}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.st.Inbox {
		if it.Message.To.Session == session {
			n++
		}
	}
	if n >= MaxInbox || s.storedTextLocked()+len(text) > MaxStoredText {
		return "", control.Errorf(api.ErrorKindConflict, "inbox full for %s", session)
	}
	s.st.Inbox = append(s.st.Inbox, &inItem{Message: m, At: now, SessionID: sessionID, Kind: kind})
	if err := s.commitLocked(); err != nil {
		return "", control.Errorf(api.ErrorKindUnavailable, "the message was not saved: %v", err)
	}
	s.Poke()
	return m.ID, nil
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

// deliverRound first saves the final state of each item in settle, then tries the oldest
// message of each session that has none in flight. An item marked Attempting that is not in
// settle had no write in flight: its mark outlived a failed save, and it is tried again.
func (s *Service) deliverRound() {
	now := s.cfg.Now()
	s.mu.Lock()
	if len(s.settle) > 0 {
		settled := 0
		for id, o := range s.settle {
			if it := s.findLocked(id); it == nil {
				delete(s.settle, id)
			} else if !it.busy {
				s.finishLocked(id, o.state, o.reason, now)
				settled++
			}
		}
		if settled > 0 {
			if err := s.commitLocked(); err != nil {
				s.cfg.Logf("channels: save: %v", err)
			} else {
				for id := range s.settle {
					if s.findLocked(id) == nil {
						delete(s.settle, id)
					}
				}
			}
		}
	}
	var heads []*inItem
	taken := map[string]bool{}
	for _, it := range slices.Clone(s.st.Inbox) {
		sess := it.Message.To.Session
		if taken[sess] {
			continue
		}
		taken[sess] = true
		if _, ok := s.settle[it.Message.ID]; ok || it.busy {
			continue
		}
		it.busy = true
		heads = append(heads, it)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, it := range heads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.deliver(it.Message.ID, it.Message, it.SessionID, it.At, it.Kind)
		}()
	}
	wg.Wait()
}

// classify maps a Check or Deliver error to a final state, or "" to hold the message.
func (s *Service) classify(err error, arrived, now time.Time) (api.ChannelState, string) {
	switch {
	case err == nil:
		return api.ChannelStateDelivered, ""
	case errors.Is(err, ErrBusy):
		return "", ""
	case errors.Is(err, ErrNoState):
		from := arrived
		if s.started.After(from) {
			from = s.started
		}
		if now.Sub(from) >= NoStateGrace {
			return api.ChannelStateRefused, "agent not running: " + err.Error()
		}
		return "", ""
	case errors.Is(err, ErrNotDelivered):
		return api.ChannelStateNotDelivered, err.Error()
	}
	return api.ChannelStateRefused, err.Error()
}

func (s *Service) deliver(id string, m api.ChannelMessage, sessionID string, arrived time.Time, kind string) {
	// release ends this attempt: it finishes the item with a final state, or frees it.
	release := func(st api.ChannelState, reason string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		it := s.findLocked(id)
		if it == nil {
			return
		}
		it.busy = false
		if st == "" {
			if it.Attempting {
				it.Attempting = false
				if err := s.commitLocked(); err != nil {
					s.cfg.Logf("channels: save: %v", err)
				}
			}
			s.broadcastLocked() // a wait that found it busy may take it now
			return
		}
		wrote := it.Attempting
		s.finishLocked(id, st, reason, s.cfg.Now())
		if err := s.commitLocked(); err != nil {
			// The failed save put the item back. A write that started is never made again: a
			// later round saves this outcome. An item never written is tried again.
			if wrote {
				s.settle[id] = outcome{st, reason}
			}
			s.cfg.Logf("channels: save: %v", err)
		}
		s.cfg.Logf("channels: message %s for %s: %s %s", id, m.To.Session, st, reason)
		s.Poke() // the next message for the session
	}
	if cur, ok := s.cfg.Local.Session(m.To.Session); !ok || cur != sessionID {
		release(api.ChannelStateRefused, fmt.Sprintf("session %s was replaced or renamed since the message arrived", m.To.Session))
		return
	}
	if err := s.cfg.Local.Check(sessionID); err != nil {
		release(s.classify(err, arrived, s.cfg.Now()))
		return
	}
	// The mark reaches the disk before the PTY write: a hab that stops between the write and
	// the finish never writes the message twice.
	s.mu.Lock()
	it := s.findLocked(id)
	if it == nil {
		s.mu.Unlock()
		return
	}
	it.Attempting = true
	if err := s.commitLocked(); err != nil {
		if it := s.findLocked(id); it != nil {
			it.busy = false
		}
		s.mu.Unlock()
		s.cfg.Logf("channels: save: %v", err)
		return
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	text := Envelope(m.ID, s.fromAddress(m), m.Text)
	switch kind {
	case kindSeed:
		text = SeedEnvelope(m.ID, m.From.Session, m.Text)
	case kindNote:
		text = NoteEnvelope(m.Text)
	}
	err := s.cfg.Local.Deliver(ctx, sessionID, text)
	release(s.classify(err, arrived, s.cfg.Now()))
}

// finishLocked removes an inbox item with its final state and sends the state to the sender.
// The caller saves.
func (s *Service) finishLocked(id string, st api.ChannelState, reason string, now time.Time) {
	i := slices.IndexFunc(s.st.Inbox, func(it *inItem) bool { return it.Message.ID == id })
	if i < 0 {
		return
	}
	m := s.st.Inbox[i].Message
	s.st.Inbox = slices.Delete(s.st.Inbox, i, i+1)
	if m.From.DeviceID == s.cfg.Self {
		s.setStateLocked(m.ID, s.cfg.Self, st, reason, now)
	} else {
		dev := m.From.DeviceID
		s.st.Outbox[dev] = append(s.st.Outbox[dev], outItem{
			Item: api.MailItem{Kind: api.MailKindStatus, Status: &api.ChannelStatus{ID: m.ID, State: st, Reason: reason, At: stamp(now)}},
			At:   now,
		})
		s.freshLocked(dev)
		s.Kick()
	}
}

// --- Wait ---

// Wait waits for a message to p.Session, or for p.Agent to reach a state in p.For (mesh.md
// 6.4). A message it returns counts as delivered once it is returned: a control connection
// cannot tell whether its answer reached the client.
func (s *Service) Wait(ctx context.Context, p api.ChannelWaitParams) (api.ChannelWaitResult, error) {
	res, written, err := s.WaitTake(ctx, p, DefaultWait)
	if written != nil {
		written(true)
	}
	return res, err
}

// WaitTake is Wait for a caller that can tell whether its answer was written: a message it
// returns is held until written is called, then finished delivered (true) or put back (false).
// written is nil when no message is returned. def is the wait when p names no timeout.
func (s *Service) WaitTake(ctx context.Context, p api.ChannelWaitParams, def time.Duration) (api.ChannelWaitResult, func(bool), error) {
	if p.Session == "" && p.Agent == "" {
		return api.ChannelWaitResult{}, nil, control.Errorf(api.ErrorKindInvalid, "channel.wait needs a session, an agent, or both")
	}
	var sessionID string
	if p.Session != "" {
		var ok bool
		if sessionID, ok = s.cfg.Local.Session(p.Session); !ok {
			return api.ChannelWaitResult{}, nil, control.Errorf(api.ErrorKindNotFound, "no session named %q on this hab", p.Session)
		}
	}
	wait := def
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
			if it := s.oldestLocked(p.Session); it != nil && !it.busy && !it.Attempting && it.SessionID == sessionID {
				// The mark reaches the disk before the answer leaves, as for a PTY write: a hab
				// that stops before the finish is saved never pastes the message after the wait
				// returned it.
				it.busy, it.Attempting = true, true
				m := it.Message
				if err := s.commitLocked(); err != nil {
					if it := s.findLocked(m.ID); it != nil {
						it.busy = false
					}
					s.mu.Unlock()
					return api.ChannelWaitResult{}, nil, control.Errorf(api.ErrorKindUnavailable, "the message was not saved: %v", err)
				}
				s.mu.Unlock()
				return api.ChannelWaitResult{Message: &api.ChannelInbound{ID: m.ID, From: s.fromAddress(m), Text: m.Text, ReplyTo: m.ReplyTo, At: m.At}},
					s.taken(m.ID), nil
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
				return api.ChannelWaitResult{Agent: &api.ChannelAgent{Session: p.Agent, State: st}}, nil, nil
			}
			if ok {
				agentCh = changed
			}
		}
		select {
		case <-ch:
		case <-agentCh:
		case <-t.C:
			return api.ChannelWaitResult{TimedOut: true}, nil, nil
		case <-ctx.Done():
			return api.ChannelWaitResult{TimedOut: true}, nil, nil
		}
	}
}

// oldestLocked returns the oldest inbox item for a session name.
func (s *Service) oldestLocked(session string) *inItem {
	for _, it := range s.st.Inbox {
		if it.Message.To.Session == session {
			return it
		}
	}
	return nil
}

// taken returns the written callback of a message a wait took.
func (s *Service) taken(id string) func(bool) {
	var once sync.Once
	return func(ok bool) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !ok {
				// Put back: the answer did not leave, so the mark goes. A failed save keeps the
				// mark, and the next delivery round tries the item again.
				if it := s.findLocked(id); it != nil {
					it.busy, it.Attempting = false, false
					if err := s.commitLocked(); err != nil {
						s.cfg.Logf("channels: save: %v", err)
					}
					s.broadcastLocked()
				}
				return
			}
			s.finishLocked(id, api.ChannelStateDelivered, "", s.cfg.Now())
			if err := s.commitLocked(); err != nil {
				// Returned to the inbox by the failed save; the answer was written, so a later
				// round saves it delivered and nothing writes it again.
				s.settle[id] = outcome{api.ChannelStateDelivered, ""}
				s.cfg.Logf("channels: save: %v", err)
			}
			s.Poke()
		})
	}
}

// --- Mail rounds ---

func (s *Service) mailLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Poll)
	defer t.Stop()
	for {
		s.expire()
		s.mailRound(s.ctx)
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
		up := s.up(l)
		s.mu.Lock()
		if up && !s.online[l.Label] {
			delete(s.fails, l.Label) // back online: try at once
		}
		s.online[l.Label] = up
		f := s.fails[l.Label]
		backoff := f != nil && now.Before(f.next) // read under the lock: a wake-up's exchange may fail now
		s.mu.Unlock()
		if !up || backoff {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.run(ctx, l)
		}()
	}
	wg.Wait()
}

// up reports whether l is a link this hab can hand mail over.
func (s *Service) up(l Link) bool {
	return l.Online && l.Caller != nil && l.DeviceID != "" && l.DeviceID != s.cfg.Self && slices.Contains(l.Caps, Capability)
}

// run makes one exchange over l. When one is in flight already, that one runs once more after
// it ends instead, so a link never has two exchanges, or two pulls, in flight.
func (s *Service) run(ctx context.Context, l Link) {
	s.mu.Lock()
	if s.running[l.Label] {
		s.again[l.Label] = true
		s.mu.Unlock()
		return
	}
	s.running[l.Label] = true
	s.mu.Unlock()
	for {
		ok := s.exchange(ctx, l)
		s.mu.Lock()
		again := s.again[l.Label] && ctx.Err() == nil
		delete(s.again, l.Label)
		if again && ok {
			s.mu.Unlock()
			continue
		}
		delete(s.running, l.Label)
		var wait time.Duration
		if again {
			// The exchange failed while a wake-up or a round asked for another: keep the
			// request, and make it once the link's backoff ends, not at the next round.
			if f := s.fails[l.Label]; f != nil {
				wait = f.next.Sub(s.cfg.Now())
			}
		}
		s.mu.Unlock()
		if again {
			s.retryAfter(l.Label, wait)
		}
		return
	}
}

// retryAfter asks for an exchange over the link labelled label once d has passed.
func (s *Service) retryAfter(label string, d time.Duration) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTimer(max(d, 0))
		defer t.Stop()
		select {
		case <-s.done:
		case <-t.C:
			s.Woken(label)
		}
	}()
}

// --- The mesh.changed wake-up (mesh.md 6.5) ---

// freshLocked notes that device's outbox gained an item; commitLocked wakes it once saved.
func (s *Service) freshLocked(device string) {
	if s.cfg.Wake != nil {
		s.fresh[device] = true
	}
}

// wakeFreshLocked runs after a save: each device whose outbox gained an item is told, unless
// it was told already and has not pulled since.
func (s *Service) wakeFreshLocked() {
	for dev := range s.fresh {
		if !s.woken[dev] {
			s.woken[dev] = true
			s.toWake[dev] = true
			signal(s.wake)
		}
	}
	clear(s.fresh)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// wakeLoop sends the wake-ups, off the lock. A device this hab holds an online link to gets
// none: the mail round pushes to it. A device with no link to this hab gets none either, and
// is free to be told again: it pulls when its link comes up.
func (s *Service) wakeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		s.mu.Lock()
		devs := make([]string, 0, len(s.toWake))
		for dev := range s.toWake {
			devs = append(devs, dev)
		}
		clear(s.toWake)
		s.mu.Unlock()
		pushed := map[string]bool{}
		if s.cfg.Peers != nil {
			for _, l := range s.cfg.Peers.Links() {
				s.mu.Lock()
				backoff := s.fails[l.Label] != nil
				s.mu.Unlock()
				if s.up(l) && !backoff { // a link in backoff pushes nothing now: wake instead
					pushed[l.DeviceID] = true
				}
			}
		}
		for _, dev := range devs {
			if pushed[dev] || !s.cfg.Wake(dev) {
				s.mu.Lock()
				delete(s.woken, dev)
				s.mu.Unlock()
			}
		}
	}
}

// Woken takes a mesh.changed from the peer at the link labelled label: mail waits there, so
// this hab pulls it now, past any backoff, rather than at its next mail round.
func (s *Service) Woken(label string) {
	s.mu.Lock()
	s.wanted[label] = true
	s.mu.Unlock()
	signal(s.pulls)
}

// pullLoop runs an exchange over each link whose peer said mail waits.
func (s *Service) pullLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.pulls:
		}
		s.mu.Lock()
		wanted := maps.Clone(s.wanted)
		clear(s.wanted)
		s.mu.Unlock()
		if s.cfg.Peers == nil {
			continue
		}
		for _, l := range s.cfg.Peers.Links() {
			if wanted[l.Label] && s.up(l) {
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.run(s.ctx, l)
				}()
			}
		}
	}
}

// exchange pushes the link's outbox, then pulls until nothing comes. It reports whether it
// ran to the end.
func (s *Service) exchange(ctx context.Context, l Link) bool {
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
			return false
		}
		s.mu.Lock()
		removed := s.applyResultsLocked(l.DeviceID, res.Results, s.cfg.Now())
		if removed > 0 {
			err = s.commitLocked()
		}
		s.mu.Unlock()
		if err == nil && removed == 0 {
			err = errors.New("no result matched an item sent")
		}
		if err != nil {
			s.failed(l, "push to", err)
			return false
		}
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
			return false
		}
		s.mu.Lock()
		delete(s.acks, l.Label)
		s.mu.Unlock()
		if len(res.Items) == 0 {
			break
		}
		results, err := s.receive(l.DeviceID, res.Items)
		if err != nil {
			s.failed(l, "pull from", err)
			return false
		}
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
	return true
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

// expire drops outbox items, records and ids older than Keep, and finishes inbox items older
// than Keep as expired. A dropped message's record turns expired.
func (s *Service) expire() {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for dev, q := range s.st.Outbox {
		keep := q[:0]
		for _, it := range q {
			if now.Sub(it.At) < Keep {
				keep = append(keep, it)
				continue
			}
			if it.Item.Kind == api.MailKindMessage && it.Item.Message != nil {
				s.setStateLocked(it.Item.Message.ID, dev, api.ChannelStateExpired, "not handed over within 7 days", now)
			}
		}
		if len(keep) != len(q) {
			changed = true
			if len(keep) == 0 {
				delete(s.st.Outbox, dev)
			} else {
				s.st.Outbox[dev] = keep
			}
		}
	}
	for _, it := range slices.Clone(s.st.Inbox) {
		if _, settling := s.settle[it.Message.ID]; !settling && !it.busy && now.Sub(it.At) >= Keep {
			s.finishLocked(it.Message.ID, api.ChannelStateExpired, "not delivered within 7 days", now)
			changed = true
		}
	}
	for id, r := range s.st.Records {
		if now.Sub(r.Sent) >= Keep && !slices.ContainsFunc(s.st.Outbox[r.ToDevice], func(it outItem) bool { return itemID(it.Item) == id }) {
			delete(s.st.Records, id)
			changed = true
		}
	}
	for id, v := range s.st.Seen {
		if now.Sub(v.At) >= Keep {
			delete(s.st.Seen, id)
			changed = true
		}
	}
	if changed {
		if err := s.commitLocked(); err != nil {
			s.cfg.Logf("channels: save: %v", err)
		}
	}
}
