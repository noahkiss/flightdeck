package push

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// summarizer is a fake summary endpoint: it answers line after delay, or err.
type summarizer struct {
	mu    sync.Mutex
	line  string
	err   error
	delay time.Duration
	gate  chan struct{} // non-nil: the answer waits for it to close
	calls atomic.Int32
	descs []string
}

func (s *summarizer) fn(ctx context.Context, r *api.PermissionRequest, w Where, desc string) (string, error) {
	s.calls.Add(1)
	s.mu.Lock()
	line, err, delay, gate := s.line, s.err, s.delay, s.gate
	s.descs = append(s.descs, desc)
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return line, err
}

func summaryRig(t *testing.T, s *summarizer, wait time.Duration) *notifierRig {
	return newNotifierRig(t, func(c *NotifierConfig) { c.Summarize, c.SummaryWait = s.fn, wait })
}

// waitFor polls cond for up to 3 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

// TestSummaryBanner: the summary titles the banner and names the answered line; the body and
// the actions stay as before; a device that hides details gets neither.
func TestSummaryBanner(t *testing.T) {
	s := &summarizer{line: "Run the test suite?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.Register("dev-mac", mac, Development, allKinds, true)

	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	pa, ma := alerts(r.prod.requests(), phone), alerts(r.dev.requests(), mac)
	if len(pa) != 1 || len(ma) != 1 {
		t.Fatalf("banners: phone %d, mac %d", len(pa), len(ma))
	}
	if alertField(pa[0], "title") != "Run the test suite?" || alertField(pa[0], "body") != "make test" {
		t.Fatalf("phone banner %v", pa[0].Body)
	}
	if aps := pa[0].Body["aps"].(map[string]any); aps["category"] != CategoryApproval {
		t.Fatalf("the summary changed the actions: %v", aps)
	}
	if alertField(ma[0], "title") != "Approval needed" || strings.Contains(alertField(ma[0], "body").(string), "test") {
		t.Fatalf("hiding device got details: %v", ma[0].Body)
	}
	r.n.Answering("p1", "req-1", "phone", true)
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa, ma = alerts(r.prod.requests(), phone), alerts(r.dev.requests(), mac)
	if alertField(pa[1], "body") != "Allowed: Run the test suite" {
		t.Fatalf("answered %v", pa[1].Body)
	}
	if b, _ := alertField(ma[1], "body").(string); strings.Contains(b, "test") || strings.Contains(b, "Run") {
		t.Fatalf("hiding device's answered line %v", ma[1].Body)
	}
	if s.calls.Load() != 1 {
		t.Fatalf("summarised %d times", s.calls.Load())
	}
}

// TestSummaryActionsUnchanged: a command too long to show whole keeps Deny and Open with a
// summary; one that fits keeps Approve.
func TestSummaryActionsUnchanged(t *testing.T) {
	s := &summarizer{line: "Deploy the site?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	long := "rsync -avz --delete " + strings.Repeat("build/output/assets/ ", 10) + "deploy@host:/srv/www"
	r.n.State(blocked("req-1", long))
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 1 || alertField(pa[0], "title") != "Deploy the site?" {
		t.Fatalf("banner %v", pa)
	}
	if aps := pa[0].Body["aps"].(map[string]any); aps["category"] != CategoryDeny {
		t.Fatalf("a summary made a cut command approvable: %v", aps)
	}
}

// TestSummaryNotUsed: plans and questions take no summary; a summary that does not fit, one
// with control characters, and a failure leave the banner as before.
func TestSummaryNotUsed(t *testing.T) {
	plan := blocked("req-p", "")
	plan.Request.Tool, plan.Request.Command, plan.Request.Input = "ExitPlanMode", "", api.RawJSON(`{"plan":"Do it"}`)
	question := blocked("req-q", "")
	question.Request.Tool, question.Request.Command = "AskUserQuestion", ""
	question.Request.Input = api.RawJSON(`{"questions":[{"question":"Which auth?","options":[{"label":"Clerk"}]}]}`)
	s := &summarizer{line: "Do things?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.n.State(plan)
	r.n.Sync()
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	r.n.State(question)
	r.n.Sync()
	if s.calls.Load() != 0 {
		t.Fatalf("plan or question summarised: %d", s.calls.Load())
	}
	pa := asks(r.prod.requests(), phone)
	if alertField(pa[0], "title") != "Approve plan?" || alertField(pa[1], "title") != "Which auth?" {
		t.Fatalf("titles %v %v", alertField(pa[0], "title"), alertField(pa[1], "title"))
	}

	for name, sum := range map[string]*summarizer{
		"too long": {line: "Run the whole test suite again now?"},
		"control":  {line: "Run‮tests?"},
		"error":    {err: errors.New("HTTP 500")},
		"empty":    {line: ""},
	} {
		r := summaryRig(t, sum, time.Second)
		r.store.Register("dev-phone", phone, Production, allKinds, false)
		r.n.State(blocked("req-1", "make test"))
		r.n.Sync()
		pa := alerts(r.prod.requests(), phone)
		if len(pa) != 1 || alertField(pa[0], "title") != "Run make test?" {
			t.Errorf("%s: banner %v", name, pa)
		}
	}
}

// TestSummaryAllHidden: when every device hides details, nothing goes to the model.
func TestSummaryAllHidden(t *testing.T) {
	s := &summarizer{line: "Run tests?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, true)
	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	if s.calls.Load() != 0 {
		t.Fatal("summarised for a device that hides details")
	}
	if pa := alerts(r.prod.requests(), phone); len(pa) != 1 || alertField(pa[0], "title") != "Approval needed" {
		t.Fatalf("banner %v", pa)
	}
}

// TestSummaryHarnessDescriptionPassed: the harness's own summary goes to the model.
func TestSummaryHarnessDescriptionPassed(t *testing.T) {
	s := &summarizer{line: "Check notes?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	st := blocked("req-1", "test -f notes.txt")
	st.Request.Input = api.RawJSON(`{"command":"test -f notes.txt","description":"Check whether notes.txt exists."}`)
	r.n.State(st)
	r.n.Sync()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.descs) != 1 || s.descs[0] != "Check whether notes.txt exists" {
		t.Fatalf("description %q", s.descs)
	}
}

// TestSummaryWaitFromArrival: requests that arrive together wait together, from arrival.
func TestSummaryWaitFromArrival(t *testing.T) {
	s := &summarizer{line: "Run tests?", delay: time.Minute}
	const wait = 300 * time.Millisecond
	r := summaryRig(t, s, wait)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	start := time.Now()
	r.held(func() {
		for _, p := range []string{"p1", "p2", "p3"} {
			st := blocked("req-"+p, "make test")
			st.Pane = p
			r.n.State(st)
		}
	})
	r.n.Sync()
	el := time.Since(start)
	if n := len(alerts(r.prod.requests(), phone)); n != 3 {
		t.Fatalf("%d banners", n)
	}
	if el < wait || el > 2*wait {
		t.Fatalf("three banners took %v with a %v wait", el, wait)
	}
	for _, q := range alerts(r.prod.requests(), phone) {
		if alertField(q, "title") != "Run make test?" {
			t.Fatalf("title %v", alertField(q, "title"))
		}
	}
}

// TestSummaryLate: a summary that lands after the banner replaces it once, quietly, with the
// same collapse id and actions, and names the answered line. One that lands after the
// request resolved replaces nothing.
func TestSummaryLate(t *testing.T) {
	gate := make(chan struct{})
	s := &summarizer{line: "Run the tests?", gate: gate}
	r := summaryRig(t, s, 50*time.Millisecond)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 1 || alertField(pa[0], "title") != "Run make test?" {
		t.Fatalf("first banner %v", pa)
	}
	close(gate)
	waitFor(t, func() bool { r.n.Sync(); return len(alerts(r.prod.requests(), phone)) == 2 })
	pa = alerts(r.prod.requests(), phone)
	late := pa[1]
	aps := late.Body["aps"].(map[string]any)
	if alertField(late, "title") != "Run the tests?" || alertField(late, "body") != "make test" ||
		late.Header.Get("apns-collapse-id") != "r-req-1" || aps["interruption-level"] != "passive" || aps["sound"] != nil || aps["category"] != CategoryApproval {
		t.Fatalf("replacement %v %v", late.Header, late.Body)
	}
	time.Sleep(50 * time.Millisecond)
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 2 {
		t.Fatalf("replaced %d times", n-1)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	if alertField(pa[2], "body") != "Run the tests" {
		t.Fatalf("answered %v", pa[2].Body)
	}

	// Resolved before the summary lands: no replacement.
	gate2 := make(chan struct{})
	s.mu.Lock()
	s.gate = gate2
	s.mu.Unlock()
	r.n.State(blocked("req-2", "go vet ./..."))
	r.n.Sync()
	r.n.State(state(api.AgentStatusIdle))
	r.n.Sync()
	before := len(alerts(r.prod.requests(), phone))
	close(gate2)
	waitFor(t, func() bool { return s.calls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != before {
		t.Fatalf("a resolved request was replaced: %d alerts, want %d", n, before)
	}
}

// TestSummaryWaitMidBatch: a request that arrives while the notifier waits on another's
// summary waits its own time from its arrival, not after the first wait ends.
func TestSummaryWaitMidBatch(t *testing.T) {
	s := &summarizer{line: "Run tests?", delay: time.Minute}
	const wait = 400 * time.Millisecond
	r := summaryRig(t, s, wait)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.n.State(blocked("req-a", "make test"))
	time.Sleep(wait / 2)
	b := blocked("req-b", "go vet ./...")
	b.Pane = "p2"
	sentB := time.Now()
	r.n.State(b)
	waitFor(t, func() bool { return len(alerts(r.prod.requests(), phone)) == 2 })
	if el := time.Since(sentB); el > wait+wait/4 {
		t.Fatalf("the second banner came %v after its request, with a %v wait", el, wait)
	}
}

// TestSummaryHeldDevice: no summary is asked for a banner presence holds back.
func TestSummaryHeldDevice(t *testing.T) {
	s := &summarizer{line: "Run tests?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.n.Bind("c-phone", "dev-phone")
	r.connect("c-phone", true)
	r.n.Focus("c-phone", "s1", true)
	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	if s.calls.Load() != 0 || len(alerts(r.prod.requests(), phone)) != 0 {
		t.Fatalf("calls %d, alerts %d", s.calls.Load(), len(alerts(r.prod.requests(), phone)))
	}
}

// A late worker must not extend the replacement window past the request's arrival.
func TestSummaryLateAfterArrivalDeadline(t *testing.T) {
	gate := make(chan struct{})
	s := &summarizer{line: "Run tests?", gate: gate}
	r := summaryRig(t, s, 20*time.Millisecond)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	st := blocked("req-1", "make test")
	r.n.State(st)
	r.n.Sync()
	r.n.mu.Lock()
	r.n.jobs = append(r.n.jobs, func() { r.n.sums[sumKey(st)].start = time.Now().Add(-summaryLate - time.Second) })
	r.n.mu.Unlock()
	r.n.signal()
	r.n.Sync()
	close(gate)
	waitFor(t, func() bool { r.n.Sync(); return readyForTest(r.n, st) })
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 1 {
		t.Fatalf("late replacement sent: %d alerts", n)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if alertField(pa[1], "body") != "Run tests" {
		t.Fatalf("answered line %v", pa[1].Body)
	}
}

// readyForTest observes completion through a worker job, avoiding access to the worker's map.
func readyForTest(n *Notifier, st api.AgentState) bool {
	result := make(chan bool, 1)
	n.mu.Lock()
	n.jobs = append(n.jobs, func() { result <- n.readySummary(st) != "" })
	n.mu.Unlock()
	n.signal()
	return <-result
}

// An unchanged state never sends its request to the model after a device changes its setting.
func TestSummaryUnchangedState(t *testing.T) {
	s := &summarizer{line: "Run tests?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, true)
	st := blocked("req-1", "make test")
	r.n.State(st)
	r.n.Sync()
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	st.Request.Command = "make test changed"
	r.n.State(st)
	r.n.Sync()
	if n := s.calls.Load(); n != 0 {
		t.Fatalf("unchanged state sent %d requests", n)
	}
}

// A device that hides details while a summary is pending receives no detailed replacement.
func TestSummaryHideWhilePending(t *testing.T) {
	gate := make(chan struct{})
	s := &summarizer{line: "Run tests?", gate: gate}
	r := summaryRig(t, s, 20*time.Millisecond)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	st := blocked("req-1", "make test")
	r.n.State(st)
	r.n.Sync()
	r.store.Register("dev-phone", phone, Production, allKinds, true)
	close(gate)
	waitFor(t, func() bool { r.n.Sync(); return readyForTest(r.n, st) })
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 1 {
		t.Fatalf("detailed replacement sent: %d alerts", n)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if body, _ := alertField(pa[1], "body").(string); strings.Contains(body, "test") {
		t.Fatalf("details in answered line: %q", body)
	}
}

// A device that changes its setting during the initial wait receives a hidden banner.
func TestSummaryHideDuringWait(t *testing.T) {
	gate := make(chan struct{})
	s := &summarizer{line: "Run tests?", gate: gate}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.n.State(blocked("req-1", "make test"))
	waitFor(t, func() bool { return s.calls.Load() == 1 })
	r.store.Register("dev-phone", phone, Production, allKinds, true)
	close(gate)
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 1 || alertField(pa[0], "title") != "Approval needed" {
		t.Fatalf("banner after hiding details: %v", pa)
	}
	if body, _ := alertField(pa[0], "body").(string); strings.Contains(body, "test") {
		t.Fatalf("details in banner: %q", body)
	}
}

// Trust banners keep their title and action category when summaries are enabled.
func TestSummaryTrustUnchanged(t *testing.T) {
	s := &summarizer{line: "Wrong title?"}
	r := summaryRig(t, s, time.Second)
	r.store.Register("dev-phone", phone, Production, []string{KindTrust}, false)
	st := state(api.AgentStatusBlocked)
	st.Kind, st.Source, st.Trust = api.BlockedKindTrust, api.StateSourceScreen, &api.TrustPrompt{Folder: "/work/new-app"}
	r.n.State(st)
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 1 || alertField(pa[0], "title") != "Trust this folder?" {
		t.Fatalf("trust banner: %v", pa)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	if len(pa) != 2 || alertField(pa[1], "title") != "Trust prompt answered" {
		t.Fatalf("answered trust banner: %v", pa)
	}
	if s.calls.Load() != 0 {
		t.Fatalf("trust summarized: %d", s.calls.Load())
	}
}
