package push

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

type notifierRig struct {
	*testRig
	store *Store
	n     *Notifier

	mu        sync.Mutex
	connected map[string]bool
	logs      []string
}

var (
	phone    = strings.Repeat("a1", 32)
	mac      = strings.Repeat("b2", 32)
	phoneLA  = strings.Repeat("c3", 32)
	allKinds = []string{KindPermission, KindQuestion, KindAuth, KindError}
)

func newNotifierRig(t *testing.T) *notifierRig {
	r := &notifierRig{testRig: newRig(t), connected: map[string]bool{}}
	s, err := OpenStore(filepath.Join(t.TempDir(), "push"), r.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.store = s
	r.n = NewNotifier(NotifierConfig{
		Sender: r.client, Store: s, Hab: "laptop", Now: r.clock.Now, Sleep: func(time.Duration) {},
		Where: func(pane, session string) Where {
			return Where{Session: "s1", SessionName: "fix-login", Project: "web-app", Harness: "claude"}
		},
		Connected: func(c string) bool { r.mu.Lock(); defer r.mu.Unlock(); return r.connected[c] },
		Logf: func(f string, a ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, f)
			r.mu.Unlock()
		},
	})
	t.Cleanup(r.n.Close)
	return r
}

func (r *notifierRig) connect(c string, on bool) { r.mu.Lock(); r.connected[c] = on; r.mu.Unlock() }

func blocked(id, cmd string) api.AgentState {
	return api.AgentState{Pane: "p1", Harness: "claude", State: api.AgentStatusBlocked, Kind: api.BlockedKindPermission,
		Request: &api.PermissionRequest{ID: id, Tool: "Bash", Command: cmd, ExpiresAt: "2026-11-01T12:05:00Z"}, At: "2026-11-01T12:00:00Z"}
}

func state(s api.AgentStatus) api.AgentState {
	return api.AgentState{Pane: "p1", Harness: "claude", State: s, At: "2026-11-01T12:00:01Z"}
}

// alerts returns the alert requests to token, in order.
func alerts(reqs []fakeReq, token string) []fakeReq {
	var out []fakeReq
	for _, q := range reqs {
		if q.Token == token && q.Header.Get("apns-push-type") == "alert" {
			out = append(out, q)
		}
	}
	return out
}

// asks returns the permission banners to token: withdrawals left out.
func asks(reqs []fakeReq, token string) []fakeReq {
	var out []fakeReq
	for _, q := range alerts(reqs, token) {
		if q.Body["flightdeck"].(map[string]any)["kind"] == KindPermission {
			out = append(out, q)
		}
	}
	return out
}

func alertField(q fakeReq, f string) any {
	return q.Body["aps"].(map[string]any)["alert"].(map[string]any)[f]
}

// TestNotifierPermissionAndWithdraw: a permission request pushes a time-sensitive banner with
// Approve and Deny, collapsed by request id. An answer through approval.answer replaces it,
// same collapse id, with a quiet "Answered on <client>"; a TUI answer says the terminal.
func TestNotifierPermissionAndWithdraw(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.Register("dev-mac", mac, Development, allKinds, false)

	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	pa, ma := alerts(r.prod.requests(), phone), alerts(r.dev.requests(), mac)
	if len(pa) != 1 || len(ma) != 1 {
		t.Fatalf("banners: phone %d, mac %d", len(pa), len(ma))
	}
	b := pa[0]
	if b.Header.Get("apns-collapse-id") != "r-req-1" || b.Header.Get("apns-priority") != "10" || alertField(b, "title") != "Run make test?" {
		t.Fatalf("banner %v %v", b.Header, b.Body)
	}
	if aps := b.Body["aps"].(map[string]any); aps["category"] != CategoryApproval || aps["interruption-level"] != "time-sensitive" {
		t.Fatalf("banner aps %v", aps)
	}
	// The same request again (a later revision) raises nothing new.
	again := blocked("req-1", "make test")
	again.Revision = 2
	r.n.State(again)
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 1 {
		t.Fatalf("a repeated request pushed again: %d", n)
	}

	// Answered from the phone app: the banner is replaced on both devices.
	r.n.Answering("p1", "req-1", "phone", true)
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa, ma = alerts(r.prod.requests(), phone), alerts(r.dev.requests(), mac)
	if len(pa) != 2 || len(ma) != 2 {
		t.Fatalf("withdrawals: phone %d, mac %d", len(pa), len(ma))
	}
	w := pa[1]
	if w.Header.Get("apns-collapse-id") != "r-req-1" || w.Header.Get("apns-priority") != "5" {
		t.Errorf("withdrawal headers %v", w.Header)
	}
	if alertField(w, "title") != "Answered on phone" || alertField(w, "body") != "Allowed: make test" {
		t.Errorf("withdrawal %v", w.Body)
	}
	if aps := w.Body["aps"].(map[string]any); aps["interruption-level"] != "passive" || aps["category"] != nil {
		t.Errorf("withdrawal aps %v", aps)
	}

	// A second request answered in the terminal: no Answering record.
	r.n.State(blocked("req-2", "go vet ./..."))
	r.n.State(state(api.AgentStatusIdle)) // coalesced: answered before the push went out
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 2 {
		t.Fatalf("a request resolved before its push still pushed: %d", n)
	}
	r.n.State(blocked("req-3", "go vet ./..."))
	r.n.Sync()
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	if len(pa) != 4 || pa[3].Header.Get("apns-collapse-id") != "r-req-3" || alertField(pa[3], "title") != "Answered in the terminal" {
		t.Fatalf("terminal withdrawal: %d alerts, last %v", len(pa), pa[len(pa)-1].Body)
	}
}

// TestNotifierPresence: a session a connected client focused within 60 s gets no banner; a
// device whose client was in the foreground within 180 s gets none either (the app shows its
// own notice), while other devices still do; stale or disconnected presence pushes.
func TestNotifierPresence(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.Register("dev-mac", mac, Production, allKinds, false)
	r.n.Bind("client-mac", "dev-mac")
	r.connect("client-mac", true)

	// Focused on the session: nothing to anyone.
	r.n.Focus("client-mac", "s1", true)
	r.n.State(blocked("r1", "make"))
	r.n.Sync()
	if n := len(r.prod.requests()); n != 0 {
		t.Fatalf("watched session pushed %d", n)
	}
	// Focus 61 s old: the hold ends; the mac was in the foreground within 180 s, so only the
	// phone gets the banner.
	r.clock.Add(61 * time.Second)
	r.n.State(blocked("r2", "make"))
	r.n.Sync()
	if p, m := len(asks(r.prod.requests(), phone)), len(asks(r.prod.requests(), mac)); p != 1 || m != 0 {
		t.Fatalf("in-app tier: phone %d, mac %d", p, m)
	}
	// 181 s since the mac's report: both push.
	r.clock.Add(120 * time.Second)
	r.n.State(blocked("r3", "make"))
	r.n.Sync()
	if p, m := len(asks(r.prod.requests(), phone)), len(asks(r.prod.requests(), mac)); p != 2 || m != 1 {
		t.Fatalf("after the active window: phone %d, mac %d", p, m)
	}
	// A fresh focus from a client whose connection dropped holds nothing.
	r.n.Focus("client-mac", "s1", true)
	r.connect("client-mac", false)
	r.n.State(blocked("r4", "make"))
	r.n.Sync()
	if p, m := len(asks(r.prod.requests(), phone)), len(asks(r.prod.requests(), mac)); p != 3 || m != 2 {
		t.Fatalf("disconnected presence: phone %d, mac %d", p, m)
	}
	// The app left the foreground: active false ends both tiers at once.
	r.connect("client-mac", true)
	r.n.Focus("client-mac", "s1", false)
	r.n.State(blocked("r5", "make"))
	r.n.Sync()
	if p, m := len(asks(r.prod.requests(), phone)), len(asks(r.prod.requests(), mac)); p != 4 || m != 3 {
		t.Fatalf("after active false: phone %d, mac %d", p, m)
	}
}

// TestNotifierKinds: each device gets only the kinds it registered; done is opt-in and
// passive; error and auth are active.
func TestNotifierKinds(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, []string{KindPermission, KindError, KindAuth}, false)
	r.store.Register("dev-mac", mac, Production, []string{KindDone}, false)
	r.n.State(state(api.AgentStatusWorking))
	r.n.State(state(api.AgentStatusDone))
	r.n.Sync()
	r.n.State(state(api.AgentStatusError))
	r.n.Sync()
	auth := state(api.AgentStatusBlocked)
	auth.Kind = api.BlockedKindAuth
	r.n.State(auth)
	r.n.Sync()
	pa, ma := alerts(r.prod.requests(), phone), alerts(r.prod.requests(), mac)
	if len(pa) != 2 || len(ma) != 1 {
		t.Fatalf("phone %d, mac %d", len(pa), len(ma))
	}
	if alertField(pa[0], "title") != "Turn stopped: web-app" || pa[0].Body["aps"].(map[string]any)["interruption-level"] != "active" {
		t.Errorf("error banner %v", pa[0].Body)
	}
	if alertField(pa[1], "title") != "Sign-in needed" {
		t.Errorf("auth banner %v", pa[1].Body)
	}
	if alertField(ma[0], "title") != "Done: fix-login" || ma[0].Body["aps"].(map[string]any)["interruption-level"] != "passive" || ma[0].Header.Get("apns-priority") != "5" {
		t.Errorf("done banner %v", ma[0].Body)
	}
}

// TestNotifierBadToken: APNs calling a token unregistered drops it from the store, once;
// a refused key marks push broken until a send works.
func TestNotifierBadToken(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.Register("dev-mac", mac, Production, allKinds, false)
	r.prod.reply(phone, 410, "Unregistered")
	r.n.State(blocked("r1", "make"))
	r.n.Sync()
	devs := r.store.Devices()
	if len(devs) != 1 || devs[0].ID != "dev-mac" {
		t.Fatalf("after 410: %+v", devs)
	}
	r.n.State(blocked("r2", "make"))
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 1 {
		t.Errorf("a dropped token was used again: %d", n)
	}
	r.prod.reply(mac, 403, "TopicDisallowed")
	r.n.State(blocked("r3", "make"))
	r.n.Sync()
	if r.n.Broken() != "TopicDisallowed" {
		t.Errorf("broken %q", r.n.Broken())
	}
	r.prod.reply(mac, 200, "")
	r.n.State(blocked("r4", "make"))
	r.n.Sync()
	if r.n.Broken() != "" {
		t.Errorf("still broken after a good send: %q", r.n.Broken())
	}
}

// TestNotifierLiveActivity: the tracked session's activity gets Allow and Deny while a
// permission is open, loses them once it resolves, throttles repeated working updates, and
// ends on done; its token is then dropped.
func TestNotifierLiveActivity(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, nil, false)
	r.store.RegisterActivity("dev-phone", "s1", phoneLA)
	acts := func() []fakeReq {
		var out []fakeReq
		for _, q := range r.prod.requests() {
			if q.Token == phoneLA {
				out = append(out, q)
			}
		}
		return out
	}
	cs := func(q fakeReq) map[string]any {
		return q.Body["aps"].(map[string]any)["content-state"].(map[string]any)
	}

	r.n.State(blocked("r1", "make test"))
	r.n.Sync()
	a := acts()
	if len(a) != 1 || a[0].Header.Get("apns-push-type") != "liveactivity" || a[0].Header.Get("apns-topic") != testTopic+".push-type.liveactivity" {
		t.Fatalf("activity update: %d", len(a))
	}
	if c := cs(a[0]); c["buttons"] != true || c["request"] != "r1" || c["title"] != "Run make test?" {
		t.Errorf("blocked content %v", c)
	}
	if n := len(alerts(r.prod.requests(), phone)); n != 0 {
		t.Errorf("a device with no kinds got %d banners", n)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	a = acts()
	if len(a) != 2 || cs(a[1])["buttons"] != false || cs(a[1])["state"] != "working" || a[1].Header.Get("apns-priority") != "5" {
		t.Fatalf("after the answer: %d, %v", len(a), cs(a[len(a)-1]))
	}
	// Working again within 30 s: throttled. After 30 s: one more.
	w := state(api.AgentStatusWorking)
	w.Revision = 5
	r.n.State(w)
	r.n.Sync()
	r.clock.Add(31 * time.Second)
	w.Revision = 6
	r.n.State(w)
	r.n.Sync()
	if n := len(acts()); n != 3 {
		t.Fatalf("working throttle: %d updates, want 3", n)
	}
	r.n.State(state(api.AgentStatusDone))
	r.n.Sync()
	a = acts()
	last := a[len(a)-1].Body["aps"].(map[string]any)
	if last["event"] != "end" || last["dismissal-date"] != float64(r.clock.Now().Add(activityLinger).Unix()) {
		t.Errorf("finish %v", last)
	}
	if d := r.store.Devices(); len(d) != 1 || d[0].Activity != nil {
		t.Errorf("activity token kept after end: %+v", d)
	}
}

// TestNotifierRaise: a notice goes to every device with a token, under the presence tiers.
func TestNotifierRaise(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, []string{KindDone}, false)
	r.store.Register("dev-mac", mac, Production, nil, false)
	r.n.Bind("client-mac", "dev-mac")
	r.connect("client-mac", true)
	r.n.Focus("client-mac", "s9", true)
	if n := r.n.Raise(Notice{ID: "n1", Title: "Backup finished", Body: "All snapshots verified.", Level: LevelPassive}); n != 1 {
		t.Fatalf("raised to %d devices, want 1 (the mac is in the foreground)", n)
	}
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 1 || alertField(pa[0], "title") != "Backup finished" || pa[0].Header.Get("apns-collapse-id") != "n-n1" || alertField(pa[0], "subtitle") != "laptop" {
		t.Fatalf("notice: %v", pa)
	}
	if fd := pa[0].Body["flightdeck"].(map[string]any); fd["kind"] != KindNotice || fd["notice"] != "n1" {
		t.Errorf("notice meta %v", fd)
	}
	if n := r.n.Raise(Notice{ID: "n2", Title: "Deploy done", Session: "s9"}); n != 0 {
		t.Errorf("a notice about a watched session went to %d devices", n)
	}
}

// TestNotifierAnsweredEarly: Claude Code keeps a request open until the tool has run. An answer
// that reached the harness withdraws the banner and drops the Live Activity buttons at once;
// the same blocked state arriving again raises nothing.
func TestNotifierAnsweredEarly(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.RegisterActivity("dev-phone", "s1", phoneLA)
	r.n.State(blocked("r1", "make test"))
	r.n.Sync()
	r.n.Answering("p1", "r1", "desk", false)
	r.n.Answered("p1", "r1", false)
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 2 || alertField(pa[1], "title") != "Answered on desk" || alertField(pa[1], "body") != "Denied: make test" {
		t.Fatalf("early withdrawal: %d alerts", len(pa))
	}
	var la []fakeReq
	for _, q := range r.prod.requests() {
		if q.Token == phoneLA {
			la = append(la, q)
		}
	}
	if len(la) != 2 {
		t.Fatalf("activity updates %d, want 2", len(la))
	}
	if cs := la[1].Body["aps"].(map[string]any)["content-state"].(map[string]any); cs["buttons"] != false || cs["state"] != "idle" {
		t.Errorf("after the answer: %v", cs)
	}
	again := blocked("r1", "make test")
	again.Revision = 9
	r.n.State(again)
	r.n.Sync()
	if n := len(r.prod.requests()); n != 4 {
		t.Errorf("the settled request raised more pushes: %d requests, want 4", n)
	}
}

// TestNotifierReplayNoBanner: after a gff-hab restart the link replays each pane's last OSC
// 7501 report with source screen. A pane first seen that way raises no banner, but its Live
// Activity is updated; the next live change pushes as usual.
func TestNotifierReplayNoBanner(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, append(allKinds, KindDone), false)
	r.store.RegisterActivity("dev-phone", "s1", phoneLA)
	replay := state(api.AgentStatusError)
	replay.Source = api.StateSourceScreen
	r.n.State(replay)
	other := state(api.AgentStatusBlocked)
	other.Pane, other.Kind, other.Source = "p2", api.BlockedKindPermission, api.StateSourceScreen
	r.n.State(other)
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 0 {
		t.Fatalf("replayed state pushed %d banners", n)
	}
	la := 0
	for _, q := range r.prod.requests() {
		if q.Token == phoneLA {
			la++
		}
	}
	if la == 0 {
		t.Fatal("replayed state did not update the Live Activity")
	}
	live := state(api.AgentStatusDone)
	live.Source = api.StateSourceOsc
	r.n.State(live)
	r.n.Sync()
	if n := len(alerts(r.prod.requests(), phone)); n != 1 {
		t.Fatalf("a live change after the replay pushed %d banners, want 1", n)
	}
}

// TestNotifierExpiration: APNs stores an alert for an offline device until apns-expiration. A
// permission banner keeps until its request expires and its withdrawal uses the same value;
// other alerts and notices keep one hour; Live Activity updates are not stored (0).
func TestNotifierExpiration(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, allKinds, false)
	r.store.RegisterActivity("dev-phone", "s1", phoneLA)
	exp := func(q fakeReq) string { return q.Header.Get("apns-expiration") }
	hour := strconv.FormatInt(r.clock.Now().Add(time.Hour).Unix(), 10)
	reqExp := strconv.FormatInt(time.Date(2026, 11, 1, 12, 5, 0, 0, time.UTC).Unix(), 10)

	r.n.State(blocked("r1", "make"))
	r.n.Sync()
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	r.n.State(state(api.AgentStatusError))
	r.n.Sync()
	r.n.Raise(Notice{ID: "n1", Title: "Backup finished"})
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 4 {
		t.Fatalf("alerts %d, want 4", len(pa))
	}
	if exp(pa[0]) != reqExp || exp(pa[1]) != reqExp {
		t.Errorf("permission banner and withdrawal: %s, %s, want %s", exp(pa[0]), exp(pa[1]), reqExp)
	}
	if exp(pa[2]) != hour || exp(pa[3]) != hour {
		t.Errorf("error and notice: %s, %s, want %s", exp(pa[2]), exp(pa[3]), hour)
	}
	for _, q := range r.prod.requests() {
		if q.Token == phoneLA && exp(q) != "0" {
			t.Errorf("Live Activity update expiration %s, want 0", exp(q))
		}
	}
}
