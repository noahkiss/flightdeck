package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Presence windows (app.md 8.3, 18.4; landscape.md 4.1).
const (
	// FocusHold: a session some client reported in focus this recently gets no banner.
	FocusHold = 60 * time.Second
	// ActiveWindow: a device whose client reported itself in the foreground this recently gets
	// no banner; the app shows its own in-app notice from the live event.
	ActiveWindow = 180 * time.Second
)

// workingEvery throttles Live Activity updates while the agent keeps working (app.md 8.2).
const workingEvery = 30 * time.Second

// activityLinger is how long a finished Live Activity stays on screen (app.md 9.1).
const activityLinger = 10 * time.Minute

// sendTimeout bounds one APNs request.
const sendTimeout = 15 * time.Second

// alertKeep is how long APNs stores an alert for a device that is offline. A permission banner
// is kept until its request expires instead.
const alertKeep = time.Hour

// Sender sends one message; *Client is one.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// NotifierConfig is what NewNotifier needs. Sender and Store are required.
type NotifierConfig struct {
	Sender Sender
	Store  *Store
	Hab    string // the hab's label, for subtitles
	// Where looks up the session and project of a pane, or of a session when pane is empty.
	// Unfiled panes return a Where with no session; they still push.
	Where func(pane, session string) Where
	// Connected reports whether a client has an open control connection.
	Connected func(client string) bool
	// DefaultEnv is the environment of a Live Activity token whose device registered no device
	// token. Default production.
	DefaultEnv string
	Now        func() time.Time
	// Sleep is the rate-limit wait; it returns early when ctx ends. Default: a timer.
	Sleep func(ctx context.Context, d time.Duration)
	Logf  func(string, ...any)
	// Capcom returns the device id of the host that runs Capcom, or "" when none does. With
	// one, each banner carries the Ask action and its context (capcom.md 8.5). Nil: no Ask.
	Capcom func() string
	// Self is this hab's device id: the context's host when Capcom runs elsewhere.
	Self string
	// Keep records an agent-state banner as a notice and returns its id, so its Ask action
	// names a notice the Capcom home can read; "" when it could not. full holds the request's
	// command, hidden only what a banner with commands hidden shows; the keeper picks hidden
	// when the session is outside the read scope. Nil: agent-state banners carry no Ask.
	Keep func(full, hidden Notice) string
}

// Notice is a notification raised through notice.raise or by the hab itself.
type Notice struct {
	ID      string
	Title   string
	Body    string
	Level   Level
	Session string // optional: the session it is about, for its subtitle and the focus hold
	Folder  string // a kept permission banner's working folder, for the read-scope check
	// Kind is the push kind: empty or KindNotice goes to every device, any other kind only to
	// the devices that registered it. Collapse replaces "n-<id>" as the collapse id, and
	// Category names the banner's actions (empty: none). Round and Run name a round
	// notification's round and run in the push's metadata.
	Kind     string
	Collapse string
	Category string
	Round    string
	Run      string
}

// Notifier turns agent state changes into pushes. State is safe to call under any lock: it
// never blocks. Pushes go out from one goroutine, in order.
type Notifier struct {
	cfg NotifierConfig

	mu      sync.Mutex
	pending map[string]api.AgentState // latest unprocessed state per pane
	order   []string                  // panes in pending, in arrival order
	jobs    []func()
	wake    chan struct{}
	closed  bool
	done    chan struct{}
	ctx     context.Context // ends at Close: sends and waits stop, unrun work is dropped
	cancel  context.CancelFunc

	// Presence, under mu.
	focus  map[string]map[string]time.Time // session -> client -> last in-focus report
	active map[string]time.Time            // client -> last foreground report
	devOf  map[string]string               // client -> device id
	// answers records an approval.answer on its way, so the withdrawal says where it came from.
	answers map[string]answer // pane -> answer

	// Owned by the worker goroutine.
	last    map[string]api.AgentState
	open    map[string]*sent     // pane -> the approval banner to withdraw
	laLast  map[string]time.Time // device -> last Live Activity update sent
	laKey   map[string]string    // device -> the state key of that update
	settled map[string]answer    // pane -> the request an answer resolved, seen as resolved now
	backoff time.Duration
	// backoffUntil: after a rate limit, non-urgent sends are skipped until then.
	backoffUntil time.Time
	brokenMu     sync.Mutex
	broken       string
}

type answer struct {
	request string
	by      string
	allow   bool
}

// sent is an approval banner that went out, kept until its request resolves.
type sent struct {
	kind     string
	request  string
	collapse string
	req      *api.PermissionRequest
	expires  time.Time // the banner's apns-expiration; the withdrawal uses the same
	to       []target
}

type target struct {
	device, token, env string
	hide               bool
	ask                bool // the device's app registered the Ask categories
}

// NewNotifier starts the notifier's goroutine.
func NewNotifier(cfg NotifierConfig) *Notifier {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Where == nil {
		cfg.Where = func(string, string) Where { return Where{} }
	}
	if cfg.Connected == nil {
		cfg.Connected = func(string) bool { return false }
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
		}
	}
	if cfg.DefaultEnv == "" {
		cfg.DefaultEnv = Production
	}
	n := &Notifier{
		cfg:     cfg,
		pending: map[string]api.AgentState{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		focus:   map[string]map[string]time.Time{},
		active:  map[string]time.Time{},
		devOf:   map[string]string{},
		answers: map[string]answer{},
		last:    map[string]api.AgentState{},
		open:    map[string]*sent{},
		laLast:  map[string]time.Time{},
		laKey:   map[string]string{},
		settled: map[string]answer{},
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	go n.run()
	return n
}

// Close stops the goroutine. A send in flight or a rate-limit wait ends at once, and queued
// work that has not run is dropped: shutdown must not wait on an unreachable APNs.
func (n *Notifier) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		<-n.done
		return
	}
	n.closed = true
	n.cancel()
	n.mu.Unlock()
	n.signal()
	<-n.done
}

func (n *Notifier) signal() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// State queues an agent state change. Only the latest state of a pane waits; one that is
// replaced before it is processed raises nothing.
func (n *Notifier) State(st api.AgentState) {
	n.mu.Lock()
	if _, ok := n.pending[st.Pane]; !ok {
		n.order = append(n.order, st.Pane)
	}
	n.pending[st.Pane] = st
	n.mu.Unlock()
	n.signal()
}

// Sync waits until everything queued before it was processed. For tests.
func (n *Notifier) Sync() {
	ch := make(chan struct{})
	n.mu.Lock()
	n.jobs = append(n.jobs, func() { close(ch) })
	n.mu.Unlock()
	n.signal()
	select {
	case <-ch:
	case <-n.done:
	}
}

// Bind records that a client speaks for a device (push.register and push.register_activity
// name it), so the client's presence applies to that device.
func (n *Notifier) Bind(client, device string) {
	n.mu.Lock()
	n.devOf[client] = device
	n.mu.Unlock()
}

// Focus records a session.focus report. active true means the session is on screen in an
// active window: the app is in the foreground. active false means it left the screen.
func (n *Notifier) Focus(client, session string, active bool) {
	now := n.cfg.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if !active {
		delete(n.focus[session], client)
		if len(n.focus[session]) == 0 {
			delete(n.focus, session)
		}
		// The client still shows another session (it switched from this one): it stays in
		// the foreground. Otherwise the app left the foreground or closed the session.
		for _, cs := range n.focus {
			if at, ok := cs[client]; ok && now.Sub(at) < FocusHold {
				return
			}
		}
		delete(n.active, client)
		return
	}
	if n.focus[session] == nil {
		n.focus[session] = map[string]time.Time{}
	}
	n.focus[session][client] = now
	n.active[client] = now
}

// Answering records an approval.answer about to be sent, so the banner it withdraws names the
// client. Unanswering drops it when the answer did not go out.
func (n *Notifier) Answering(pane, request, by string, allow bool) {
	n.mu.Lock()
	n.answers[pane] = answer{request: request, by: by, allow: allow}
	n.mu.Unlock()
}

// Answered reports that an answer to request reached the harness. The banner is withdrawn and
// the Live Activity loses its buttons now, without waiting for the harness's next hook: Claude
// Code keeps the request open until the tool has run.
func (n *Notifier) Answered(pane, request string, allow bool) {
	n.mu.Lock()
	n.jobs = append(n.jobs, func() {
		n.settled[pane] = answer{request: request, allow: allow}
		if st, ok := n.last[pane]; ok {
			n.process(st)
		}
	})
	n.mu.Unlock()
	n.signal()
}

// Forget drops a pane that is gone. An open approval banner is replaced with "Pane closed",
// same collapse id, and the pane's records go.
func (n *Notifier) Forget(pane string) {
	n.mu.Lock()
	delete(n.answers, pane)
	n.jobs = append(n.jobs, func() {
		if ob := n.open[pane]; ob != nil {
			w := n.cfg.Where(pane, "")
			w.Hab = n.cfg.Hab
			for _, t := range ob.to {
				b := ClosedBanner(w, ob.req, t.hide)
				payload := AlertPayload(b, Meta{Hab: n.cfg.Hab, Session: w.Session, Pane: pane, State: string(api.AgentStatusExited), Request: ob.request}, w.Session)
				n.send(Message{Token: t.token, Environment: t.env, Type: TypeAlert, Priority: 5, CollapseID: ob.collapse, Expiration: ob.expires, Payload: payload}, true)
			}
		}
		delete(n.open, pane)
		delete(n.last, pane)
		delete(n.settled, pane)
	})
	n.mu.Unlock()
	n.signal()
}

// effective reads a state whose permission request an answer already resolved as the state the
// answer leads to: working after an allow, idle after a deny.
func (n *Notifier) effective(st api.AgentState) api.AgentState {
	a, ok := n.settled[st.Pane]
	if !ok || st.State != api.AgentStatusBlocked || requestID(st) == "" || requestID(st) != a.request {
		return st
	}
	st.State, st.Kind, st.Request = api.AgentStatusWorking, "", nil
	if !a.allow {
		st.State = api.AgentStatusIdle
	}
	return st
}

// Unanswering drops the record Answering made for request, if it is still there.
func (n *Notifier) Unanswering(pane, request string) {
	n.mu.Lock()
	if a, ok := n.answers[pane]; ok && a.request == request {
		delete(n.answers, pane)
	}
	n.mu.Unlock()
}

// Broken returns why APNs refuses the key, or "" while push works.
func (n *Notifier) Broken() string {
	n.brokenMu.Lock()
	defer n.brokenMu.Unlock()
	return n.broken
}

// watchedLocked reports whether a connected client has the session in focus (FocusHold).
func (n *Notifier) watchedLocked(session string, now time.Time) bool {
	if session == "" {
		return false
	}
	for c, at := range n.focus[session] {
		if now.Sub(at) < FocusHold && n.cfg.Connected(c) {
			return true
		}
	}
	return false
}

// activeLocked reports whether a connected client of the device was in the foreground within
// ActiveWindow.
func (n *Notifier) activeLocked(device string, now time.Time) bool {
	for c, d := range n.devOf {
		if d != device {
			continue
		}
		if at, ok := n.active[c]; ok && now.Sub(at) < ActiveWindow && n.cfg.Connected(c) {
			return true
		}
	}
	return false
}

// Tier says how a device hears about an event.
type Tier int

const (
	TierPush    Tier = iota // send the banner
	TierInApp               // the device's app is in the foreground: it shows its own notice
	TierWatched             // a client watches the session: nothing
)

// tier picks the presence tier for one device and session.
func (n *Notifier) tier(device, session string) Tier {
	now := n.cfg.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.watchedLocked(session, now):
		return TierWatched
	case n.activeLocked(device, now):
		return TierInApp
	}
	return TierPush
}

// Raise queues a notice for every registered device that wants its kind, under the presence
// tiers. It returns the number of devices it goes to.
func (n *Notifier) Raise(no Notice) int {
	devs := n.cfg.Store.Devices()
	var to []target
	kind := no.Kind
	if kind == "" {
		kind = KindNotice
	}
	for _, d := range devs {
		if d.Token == "" || kind != KindNotice && !d.Wants(kind) || n.tier(d.ID, no.Session) != TierPush {
			continue
		}
		to = append(to, target{device: d.ID, token: d.Token, env: d.Environment, ask: d.Asks()})
	}
	if len(to) == 0 {
		return 0
	}
	n.mu.Lock()
	n.jobs = append(n.jobs, func() {
		w := Where{Hab: n.cfg.Hab}
		if no.Session != "" {
			w = n.cfg.Where("", no.Session)
			w.Hab, w.Session = n.cfg.Hab, no.Session
		}
		b := NoticeBanner(w, no.Title, no.Body, no.Level)
		b.Kind, b.Category = kind, no.Category
		meta := Meta{Hab: n.cfg.Hab, Session: no.Session, Notice: no.ID, Round: no.Round, Run: no.Run}
		collapse := no.Collapse
		if collapse == "" {
			collapse = "n-" + no.ID
		}
		meta.Context, meta.CapcomHome = n.ask(no.ID)
		for _, t := range to {
			// Only an app that registered the Ask categories gets one; any other keeps the
			// categories it knows. The context rides along either way.
			tb := b
			if meta.Context != nil && t.ask {
				tb.Category = AskCategory(b.Category)
			}
			payload := AlertPayload(tb, meta, no.Session)
			n.send(Message{Token: t.token, Environment: t.env, Type: TypeAlert, Priority: priority(b.Level), CollapseID: collapse, Expiration: n.cfg.Now().Add(alertKeep), Payload: payload}, true)
		}
	})
	n.mu.Unlock()
	n.signal()
	return len(to)
}

// ask returns the Ask action's context for notice id, and the Capcom home's device id; nil
// when no host runs Capcom.
func (n *Notifier) ask(id string) (*Ask, string) {
	if n.cfg.Capcom == nil || id == "" {
		return nil, ""
	}
	home := n.cfg.Capcom()
	if home == "" {
		return nil, ""
	}
	a := &Ask{Kind: "notification", Ref: id}
	if home != n.cfg.Self {
		a.Host = n.cfg.Self
	}
	return a, home
}

// keptNotice is the notice an agent-state banner is recorded as. With hide, only what a
// banner with commands hidden shows; otherwise the banner's words with the commands shown, and
// a permission request's whole command or file list as the body.
func keptNotice(kind string, st api.AgentState, w Where, hide bool) Notice {
	var b Banner
	switch kind {
	case KindPermission:
		b = PermissionBanner(w, st.Request, hide)
		switch r := st.Request; {
		case hide || r == nil:
		case r.Command != "":
			b.Body = r.Command
		case len(r.Files) > 0:
			b.Body = strings.Join(r.Files, "\n")
		}
	case KindQuestion:
		b = QuestionBanner(w)
	case KindAuth:
		b = AuthBanner(w)
	case KindTrust:
		b = TrustBanner(w, st.Trust)
	case KindError:
		b = ErrorBanner(w)
	case KindDone:
		b = DoneBanner(w)
	}
	no := Notice{Title: b.Title, Body: b.Body, Level: b.Level, Session: w.Session}
	if st.Request != nil {
		no.Folder = st.Request.Cwd
	}
	if st.Trust != nil {
		no.Folder = st.Trust.Folder
	}
	return no
}

func (n *Notifier) run() {
	defer close(n.done)
	for {
		n.mu.Lock()
		order, pending, jobs, closed := n.order, n.pending, n.jobs, n.closed
		n.order, n.pending, n.jobs = nil, map[string]api.AgentState{}, nil
		n.mu.Unlock()
		if closed {
			return
		}
		for _, p := range order {
			if n.ctx.Err() != nil {
				return
			}
			n.process(pending[p])
		}
		for _, j := range jobs {
			if n.ctx.Err() != nil {
				return
			}
			j()
		}
		if len(order) > 0 || len(jobs) > 0 {
			continue
		}
		<-n.wake
	}
}

// key names an agent state for change detection: a new key may raise a banner.
func key(st api.AgentState) string {
	switch st.State {
	case api.AgentStatusBlocked:
		k := "blocked/" + string(st.Kind)
		if st.Request != nil {
			k += "/" + st.Request.ID
		}
		return k
	case api.AgentStatusError, api.AgentStatusDone, api.AgentStatusWorking, api.AgentStatusIdle, api.AgentStatusExited:
		return string(st.State)
	}
	return ""
}

func requestID(st api.AgentState) string {
	if st.Request != nil {
		return st.Request.ID
	}
	return ""
}

// collapseID keeps an id within APNs's 64 bytes.
func collapseID(prefix, id string) string {
	s := prefix + id
	if len(s) <= 64 {
		return s
	}
	sum := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(sum[:16])
}

func priority(l Level) int {
	if l == LevelPassive {
		return 5
	}
	return 10
}

func (n *Notifier) process(st api.AgentState) {
	st = n.effective(st)
	prev, seen := n.last[st.Pane]
	n.last[st.Pane] = st
	k := key(st)
	changed := !seen || key(prev) != k
	ob := n.open[st.Pane]
	resolved := ob != nil && !(st.State == api.AgentStatusBlocked && string(st.Kind) == ob.kind && requestID(st) == ob.request)
	// Most changes are a pane that keeps working: skip the lookup when nothing can go out.
	if !changed && !resolved && !n.anyActivity() {
		return
	}
	w := n.cfg.Where(st.Pane, "")
	w.Hab = n.cfg.Hab
	if w.Harness == "" {
		w.Harness = st.Harness
	}
	// The open approval resolved: replace its banner, same collapse id (app.md 7.5).
	if resolved {
		delete(n.open, st.Pane)
		n.mu.Lock()
		a, ok := n.answers[st.Pane]
		delete(n.answers, st.Pane)
		n.mu.Unlock()
		var by string
		var allow *bool
		if ok && a.request == ob.request {
			by, allow = a.by, &a.allow
		}
		for _, t := range ob.to {
			b := AnsweredBanner(w, by, allow, ob.req, t.hide)
			if st.State == api.AgentStatusExited && by == "" {
				b = EndedBanner(w, ob.req, t.hide)
			}
			payload := AlertPayload(b, Meta{Hab: n.cfg.Hab, Session: w.Session, Pane: st.Pane, State: string(st.State), Request: ob.request}, w.Session)
			n.send(Message{Token: t.token, Environment: t.env, Type: TypeAlert, Priority: 5, CollapseID: ob.collapse, Expiration: ob.expires, Payload: payload}, true)
		}
	}
	// A pane first seen through a replayed screen report is old state: after a gff-hab restart
	// the link replays each pane's last OSC 7501 report. Record it and update the Live
	// Activity, but raise no banner.
	// A trust prompt is read from the screen alone (app.md 7.10); the link replays none.
	if changed && !(!seen && st.Source == api.StateSourceScreen && st.Kind != api.BlockedKindTrust) {
		n.banner(st, w)
	}
	n.activity(st, w, changed)
}

// anyActivity reports whether some device has a Live Activity token.
func (n *Notifier) anyActivity() bool {
	for _, d := range n.cfg.Store.Devices() {
		if d.Activity != nil {
			return true
		}
	}
	return false
}

// banner sends the banner a new state raises, to each device that wants its kind and is not
// held by presence.
func (n *Notifier) banner(st api.AgentState, w Where) {
	var kind, collapse string
	switch {
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindPermission:
		kind, collapse = KindPermission, collapseID("r-", requestID(st))
		if requestID(st) == "" {
			collapse = collapseID("r-", st.Pane)
		}
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindQuestion:
		kind, collapse = KindQuestion, collapseID("q-", st.Pane)
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindAuth:
		kind, collapse = KindAuth, collapseID("a-", st.Pane)
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindTrust:
		kind, collapse = KindTrust, collapseID("t-", st.Pane)
	case st.State == api.AgentStatusError:
		kind, collapse = KindError, collapseID("e-", st.Pane)
	case st.State == api.AgentStatusDone:
		kind, collapse = KindDone, collapseID("d-", st.Pane)
	default:
		return
	}
	meta := Meta{Hab: n.cfg.Hab, Session: w.Session, Pane: st.Pane, State: string(st.State), Request: requestID(st)}
	// APNs keeps an alert for an offline device until it expires: a permission request until
	// its own expiry, the rest for alertKeep.
	expires := n.cfg.Now().Add(alertKeep)
	if st.Request != nil {
		meta.ExpiresAt = st.Request.ExpiresAt
		if t, err := time.Parse(time.RFC3339, st.Request.ExpiresAt); err == nil && kind == KindPermission {
			expires = t
		}
	}
	// The banner is kept as a notice, once, when it first goes to a device: its Ask action
	// names that notice.
	kept := n.cfg.Keep == nil || n.cfg.Capcom == nil
	// A device that hides commands keeps them out of the kept notice too: Ask must not show
	// in Capcom what that banner left out.
	hide := false
	for _, d := range n.cfg.Store.Devices() {
		hide = hide || (d.Token != "" && d.Wants(kind) && d.HideCommands)
	}
	var out *sent
	if kind == KindPermission || kind == KindQuestion {
		out = &sent{kind: string(st.Kind), request: requestID(st), collapse: collapse, req: st.Request, expires: expires}
	}
	for _, d := range n.cfg.Store.Devices() {
		if d.Token == "" || !d.Wants(kind) {
			continue
		}
		switch n.tier(d.ID, w.Session) {
		case TierWatched:
			n.cfg.Logf("push: %s for pane %s held: a client watches session %s", kind, st.Pane, w.Session)
			continue
		case TierInApp:
			continue
		}
		var b Banner
		switch kind {
		case KindPermission:
			b = PermissionBanner(w, st.Request, d.HideCommands)
		case KindQuestion:
			b = QuestionBanner(w)
		case KindAuth:
			b = AuthBanner(w)
		case KindTrust:
			b = TrustBanner(w, st.Trust)
		case KindError:
			b = ErrorBanner(w)
		case KindDone:
			b = DoneBanner(w)
		}
		if !kept {
			kept = true
			if n.cfg.Capcom() != "" {
				hidden := keptNotice(kind, st, w, true)
				full := hidden
				if !hide {
					full = keptNotice(kind, st, w, false)
				}
				meta.Context, meta.CapcomHome = n.ask(n.cfg.Keep(full, hidden))
			}
		}
		if meta.Context != nil && d.Asks() {
			b.Category = AskCategory(b.Category)
		}
		payload := AlertPayload(b, meta, w.Session)
		if n.send(Message{Token: d.Token, Environment: d.Environment, Type: TypeAlert, Priority: priority(b.Level), CollapseID: collapse, Expiration: expires, Payload: payload}, true) && out != nil {
			out.to = append(out.to, target{device: d.ID, token: d.Token, env: d.Environment, hide: d.HideCommands})
		}
	}
	if out != nil && len(out.to) > 0 {
		n.open[st.Pane] = out
	}
}

// activity updates the Live Activity of each device that tracks the pane's session (app.md
// 9.1). A change of state goes out at once; a repeated working state at most every
// workingEvery; other repeats not at all. done and error end the activity, which lingers.
func (n *Notifier) activity(st api.AgentState, w Where, changed bool) {
	if w.Session == "" {
		return
	}
	now := n.cfg.Now()
	k := key(st)
	for _, d := range n.cfg.Store.Devices() {
		if d.Activity == nil || d.Activity.Session != w.Session {
			continue
		}
		dk := d.ID + "\x00" + d.Activity.Token
		if !changed && n.laKey[dk] == k {
			if st.State != api.AgentStatusWorking || now.Sub(n.laLast[dk]) < workingEvery {
				continue
			}
		}
		state := ActivityState{
			State: string(st.State), Kind: string(st.Kind), Title: n.activityTitle(st, w, d.HideCommands),
			Request: requestID(st), Pane: st.Pane, Since: st.At, Hab: n.cfg.Hab, Harness: st.Harness,
			Buttons: st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindPermission && st.Request != nil && st.Request.ID != "",
			Others:  n.othersBlocked(st.Pane),
		}
		end := st.State == api.AgentStatusDone || st.State == api.AgentStatusError || st.State == api.AgentStatusExited
		prio := 10
		if st.State == api.AgentStatusWorking || st.State == api.AgentStatusIdle {
			prio = 5
		}
		env := d.Environment
		if env == "" {
			env = n.cfg.DefaultEnv
		}
		payload := ActivityPayload(state, now.Unix(), end, now.Add(activityLinger).Unix())
		ok := n.send(Message{Token: d.Activity.Token, Environment: env, Type: TypeLiveActivity, Priority: prio, Payload: payload}, prio == 10)
		if ok {
			n.laKey[dk], n.laLast[dk] = k, now
			if end {
				n.cfg.Store.EndActivity(d.ID, d.Activity.Token)
				delete(n.laKey, dk)
				delete(n.laLast, dk)
			}
		}
	}
}

func (n *Notifier) activityTitle(st api.AgentState, w Where, hide bool) string {
	switch {
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindPermission:
		return PermissionBanner(w, st.Request, hide).Title
	case st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindQuestion:
		return "Question"
	case st.State == api.AgentStatusBlocked:
		return "Sign-in needed"
	case st.State == api.AgentStatusWorking:
		return "Working"
	case st.State == api.AgentStatusDone:
		return "Finished"
	case st.State == api.AgentStatusError:
		return "Stopped on an error"
	case st.State == api.AgentStatusExited:
		return "Exited"
	}
	return "Idle"
}

// othersBlocked counts the other panes on this hab that wait for the owner.
func (n *Notifier) othersBlocked(pane string) int {
	c := 0
	for p, st := range n.last {
		if p != pane && st.State == api.AgentStatusBlocked {
			c++
		}
	}
	return c
}

// send posts one message and handles APNs's answer: a bad token is dropped from the store, a
// refused key marks push broken, and a rate limit backs off; urgent messages wait out the
// back-off and try once more, others are dropped, as is every non-urgent message until the
// back-off ends. It reports whether APNs took the message.
func (n *Notifier) send(m Message, urgent bool) bool {
	for attempt := 0; attempt < 2; attempt++ {
		if n.ctx.Err() != nil {
			return false // closing: drop it
		}
		if !urgent && n.cfg.Now().Before(n.backoffUntil) {
			return false
		}
		ctx, cancel := context.WithTimeout(n.ctx, sendTimeout)
		err := n.cfg.Sender.Send(ctx, m)
		cancel()
		if err != nil && n.ctx.Err() != nil {
			return false
		}
		if err == nil {
			n.backoff, n.backoffUntil = 0, time.Time{}
			n.setBroken("")
			return true
		}
		if errors.Is(err, ErrNoKey) {
			return false // the keyring logs its own failures
		}
		var e *Error
		if !errors.As(err, &e) {
			n.cfg.Logf("push: %v", err)
			return false
		}
		switch {
		case e.BadToken():
			if dropped, _ := n.cfg.Store.Drop(m.Token); dropped {
				n.cfg.Logf("push: APNs refused a token (%s); dropped it", e.Reason)
			}
			return false
		case e.KeyRejected():
			n.setBroken(e.Reason)
			return false
		case e.RateLimited():
			n.backoff = min(max(2*n.backoff, time.Second), time.Minute)
			n.backoffUntil = n.cfg.Now().Add(n.backoff)
			if !urgent || attempt > 0 {
				return false
			}
			n.cfg.Logf("push: APNs rate limit; waiting %v", n.backoff)
			n.cfg.Sleep(n.ctx, n.backoff)
			continue
		}
		n.cfg.Logf("push: %v", err)
		return false
	}
	return false
}

func (n *Notifier) setBroken(reason string) {
	n.brokenMu.Lock()
	defer n.brokenMu.Unlock()
	if reason != "" && n.broken == "" {
		n.cfg.Logf("push broken: APNs refuses the key or topic (%s); check the push config", reason)
	}
	n.broken = reason
}
