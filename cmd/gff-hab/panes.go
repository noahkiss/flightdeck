package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/panetext"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Reading panes (hab-logic-api.md 3.11): pane.read returns a pane's screen and scrollback as
// plain text from the hab's own parser; pane.wait follows the pane's output until a line
// matches, the pane is quiet, its program exits, or the time ends. agent.read_panes decides
// what an agent may read through its pane route or a peer link.

// paneTextCap bounds the row text of one pane.read answer.
const paneTextCap = 1 << 20

// The waits of pane.wait: as channel.wait (mesh.md 6.5).
const (
	paneWaitDefault = 180 * time.Second
	paneWaitPane    = 110 * time.Second
	paneWaitMax     = 600 * time.Second
)

// The bounds on waits (hab-logic-api.md 3.11): concurrent waits per pane route token and per
// peer link, and on the hab; how far back from may replay for an agent's door; how often a
// pattern is tried on a line still open.
const (
	paneWaitsPerCaller = 16
	paneWaitsAll       = 64
	paneReplayMax      = 1 << 20
	paneOpenEvery      = 100 * time.Millisecond
)

// paneLease is the most a forwarded wait runs on the far hab before the near hab asks again
// (hab-logic-api.md 3.11): a near caller that goes away leaves a far wait for at most this long.
// A test build shortens it (panesseam_testseam.go).
var paneLease = 30 * time.Second

// The values of agent.read_panes.
const (
	readPanesOff     = "off"
	readPanesProject = "project"
	readPanesAll     = "all"
)

// readDoor says which door a pane.read or pane.wait came through.
type readDoor int

const (
	doorOwner readDoor = iota // a control client that is not a peer link: the owner
	doorRoute                 // the pane route: an agent in the token's pane
	doorPeer                  // a peer link: an agent on another hab
)

// paneCaller is who asks: the door; for the pane route the caller's pane id; for a peer link
// the connection, which the wait bound counts by. capcom marks a peer link's call as the Capcom
// service's forward: set only when the peer holds the Capcom mark in this hab's belief
// (hab-logic-api.md 3.11).
type paneCaller struct {
	door   readDoor
	pane   string
	conn   string
	capcom bool
}

// asCapcom is the forward mark of the Capcom service's calls to a peer (hab-logic-api.md 3.11).
const asCapcom = "capcom"

// key names the caller for the wait bound; "" for the owner, who counts only toward the hab's.
func (c paneCaller) key() string {
	switch c.door {
	case doorRoute:
		return "route:" + c.pane
	case doorPeer:
		return "peer:" + c.conn
	}
	return ""
}

// paneService serves pane.read and pane.wait.
type paneService struct {
	lk    *link.Link
	store *structure.Store
	// rule returns agent.read_panes now; drive returns agent.drive_panes (hab-logic-api.md 3.12).
	rule  func() string
	drive func() string
	// links lists the peer links this hab dialed; channelPeers over the peer manager.
	links func() []channels.Link
	// self and host are this hab's device id and host id: a label/name that names this hab
	// reads here.
	self, host string
	// capcomHome is the device id of the hab that holds the Capcom mark in this hab's belief,
	// "" for none: a peer's forward marked capcom counts only from it. Nil: none.
	capcomHome func() string
	logf       func(string, ...any)
	slots      waitSlots
	// The pane-drive part (panedrive.go). Nil fields answer unavailable.
	drv paneDrive
}

// paneTarget is a resolved local target: the pane, and its session when it is filed.
type paneTarget struct {
	pane    string
	session api.Session
	filed   bool
	agent   bool // an agent runs in the pane; set for agent.drive_panes only
}

// ruleValue reads agent.read_panes: a word the settings service checked, else all (its
// default).
func ruleValue(s *settings.Service) func() string {
	return wordSetting(s, settings.AgentReadPanes, readPanesAll)
}

// wordSetting reads a word setting: a word the settings service checked, else def.
func wordSetting(s *settings.Service, id, def string) func() string {
	return func() string {
		var w string
		if err := json.Unmarshal(s.Value(id), &w); err != nil || w == "" {
			return def
		}
		return w
	}
}

// isCapcom says whether the caller is the Capcom service: its route on this hab, or its forward
// from the peer that holds the Capcom mark (hab-logic-api.md 3.11, 3.12). Capcom has no pane and
// no project: every project counts as its own.
func (c paneCaller) isCapcom() bool {
	return (c.door == doorRoute && c.pane == capcomRoute) || (c.door == doorPeer && c.capcom)
}

// forwardAs is the mark a forward to a peer carries: capcom for the Capcom service's call, else
// none. No other caller's params can set it.
func (c paneCaller) forwardAs() string {
	if c.isCapcom() {
		return asCapcom
	}
	return ""
}

// markCapcom takes a peer link's capcom mark only from the peer that holds the Capcom mark in
// this hab's belief; from any other peer, or any other door, the mark is ignored and the door's
// own rule applies (hab-logic-api.md 3.11).
func (s *paneService) markCapcom(c paneCaller, device, as string) paneCaller {
	if c.door != doorPeer || as != asCapcom || device == "" || s.capcomHome == nil {
		return c
	}
	if home := s.capcomHome(); home != "" && home == device && device != s.self {
		c.capcom = true
	}
	return c
}

// limited says whether the rule limits this caller: an agent's door under a value other than
// all. The Capcom service is limited only under off, which shuts it out of this hab: at every
// other value it reaches every pane, so unknown targets keep their own kinds.
func limited(rule string, c paneCaller) bool {
	if c.isCapcom() {
		return rule == readPanesOff
	}
	return c.door != doorOwner && rule != readPanesAll
}

// mayRead is the rule (hab-logic-api.md 3.11). The owner reads every pane. A peer link reads
// every pane under all, else none. The Capcom service reads every pane at every value but off.
// The pane route reads its own pane always, the panes of its own session's project under
// project, and every pane under all.
func mayRead(rule string, c paneCaller, callerProject string, t paneTarget) bool {
	switch {
	case c.door == doorOwner:
		return true
	case c.isCapcom():
		return rule != readPanesOff
	case c.door == doorPeer:
		return rule == readPanesAll
	}
	if samePane(c.pane, t.pane) || rule == readPanesAll {
		return true
	}
	return rule == readPanesProject && callerProject != "" && t.filed && t.session.Project == callerProject
}

// samePane compares two pane ids in any spelling parseUUID takes.
func samePane(a, b string) bool {
	ua, ok1 := parseUUID(a)
	ub, ok2 := parseUUID(b)
	return ok1 && ok2 && ua == ub
}

// deny logs a refusal of a read with ids only (diagnostics.md 10.1) and answers denied with a
// hint that names the setting, never the target. target is "" when the call names no pane here.
func (s *paneService) deny(method string, c paneCaller, rule, target string) error {
	return s.denyBy(gateRead, method, c, rule, target)
}

// denyBy is deny for either setting (hab-logic-api.md 3.11, 3.12).
func (s *paneService) denyBy(g paneGate, method string, c paneCaller, rule, target string) error {
	if target == "" {
		target = "none"
	}
	s.logf("panes: %s denied: %s %s, caller %s, target pane %s", method, g.setting(), rule, c.name(), target)
	who := "an agent"
	if c.isCapcom() {
		who = "Capcom"
	}
	return control.Errorf(api.ErrorKindDenied, "%s is %s on this hab: %s may %s. The owner changes it in Flightdeck's settings", g.setting(), rule, who, g.allowed(rule, c))
}

// prepare resolves the target of a call and applies the rule. A limited caller learns nothing
// about a target it may not read: an unknown or exited target answers denied too, and so does
// a label/name on a peer, since only all lets an agent read past its own hab.
func (s *paneService) prepare(method string, c paneCaller, pane, session string) (paneTarget, *channels.Link, string, error) {
	return s.prepareBy(gateRead, method, c, pane, session)
}

// prepareBy is prepare under either setting: agent.read_panes for reads and lists,
// agent.drive_panes for typing (hab-logic-api.md 3.12).
func (s *paneService) prepareBy(g paneGate, method string, c paneCaller, pane, session string) (paneTarget, *channels.Link, string, error) {
	rule := s.ruleOf(g)
	lim := limited(rule, c)
	t, peer, bare, err := s.resolve(c, pane, session, g.capability())
	if err != nil {
		var ce *control.Error
		if lim && errors.As(err, &ce) && ce.Data.Kind != api.ErrorKindInvalid {
			return paneTarget{}, nil, "", s.denyBy(g, method, c, rule, "")
		}
		return paneTarget{}, nil, "", err
	}
	if peer != nil {
		if lim {
			return paneTarget{}, nil, "", s.denyBy(g, method, c, rule, "")
		}
		return t, peer, bare, nil
	}
	if g == gateDrive && s.drv.agent != nil {
		if a, ok := s.drv.agent(t.pane); ok && a.Revision > 0 && a.State != api.AgentStatusExited && a.State != api.AgentStatusNone {
			t.agent = true
		}
	}
	if !g.may(rule, c, s.callerProject(c), t) {
		return paneTarget{}, nil, "", s.denyBy(g, method, c, rule, t.pane)
	}
	return t, nil, "", nil
}

// callerProject is the project of the route caller's own session; "" for none, and for any
// other door.
func (s *paneService) callerProject(c paneCaller) string {
	if c.door != doorRoute {
		return ""
	}
	if ss, ok := s.store.SessionByPane(c.pane); ok {
		return ss.Project
	}
	return ""
}

// resolve finds the target of a call. A label/name for a peer returns the link to forward on,
// with the bare name.
func (s *paneService) resolve(c paneCaller, pane, session, capability string) (paneTarget, *channels.Link, string, error) {
	if (pane == "") == (session == "") {
		return paneTarget{}, nil, "", control.Errorf(api.ErrorKindInvalid, "give pane or session, not both")
	}
	if pane != "" {
		u, ok := parseUUID(pane)
		if !ok {
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane)
		}
		if _, ok := s.lk.Pane(u); !ok {
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindNotFound, "no pane %s on this hab", pane)
		}
		t := paneTarget{pane: u.String()}
		t.session, t.filed = s.store.SessionByPane(t.pane)
		return t, nil, "", nil
	}
	name := session
	if label, bare, ok := strings.Cut(session, "/"); ok {
		if label == "" || bare == "" {
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindInvalid, "session %q: want a name, an id or label/name", session)
		}
		here := label == s.host
		var peer *channels.Link
		for _, l := range s.links() {
			if l.Label != label {
				continue
			}
			if l.DeviceID != "" && l.DeviceID == s.self {
				here = true
				break
			}
			peer = &l
			break
		}
		switch {
		case here:
			name = bare
		case c.door == doorPeer:
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindInvalid, "a call over a peer link reaches only this hab's own panes: a forward never forwards")
		case peer == nil:
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindNotFound, "no peer labelled %q in the peer list", label)
		case !peer.Online || peer.Caller == nil:
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindUnavailable, "peer %s is not online from this hab, so its panes cannot be reached; try again once it is online", label)
		case !slices.Contains(peer.Caps, capability):
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindUnavailable, "peer %s runs without %s, so this call cannot reach its panes; update its gff-hab", label, capability)
		default:
			return paneTarget{}, peer, bare, nil
		}
	}
	ss, ok := s.store.Session(name)
	if !ok {
		ss, ok = s.store.SessionByName(name)
	}
	if !ok {
		return paneTarget{}, nil, "", control.Errorf(api.ErrorKindNotFound, "no session %q on this hab", name)
	}
	if ss.Pane == "" || ss.State == api.SessionStateExited {
		return paneTarget{}, nil, "", control.Errorf(api.ErrorKindConflict, "session %s has no live pane", name)
	}
	return paneTarget{pane: ss.Pane, session: ss, filed: true}, nil, "", nil
}

// paneReadCapability is the capability of pane.read and pane.wait.
const paneReadCapability = "pane-read"

// Read serves pane.read.
func (s *paneService) Read(ctx context.Context, c paneCaller, p api.PaneReadParams) (api.PaneText, error) {
	if p.Scrollback < 0 || p.Scrollback > link.MaxScrollbackPage {
		return api.PaneText{}, control.Errorf(api.ErrorKindInvalid, "scrollback %d: want 0 to %d rows", p.Scrollback, link.MaxScrollbackPage)
	}
	t, peer, bare, err := s.prepare("pane.read", c, p.Pane, p.Session)
	if err != nil {
		return api.PaneText{}, err
	}
	if peer != nil {
		var r api.PaneText
		fwd := p
		fwd.Session, fwd.As = bare, c.forwardAs()
		if err := peer.Caller.Call(ctx, "pane.read", fwd, &r); err != nil {
			return api.PaneText{}, peerErr(peer.Label, err)
		}
		cleanPeerText(&r)
		r.Session = peer.Label + "/" + bare
		return r, nil
	}
	u, _ := parseUUID(t.pane)
	tx, err := s.lk.ReadText(u, p.Scrollback)
	if err != nil {
		return api.PaneText{}, linkErr(t.pane, err)
	}
	r := api.PaneText{Pane: t.pane, Cols: tx.Cols, Rows: tx.Rows, Screen: tx.Screen,
		ScrollbackTotal: tx.ScrollbackTotal, Alternate: tx.Alternate, CursorCol: tx.CursorCol,
		CursorRow: tx.CursorRow, Offset: tx.Offset}
	if t.filed {
		r.Session = t.session.Name
	}
	r.Scrollback, r.Truncated = capRows(tx.Screen, tx.Scrollback, paneTextCap)
	return r, nil
}

// cleanPeerText treats another hab's answer as untrusted (hab-logic-api.md 3.11): it drops
// control characters from every string and applies the size cap again, to the screen as well.
func cleanPeerText(r *api.PaneText) {
	r.Pane = panetext.CleanControls(r.Pane)
	n, cut := 0, false
	for i, row := range r.Screen {
		row = panetext.CleanControls(row)
		if n+len(row) > paneTextCap {
			row, cut = "", true
		}
		n += len(row)
		r.Screen[i] = row
	}
	for i, row := range r.Scrollback {
		r.Scrollback[i] = panetext.CleanControls(row)
	}
	var sbCut bool
	r.Scrollback, sbCut = capRows(r.Screen, r.Scrollback, paneTextCap)
	r.Truncated = r.Truncated || cut || sbCut
}

// cleanPeerWait treats another hab's wait answer as untrusted.
func cleanPeerWait(r *api.PaneWaitResult) {
	r.Pane = panetext.CleanControls(r.Pane)
	r.Line = panetext.CleanControls(r.Line)
	if len(r.Line) > panetext.MaxLine {
		r.Line = strings.ToValidUTF8(r.Line[:panetext.MaxLine], "")
	}
	r.Reason = api.PaneWaitReason(panetext.CleanControls(string(r.Reason)))
}

// capRows keeps the newest scrollback rows that fit in max bytes beside the whole screen.
func capRows(screen, scrollback []string, max int) ([]string, bool) {
	n := 0
	for _, r := range screen {
		n += len(r)
	}
	i := len(scrollback)
	for i > 0 && n+len(scrollback[i-1]) <= max {
		n += len(scrollback[i-1])
		i--
	}
	return scrollback[i:], i > 0
}

// waitSlots bounds concurrent waits: per caller key and on the hab.
type waitSlots struct {
	mu  sync.Mutex
	all int
	by  map[string]int
}

// take holds a slot for key ("" counts only toward the hab's bound), or answers false when a
// bound is reached. release gives it back.
func (w *waitSlots) take(key string) (release func(), ok bool) {
	return w.takeN(key, paneWaitsPerCaller, paneWaitsAll)
}

// takeN is take with its own bounds: per for each caller key, all on the hab.
func (w *waitSlots) takeN(key string, per, all int) (release func(), ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.all >= all || (key != "" && w.by[key] >= per) {
		return nil, false
	}
	if w.by == nil {
		w.by = map[string]int{}
	}
	w.all++
	if key != "" {
		w.by[key]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.all--
			if key != "" {
				if w.by[key]--; w.by[key] <= 0 {
					delete(w.by, key)
				}
			}
		})
	}, true
}

// Wait serves pane.wait. The default time is 110 s from a pane, else 180 s.
func (s *paneService) Wait(ctx context.Context, c paneCaller, p api.PaneWaitParams) (api.PaneWaitResult, error) {
	if p.Match == "" && p.Pattern == "" && p.QuietMS <= 0 {
		return api.PaneWaitResult{}, control.Errorf(api.ErrorKindInvalid, "pane.wait needs match, pattern or quiet_ms")
	}
	if p.QuietMS < 0 || p.TimeoutMS < 0 {
		return api.PaneWaitResult{}, control.Errorf(api.ErrorKindInvalid, "quiet_ms and timeout_ms cannot be negative")
	}
	wait := paneWaitDefault
	if c.door == doorRoute {
		wait = paneWaitPane
	}
	if p.TimeoutMS > 0 {
		wait = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	if wait > paneWaitMax {
		return api.PaneWaitResult{}, control.Errorf(api.ErrorKindInvalid, "timeout_ms %d: at most %d", p.TimeoutMS, paneWaitMax.Milliseconds())
	}
	var re *regexp.Regexp
	if p.Pattern != "" {
		var err error
		if re, err = regexp.Compile(p.Pattern); err != nil {
			return api.PaneWaitResult{}, control.Errorf(api.ErrorKindInvalid, "pattern does not compile: %v", err)
		}
	}
	t, peer, bare, err := s.prepare("pane.wait", c, p.Pane, p.Session)
	if err != nil {
		return api.PaneWaitResult{}, err
	}
	release, ok := s.slots.take(c.key())
	if !ok {
		return api.PaneWaitResult{}, control.Errorf(api.ErrorKindUnavailable, "too many pane waits at once (at most %d per caller, %d on this hab); end one and try again", paneWaitsPerCaller, paneWaitsAll)
	}
	defer release()
	if peer != nil {
		p.As = c.forwardAs()
		return s.forwardWait(ctx, peer, bare, p, wait)
	}
	u, _ := parseUUID(t.pane)
	st, ok := s.lk.Pane(u)
	if !ok {
		return api.PaneWaitResult{}, control.Errorf(api.ErrorKindNotFound, "no pane %s on this hab", t.pane)
	}
	start := st.Offset
	from := start
	if p.From != nil {
		if *p.From > start {
			return api.PaneWaitResult{}, control.Errorf(api.ErrorKindInvalid, "from %d is past the pane's output (%d)", *p.From, start)
		}
		from = clampFrom(c, start, *p.From)
	}
	res := api.PaneWaitResult{Pane: t.pane}
	if t.filed {
		res.Session = t.session.Name
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	m := &waitMatcher{text: []byte(p.Match), re: re, every: paneOpenEvery}
	var quiet <-chan time.Time
	var qt *time.Timer
	if p.QuietMS > 0 {
		qt = time.NewTimer(time.Duration(p.QuietMS) * time.Millisecond)
		defer qt.Stop()
		quiet = qt.C
	}
	q := time.Duration(p.QuietMS) * time.Millisecond
	off := from
	for {
		sub, err := s.lk.Subscribe(ctx, u, off)
		if err != nil {
			if ctx.Err() != nil {
				return s.ended(ctx, res, off)
			}
			if errors.Is(err, link.ErrGone) || errors.Is(err, link.ErrNoPane) {
				res.Reason, res.Offset = api.PaneWaitExited, off
				return res, nil
			}
			return api.PaneWaitResult{}, linkErr(t.pane, err)
		}
		r, again, err := s.follow(ctx, sub, m, &off, start, qt, q, quiet)
		sub.Close()
		if err != nil {
			return api.PaneWaitResult{}, err
		}
		if !again {
			if r.Reason == "" {
				return s.ended(ctx, res, off)
			}
			res.Reason, res.Line, res.Offset = r.Reason, r.Line, off
			return res, nil
		}
	}
}

// clampFrom limits how far back an agent's door may replay: at most paneReplayMax bytes before
// the pane's offset start. The owner replays from any offset the holder still has.
func clampFrom(c paneCaller, start, from uint64) uint64 {
	if c.door != doorOwner && start-from > paneReplayMax {
		return start - paneReplayMax
	}
	return from
}

// forwardWait runs a wait on a peer's session in leases of at most paneLease: each lease is a
// pane.wait on the far hab from the offset the last one reached, so a near caller that goes away
// leaves the far wait running for at most one lease (hab-logic-api.md 3.11). Silence carries
// across leases for quiet: a lease that ended with no output shortens the next one's quiet_ms.
func (s *paneService) forwardWait(ctx context.Context, peer *channels.Link, bare string, p api.PaneWaitParams, wait time.Duration) (api.PaneWaitResult, error) {
	deadline := time.Now().Add(wait)
	from := p.From
	quietLeft := p.QuietMS
	var last api.PaneWaitResult
	for {
		left := time.Until(deadline)
		if left <= 0 {
			last.Reason = api.PaneWaitTimedOut
			last.Session = peer.Label + "/" + bare
			return last, nil
		}
		lease := min(left, paneLease)
		fwd := p
		fwd.Session, fwd.From, fwd.TimeoutMS = bare, from, max(int(lease/time.Millisecond), 1)
		if p.QuietMS > 0 {
			fwd.QuietMS = max(quietLeft, 1)
		}
		var r api.PaneWaitResult
		if err := peer.Caller.Call(ctx, "pane.wait", fwd, &r); err != nil {
			return api.PaneWaitResult{}, peerErr(peer.Label, err)
		}
		cleanPeerWait(&r)
		r.Session = peer.Label + "/" + bare
		if r.Reason != api.PaneWaitTimedOut {
			return r, nil
		}
		if from != nil && r.Offset == *from {
			quietLeft -= int(lease / time.Millisecond)
		} else {
			quietLeft = p.QuietMS
		}
		o := r.Offset
		from, last = &o, r
	}
}

// ended answers the end of the time, or the caller going away.
func (s *paneService) ended(ctx context.Context, res api.PaneWaitResult, off uint64) (api.PaneWaitResult, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Reason, res.Offset = api.PaneWaitTimedOut, off
		return res, nil
	}
	return api.PaneWaitResult{}, control.Errorf(api.ErrorKindUnavailable, "the wait was cancelled")
}

// follow reads one subscription. again is true when the subscription fell behind and the wait
// goes on from *off with a new one. A result with no reason means ctx ended.
func (s *paneService) follow(ctx context.Context, sub *link.Subscription, m *waitMatcher, off *uint64, start uint64, qt *time.Timer, q time.Duration, quiet <-chan time.Time) (api.PaneWaitResult, bool, error) {
	var recheck <-chan time.Time
	matched := func() (api.PaneWaitResult, bool, error) {
		return api.PaneWaitResult{Reason: api.PaneWaitMatch, Line: m.hit}, false, nil
	}
	for {
		if m.pending && recheck == nil {
			recheck = time.After(m.every)
		}
		select {
		case <-ctx.Done():
			return api.PaneWaitResult{}, false, nil
		case <-quiet:
			return api.PaneWaitResult{Reason: api.PaneWaitQuiet}, false, nil
		case now := <-recheck:
			recheck = nil
			if m.pending && m.checkOpen(now) {
				return matched()
			}
		case ch, ok := <-sub.C:
			if !ok {
				switch err := sub.Err(); {
				case errors.Is(err, link.ErrSlow):
					return api.PaneWaitResult{}, true, nil
				case errors.Is(err, link.ErrGone):
					return api.PaneWaitResult{Reason: api.PaneWaitExited}, false, nil
				case ctx.Err() != nil:
					return api.PaneWaitResult{}, false, nil
				default:
					return api.PaneWaitResult{}, false, linkErr("", err)
				}
			}
			switch ch.Kind {
			case link.ChunkSnapshot:
				// Bytes from before ch.Offset are lost: go on after them.
				m.reset()
				*off = ch.Offset
			case link.ChunkExited:
				return api.PaneWaitResult{Reason: api.PaneWaitExited}, false, nil
			case link.ChunkBytes:
				end := ch.Offset + uint64(len(ch.Data))
				if end > start && qt != nil {
					// New output since the wait began: quiet counts from now.
					if !qt.Stop() {
						select {
						case <-qt.C:
						default:
						}
					}
					qt.Reset(q)
				}
				*off = end
				if m.feed(ch.Data, time.Now()) {
					return matched()
				}
			}
		}
	}
}

// waitMatcher checks new output against the literal and the pattern at a bounded cost
// (hab-logic-api.md 3.11). Each ended line is checked once. On the open line, the literal is
// searched only in the bytes added since the last search (with an overlap of its length), and
// the pattern is tried at most once per every, plus once when that time has passed with the
// line changed.
type waitMatcher struct {
	text  []byte
	re    *regexp.Regexp
	every time.Duration

	lines   panetext.Lines
	gen     uint64    // the open line's Gen when last examined
	litFrom int       // open-line bytes already searched for text
	lastRe  time.Time // when the pattern last ran on the open line
	pending bool      // the open line changed since the pattern last ran on it
	hit     string    // the matched line
	work    int       // bytes examined; a test bounds it
}

// feed reads b and reports a match.
func (m *waitMatcher) feed(b []byte, now time.Time) bool {
	if m.lines.Feed(b, func(line string) bool {
		m.litFrom, m.pending = 0, false
		return m.check(line)
	}) {
		return true
	}
	if m.lines.Gen() == m.gen {
		return false
	}
	m.gen = m.lines.Gen()
	open := m.lines.OpenBytes()
	if len(m.text) > 0 && len(open) > 0 {
		from := max(0, m.litFrom-len(m.text)+1)
		m.work += len(open) - from
		m.litFrom = len(open)
		if bytes.Contains(open[from:], m.text) {
			m.hit = m.lines.Open()
			return true
		}
	}
	if m.re != nil && len(open) > 0 {
		if now.Sub(m.lastRe) >= m.every {
			return m.checkOpen(now)
		}
		m.pending = true
	}
	return false
}

// checkOpen tries the pattern on the open line now.
func (m *waitMatcher) checkOpen(now time.Time) bool {
	m.pending, m.lastRe = false, now
	line := m.lines.Open()
	m.work += len(line)
	if m.re.MatchString(line) {
		m.hit = line
		return true
	}
	return false
}

// check tries an ended line.
func (m *waitMatcher) check(line string) bool {
	m.work += len(line)
	if (len(m.text) > 0 && strings.Contains(line, string(m.text))) || (m.re != nil && m.re.MatchString(line)) {
		m.hit = line
		return true
	}
	return false
}

// reset forgets the open line: bytes before a snapshot chunk were lost.
func (m *waitMatcher) reset() {
	m.lines = panetext.Lines{}
	m.gen, m.litFrom, m.pending = 0, 0, false
}

// linkErr maps a link error for pane.read and pane.wait.
func linkErr(pane string, err error) error {
	switch {
	case errors.Is(err, link.ErrNoPane):
		return control.Errorf(api.ErrorKindNotFound, "no pane %s on this hab", pane)
	case errors.Is(err, link.ErrGone), errors.Is(err, link.ErrClosed):
		return control.Errorf(api.ErrorKindConflict, "pane %s is closed", pane)
	case errors.Is(err, link.ErrLost):
		return control.Errorf(api.ErrorKindUnavailable, "pane %s is lost: its holder is not reached", pane)
	case errors.Is(err, link.ErrAhead):
		return control.Errorf(api.ErrorKindInvalid, "from is past the pane's output")
	}
	return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
}

// peerErr keeps a peer's refusal kind, with its text cleaned of control characters (another
// hab's text is untrusted), and names the label for a link that failed.
func peerErr(label string, err error) error {
	var ce *control.Error
	if errors.As(err, &ce) {
		out := *ce
		out.Message = panetext.CleanControls(ce.Message)
		out.Data.Hint = panetext.CleanControls(ce.Data.Hint)
		out.Data.Kind = api.ErrorKind(panetext.CleanControls(string(ce.Data.Kind)))
		return &out
	}
	return control.Errorf(api.ErrorKindUnavailable, "peer %s: %s", label, panetext.CleanControls(err.Error()))
}

// controlPanes serves pane.read and pane.wait on the control connection.
type controlPanes struct{ s *paneService }

var _ control.Panes = controlPanes{}

func (c controlPanes) Read(ctx context.Context, from control.PaneCaller, p api.PaneReadParams) (api.PaneText, error) {
	return c.s.Read(ctx, c.s.markCapcom(controlCaller(from), from.Device, p.As), p)
}

func (c controlPanes) Wait(ctx context.Context, from control.PaneCaller, p api.PaneWaitParams) (api.PaneWaitResult, error) {
	return c.s.Wait(ctx, c.s.markCapcom(controlCaller(from), from.Device, p.As), p)
}

func (c controlPanes) List(ctx context.Context, from control.PaneCaller, p api.PaneListParams) (api.PaneList, error) {
	return c.s.List(ctx, c.s.markCapcom(controlCaller(from), from.Device, p.As), p)
}

func (c controlPanes) Type(ctx context.Context, from control.PaneCaller, p api.PaneTypeParams) (api.PaneTyped, error) {
	return c.s.Type(ctx, c.s.markCapcom(controlCaller(from), from.Device, p.As), p)
}

func (c controlPanes) New(ctx context.Context, from control.PaneCaller, p api.PaneNewParams) (api.PaneStarted, error) {
	return c.s.New(ctx, controlCaller(from), p)
}

func controlCaller(from control.PaneCaller) paneCaller {
	if from.Peer {
		return paneCaller{door: doorPeer, conn: from.Conn}
	}
	return paneCaller{door: doorOwner}
}

// paneHook serves the pane methods on the pane route: the token's pane is the caller.
func paneHook(s *paneService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		c := paneCaller{door: doorRoute, pane: pane}
		var r any
		var err error
		switch method {
		case "pane.read":
			r, err = routeCall(params, method, func(p api.PaneReadParams) (any, error) { return s.Read(ctx, c, p) })
		case "pane.wait":
			r, err = routeCall(params, method, func(p api.PaneWaitParams) (any, error) { return s.Wait(ctx, c, p) })
		case "pane.list":
			r, err = routeCall(params, method, func(p api.PaneListParams) (any, error) { return s.List(ctx, c, p) })
		case "pane.type":
			r, err = routeCall(params, method, func(p api.PaneTypeParams) (any, error) { return s.Type(ctx, c, p) })
		case "pane.new":
			r, err = routeCall(params, method, func(p api.PaneNewParams) (any, error) { return s.New(ctx, c, p) })
		default:
			err = control.Errorf(api.ErrorKindInvalid, "%q is not a pane method", method)
		}
		return r, nil, err
	}
}

// routeCall decodes a route call's params and runs it.
func routeCall[P any](params json.RawMessage, method string, run func(P) (any, error)) (any, error) {
	var p P
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err)
	}
	return run(p)
}
