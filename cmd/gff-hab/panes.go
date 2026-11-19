package main

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
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

// paneCaller is who asks: the door, and for the pane route the caller's pane id.
type paneCaller struct {
	door readDoor
	pane string
}

// paneService serves pane.read and pane.wait.
type paneService struct {
	lk    *link.Link
	store *structure.Store
	// rule returns agent.read_panes now.
	rule func() string
	// links lists the peer links this hab dialed; channelPeers over the peer manager.
	links func() []channels.Link
	// self and host are this hab's device id and host id: a label/name that names this hab
	// reads here.
	self, host string
	logf       func(string, ...any)
}

// paneTarget is a resolved local target: the pane, and its session when it is filed.
type paneTarget struct {
	pane    string
	session api.Session
	filed   bool
}

// ruleValue reads agent.read_panes: a word the settings service checked, else all (its
// default).
func ruleValue(s *settings.Service) func() string {
	return func() string {
		var w string
		if err := json.Unmarshal(s.Value(settings.AgentReadPanes), &w); err != nil || w == "" {
			return readPanesAll
		}
		return w
	}
}

// mayRead is the rule (hab-logic-api.md 3.11). The owner reads every pane. A peer link reads
// every pane under all, else none. The pane route reads its own pane always, the panes of its
// own session's project under project, and every pane under all.
func mayRead(rule string, c paneCaller, callerProject string, t paneTarget) bool {
	switch c.door {
	case doorOwner:
		return true
	case doorPeer:
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

// check applies the rule. A refusal is logged with ids only (diagnostics.md 10.1) and answers
// denied with a hint that names the setting, never the target.
func (s *paneService) check(method string, c paneCaller, t paneTarget) (string, error) {
	rule := s.rule()
	project := ""
	if c.door == doorRoute {
		if ss, ok := s.store.SessionByPane(c.pane); ok {
			project = ss.Project
		}
	}
	if mayRead(rule, c, project, t) {
		return rule, nil
	}
	from := c.pane
	if c.door == doorPeer {
		from = "peer link"
	}
	s.logf("panes: %s denied: agent.read_panes %s, caller %s, target pane %s", method, rule, from, t.pane)
	why := "its own pane only"
	switch {
	case c.door == doorPeer:
		why = "no pane for an agent on another hab"
	case rule == readPanesProject:
		why = "its own pane and its own project's panes only"
	}
	return rule, control.Errorf(api.ErrorKindDenied, "agent.read_panes is %s on this hab: an agent may read %s. The owner changes it in Flightdeck's settings", rule, why)
}

// resolve finds the target of a call. A label/name for a peer returns the link to forward on,
// with the bare name.
func (s *paneService) resolve(c paneCaller, pane, session string) (paneTarget, *channels.Link, string, error) {
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
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindInvalid, "a call over a peer link reads only this hab's own panes: a forward never forwards")
		case peer == nil:
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindNotFound, "no peer labelled %q in the peer list", label)
		case !peer.Online || peer.Caller == nil:
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindUnavailable, "peer %s is not online from this hab, so its panes cannot be read; try again once it is online", label)
		case !containsWord(peer.Caps, paneReadCapability):
			return paneTarget{}, nil, "", control.Errorf(api.ErrorKindUnavailable, "peer %s runs without pane-read, so its panes cannot be read; update its gff-hab", label)
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

func containsWord(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

// Read serves pane.read.
func (s *paneService) Read(ctx context.Context, c paneCaller, p api.PaneReadParams) (api.PaneText, error) {
	if p.Scrollback < 0 || p.Scrollback > link.MaxScrollbackPage {
		return api.PaneText{}, control.Errorf(api.ErrorKindInvalid, "scrollback %d: want 0 to %d rows", p.Scrollback, link.MaxScrollbackPage)
	}
	t, peer, bare, err := s.resolve(c, p.Pane, p.Session)
	if err != nil {
		return api.PaneText{}, err
	}
	if peer != nil {
		var r api.PaneText
		fwd := p
		fwd.Session = bare
		if err := peer.Caller.Call(ctx, "pane.read", fwd, &r); err != nil {
			return api.PaneText{}, peerErr(peer.Label, err)
		}
		r.Session = peer.Label + "/" + bare
		return r, nil
	}
	if _, err := s.check("pane.read", c, t); err != nil {
		return api.PaneText{}, err
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

// Wait serves pane.wait. door decides the default time: 110 s from a pane.
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
	t, peer, bare, err := s.resolve(c, p.Pane, p.Session)
	if err != nil {
		return api.PaneWaitResult{}, err
	}
	if peer != nil {
		var r api.PaneWaitResult
		fwd := p
		fwd.Session, fwd.TimeoutMS = bare, int(wait/time.Millisecond)
		if err := peer.Caller.Call(ctx, "pane.wait", fwd, &r); err != nil {
			return api.PaneWaitResult{}, peerErr(peer.Label, err)
		}
		r.Session = peer.Label + "/" + bare
		return r, nil
	}
	if _, err := s.check("pane.wait", c, t); err != nil {
		return api.PaneWaitResult{}, err
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
		from = *p.From
	}
	res := api.PaneWaitResult{Pane: t.pane}
	if t.filed {
		res.Session = t.session.Name
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	m := lineMatcher{text: p.Match, re: re, quietMS: p.QuietMS}
	var quiet <-chan time.Time
	var qt *time.Timer
	if p.QuietMS > 0 {
		qt = time.NewTimer(time.Duration(p.QuietMS) * time.Millisecond)
		defer qt.Stop()
		quiet = qt.C
	}
	var lines panetext.Lines
	off := from
	for {
		sub, err := s.lk.Subscribe(ctx, u, off)
		if err != nil {
			if ctx.Err() != nil {
				return s.ended(res, off, ctx)
			}
			if errors.Is(err, link.ErrGone) || errors.Is(err, link.ErrNoPane) {
				res.Reason, res.Offset = api.PaneWaitExited, off
				return res, nil
			}
			return api.PaneWaitResult{}, linkErr(t.pane, err)
		}
		r, again, err := s.follow(ctx, sub, &lines, &m, &off, start, qt, quiet)
		sub.Close()
		if err != nil {
			return api.PaneWaitResult{}, err
		}
		if !again {
			if r.Reason == "" {
				return s.ended(res, off, ctx)
			}
			res.Reason, res.Line, res.Offset = r.Reason, r.Line, off
			return res, nil
		}
	}
}

// ended answers the end of the time, or the caller going away.
func (s *paneService) ended(res api.PaneWaitResult, off uint64, ctx context.Context) (api.PaneWaitResult, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Reason, res.Offset = api.PaneWaitTimedOut, off
		return res, nil
	}
	return api.PaneWaitResult{}, control.Errorf(api.ErrorKindUnavailable, "the wait was cancelled")
}

// follow reads one subscription. again is true when the subscription fell behind and the wait
// goes on from *off with a new one. A result with no reason means ctx ended.
func (s *paneService) follow(ctx context.Context, sub *link.Subscription, lines *panetext.Lines, m *lineMatcher, off *uint64, start uint64, qt *time.Timer, quiet <-chan time.Time) (api.PaneWaitResult, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return api.PaneWaitResult{}, false, nil
		case <-quiet:
			return api.PaneWaitResult{Reason: api.PaneWaitQuiet}, false, nil
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
				*lines = panetext.Lines{}
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
					qt.Reset(time.Duration(m.quietMS) * time.Millisecond)
				}
				var hit string
				lines.Feed(ch.Data, func(line string, done bool) bool {
					if m.matches(line) {
						hit = line
						return true
					}
					return false
				})
				*off = end
				if hit != "" || (m.matched) {
					return api.PaneWaitResult{Reason: api.PaneWaitMatch, Line: hit}, false, nil
				}
			}
		}
	}
}

// lineMatcher checks one line against the literal and the pattern.
type lineMatcher struct {
	text    string
	re      *regexp.Regexp
	quietMS int
	matched bool
}

func (m *lineMatcher) matches(line string) bool {
	if (m.text != "" && strings.Contains(line, m.text)) || (m.re != nil && m.re.MatchString(line)) {
		m.matched = true
		return true
	}
	return false
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

// peerErr keeps a peer's refusal as it is (its kind and hint), and names the label for a link
// that failed.
func peerErr(label string, err error) error {
	var ce *control.Error
	if errors.As(err, &ce) {
		return err
	}
	return control.Errorf(api.ErrorKindUnavailable, "peer %s: %v", label, err)
}

// controlPanes serves pane.read and pane.wait on the control connection.
type controlPanes struct{ s *paneService }

var _ control.Panes = controlPanes{}

func (c controlPanes) Read(ctx context.Context, peer bool, p api.PaneReadParams) (api.PaneText, error) {
	return c.s.Read(ctx, controlCaller(peer), p)
}

func (c controlPanes) Wait(ctx context.Context, peer bool, p api.PaneWaitParams) (api.PaneWaitResult, error) {
	return c.s.Wait(ctx, controlCaller(peer), p)
}

func controlCaller(peer bool) paneCaller {
	if peer {
		return paneCaller{door: doorPeer}
	}
	return paneCaller{door: doorOwner}
}

// paneHook serves pane.read and pane.wait on the pane route: the token's pane is the caller.
func paneHook(s *paneService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		c := paneCaller{door: doorRoute, pane: pane}
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		if method == "pane.read" {
			var p api.PaneReadParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			r, err := s.Read(ctx, c, p)
			return r, nil, err
		}
		var p api.PaneWaitParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil, bad(err)
		}
		r, err := s.Wait(ctx, c, p)
		return r, nil, err
	}
}
