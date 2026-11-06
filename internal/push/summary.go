package push

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/noahkiss/flightdeck/api"
)

// Model-written summaries (app.md 16.5).
const (
	// DefaultSummaryWait is how long a permission banner waits for its summary, from when
	// the notifier first saw the request.
	DefaultSummaryWait = 1500 * time.Millisecond
	// summaryLate bounds the whole summary call: one that lands after the banner went out,
	// and within this, replaces the banner once.
	summaryLate = 10 * time.Second
	// summaryKeep drops a pending summary whose request never resolved through the notifier.
	summaryKeep = 10 * time.Minute
)

// Summarize asks for a model-written summary of a permission request: w names where it runs,
// description is the harness's own summary of a command (16.4). It returns the model's line;
// an error, or a line that does not fit the title budget whole, leaves the banner as before.
type Summarize func(ctx context.Context, r *api.PermissionRequest, w Where, description string) (string, error)

// Summarizable reports whether a request gets a model-written summary: a permission request
// that is not a plan or a question.
func Summarizable(r *api.PermissionRequest) bool {
	return r != nil && r.Tool != toolPlan && r.Tool != toolQuestion
}

// summaryTitle is a summary as a banner title: a question in 4 words and 28 characters with
// its "?", taken only whole. "" when the request takes no summary or the line does not fit.
func summaryTitle(r *api.PermissionRequest, sum string) string {
	if sum == "" || !Summarizable(r) {
		return ""
	}
	if strings.IndexFunc(sum, func(c rune) bool { return unicode.IsControl(c) || unicode.Is(unicode.Cf, c) }) >= 0 {
		return ""
	}
	s := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sum), "?"))
	if s == "" || !Fits(s, 4, 27) {
		return ""
	}
	return s + "?"
}

// pendingSummary is one request's summary call. line is set before done closes.
type pendingSummary struct {
	pane  string
	start time.Time // wall clock, for the wait: when the notifier first saw the request
	done  chan struct{}
	line  string // the fitted title, "" when the call failed or the line did not fit
}

// sumKey names one request of a pane: its id, and what it asks, for a request with no id.
func sumKey(st api.AgentState) string {
	r := st.Request
	return st.Pane + "\x00" + r.ID + "\x00" + r.Tool + "\x00" + r.Command + "\x00" + strings.Join(r.Files, "\x00")
}

func (n *Notifier) summaryWait() time.Duration {
	if n.cfg.SummaryWait > 0 {
		return n.cfg.SummaryWait
	}
	return DefaultSummaryWait
}

// wantsSummary reports whether a state's request takes a summary.
func (n *Notifier) wantsSummary(st api.AgentState) bool {
	return n.cfg.Summarize != nil && st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindPermission && Summarizable(st.Request)
}

// prefetch starts the summary call for a new permission request, before the worker gets to
// its banner. arrived is when State first queued it: the wait runs from then, so requests that
// arrive together, or while the worker waits on another, never wait one after the other.
// Nothing goes to the model for a state that raises no banner (a replayed screen report),
// or when every device that would get the banner hides details or is held by presence.
// Worker goroutine only.
func (n *Notifier) prefetch(st api.AgentState, arrived time.Time) {
	if !n.wantsSummary(st) {
		return
	}
	prev, seen := n.last[st.Pane]
	if seen && key(prev) == key(st) || !seen && st.Source == api.StateSourceScreen {
		return
	}
	k := sumKey(st)
	if _, ok := n.sums[k]; ok {
		return
	}
	w := n.cfg.Where(st.Pane, "")
	w.Hab = n.cfg.Hab
	if w.Harness == "" {
		w.Harness = st.Harness
	}
	shows := false
	for _, d := range n.cfg.Store.Devices() {
		shows = shows || (d.Token != "" && d.Wants(KindPermission) && !d.HideCommands && n.tier(d.ID, w.Session) == TierPush)
	}
	if !shows {
		return
	}
	now := time.Now()
	for key, p := range n.sums {
		if now.Sub(p.start) > summaryKeep {
			delete(n.sums, key)
		}
	}
	if arrived.IsZero() || arrived.After(now) {
		arrived = now
	}
	p := &pendingSummary{pane: st.Pane, start: arrived, done: make(chan struct{})}
	n.sums[k] = p
	req := st.Request
	desc := commandDescription(req)
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, summaryLate)
		defer cancel()
		line, err := n.cfg.Summarize(ctx, req, w, desc)
		if err == nil {
			p.line = summaryTitle(req, line)
		}
		close(p.done)
		if p.line == "" {
			return
		}
		n.mu.Lock()
		if !n.closed {
			n.jobs = append(n.jobs, func() { n.lateSummary(k, p) })
		}
		n.mu.Unlock()
		n.signal()
	}()
}

// summaryFor waits for a state's summary until its wait from arrival ends, and returns it, or
// "" when there is none by then.
func (n *Notifier) summaryFor(st api.AgentState) string {
	if !n.wantsSummary(st) {
		return ""
	}
	p := n.sums[sumKey(st)]
	if p == nil {
		return ""
	}
	if left := time.Until(p.start.Add(n.summaryWait())); left > 0 {
		t := time.NewTimer(left)
		select {
		case <-p.done:
		case <-t.C:
		case <-n.ctx.Done():
		}
		t.Stop()
	}
	return readyLine(p)
}

// readySummary is a state's summary when it has landed, without waiting.
func (n *Notifier) readySummary(st api.AgentState) string {
	if !n.wantsSummary(st) {
		return ""
	}
	return readyLine(n.sums[sumKey(st)])
}

func readyLine(p *pendingSummary) string {
	if p == nil {
		return ""
	}
	select {
	case <-p.done:
		return p.line
	default:
		return ""
	}
}

// openSummary is the summary of an open approval: the one its banner showed, else one that
// landed since.
func (n *Notifier) openSummary(ob *sent) string {
	if ob.summary != "" || ob.sumKey == "" {
		return ob.summary
	}
	return readyLine(n.sums[ob.sumKey])
}

// lateSummary replaces an approval banner that went out without its summary, once, while its
// request is still open: same collapse id, a passive alert with no sound (app.md 16.5).
func (n *Notifier) lateSummary(k string, p *pendingSummary) {
	ob := n.open[p.pane]
	if ob == nil || ob.sumKey != k || ob.summary != "" || time.Since(p.start) > summaryLate {
		return
	}
	ob.summary = p.line
	w := n.cfg.Where(p.pane, "")
	w.Hab = n.cfg.Hab
	for _, t := range ob.to {
		if n.hideTarget(t) {
			continue
		}
		b := permissionBanner(w, ob.req, false, p.line)
		b.Level, b.Category = LevelPassive, t.category
		payload := AlertPayload(b, ob.meta, w.Session)
		n.send(Message{Token: t.token, Environment: t.env, Type: TypeAlert, Priority: priority(b.Level), CollapseID: ob.collapse, Expiration: ob.expires, Payload: payload}, true)
	}
}

// hideTarget respects both the banner's original setting and the device's current setting.
func (n *Notifier) hideTarget(t target) bool {
	if t.hide {
		return true
	}
	for _, d := range n.cfg.Store.Devices() {
		if d.ID == t.device && d.Token == t.token {
			return d.HideCommands
		}
	}
	return true
}
