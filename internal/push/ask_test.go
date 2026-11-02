package push

import (
	"fmt"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// askRig is a notifier whose Capcom home and kept notices the test controls.
type askRig struct {
	*notifierRig
	mu    sync.Mutex
	home  string
	kept  []Notice
	nokey bool // Keep fails
}

func newAskRig(t *testing.T) *askRig {
	a := &askRig{}
	a.notifierRig = newNotifierRig(t, func(c *NotifierConfig) {
		c.Self = "dev-self"
		c.Capcom = func() string { a.mu.Lock(); defer a.mu.Unlock(); return a.home }
		c.Keep = func(n Notice) string {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.nokey {
				return ""
			}
			a.kept = append(a.kept, n)
			return fmt.Sprintf("k%d", len(a.kept))
		}
	})
	return a
}

func (a *askRig) setHome(h string) { a.mu.Lock(); a.home = h; a.mu.Unlock() }

func askCat(q fakeReq) any { return q.Body["aps"].(map[string]any)["category"] }

func askMeta(q fakeReq) map[string]any { return q.Body["flightdeck"].(map[string]any) }

// askContext returns the banner's capcom_context as kind, ref, host; ok false when absent.
func askContext(q fakeReq) (kind, ref, host any, ok bool) {
	c, ok := askMeta(q)["capcom_context"].(map[string]any)
	if !ok {
		return nil, nil, nil, false
	}
	return c["kind"], c["ref"], c["host"], true
}

// TestNotifierAsk: while some host runs Capcom, every banner carries the Ask action in its
// category and the context item capcom.open takes (capcom.md 8.5): a notice's own id, or the
// id of the notice an agent-state banner is kept as, with this hab's device id as the host
// when Capcom runs elsewhere. With no Capcom host, nothing changes.
func TestNotifierAsk(t *testing.T) {
	r := newAskRig(t)
	r.store.Register("dev-phone", phone, Production, append(allKinds, KindDone), false)
	r.store.Register("dev-mac", mac, Production, allKinds, true) // commands hidden

	// No Capcom host: the categories of before, no context, nothing kept.
	r.n.State(blocked("req-1", "make test"))
	r.n.Sync()
	r.n.Raise(Notice{ID: "n0", Title: "Backup finished"})
	r.n.Sync()
	pa := alerts(r.prod.requests(), phone)
	if len(pa) != 2 || askCat(pa[0]) != CategoryApproval || askCat(pa[1]) != nil {
		t.Fatalf("no Capcom: %d alerts, categories %v %v", len(pa), askCat(pa[0]), askCat(pa[len(pa)-1]))
	}
	for _, q := range pa {
		if _, _, _, ok := askContext(q); ok || askMeta(q)["capcom_home"] != nil {
			t.Errorf("no Capcom, but a context: %v", askMeta(q))
		}
	}
	if len(r.kept) != 0 {
		t.Errorf("kept %v with no Capcom host", r.kept)
	}
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()

	// Capcom runs on this hab: no host in the context.
	r.setHome("dev-self")
	r.n.State(blocked("req-2", "go test ./... -run TestLongName -count=1"))
	r.n.Sync()
	pa, ma := alerts(r.prod.requests(), phone), alerts(r.prod.requests(), mac)
	p, m := pa[len(pa)-1], ma[len(ma)-1]
	if askCat(p) != CategoryApprovalAsk || askCat(m) != CategoryDenyAsk {
		t.Fatalf("categories phone %v, mac %v", askCat(p), askCat(m))
	}
	kind, ref, host, ok := askContext(p)
	if !ok || kind != "notification" || ref != "k1" || host != nil || askMeta(p)["capcom_home"] != "dev-self" {
		t.Fatalf("phone context %v", askMeta(p))
	}
	if _, mref, _, _ := askContext(m); mref != "k1" {
		t.Errorf("mac context %v: one notice per banner, not per device", askMeta(m))
	}
	if len(r.kept) != 1 || r.kept[0].Title != "Run go test?" || r.kept[0].Body != "go test ./... -run TestLongName -count=1" ||
		r.kept[0].Session != "s1" || r.kept[0].Level != LevelTimeSensitive {
		t.Errorf("kept %+v", r.kept)
	}
	// The withdrawal is a record: no actions, no context.
	r.n.State(state(api.AgentStatusWorking))
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	if w := pa[len(pa)-1]; askCat(w) != nil || alertField(w, "title") != "Answered in the terminal" {
		t.Errorf("withdrawal %v", w.Body)
	} else if _, _, _, ok := askContext(w); ok {
		t.Errorf("withdrawal context %v", askMeta(w))
	}

	// Capcom runs on another host: the context names this hab by device id.
	r.setHome("dev-home")
	r.n.State(state(api.AgentStatusError))
	r.n.Sync()
	r.n.State(state(api.AgentStatusDone))
	r.n.Sync()
	r.n.Raise(Notice{ID: "n1", Title: "Deploy failed", Body: "exit 2"})
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	want := []struct {
		cat, ref string
	}{{CategoryOpenAsk, "k2"}, {CategoryAsk, "k3"}, {CategoryAsk, "n1"}}
	pa = pa[len(pa)-3:]
	for i, w := range want {
		_, ref, host, ok := askContext(pa[i])
		if askCat(pa[i]) != w.cat || !ok || ref != w.ref || host != "dev-self" || askMeta(pa[i])["capcom_home"] != "dev-home" {
			t.Errorf("banner %d: category %v, meta %v; want %s, ref %s", i, askCat(pa[i]), askMeta(pa[i]), w.cat, w.ref)
		}
	}
	if len(r.kept) != 3 {
		t.Errorf("a notice was kept again for notice.raise: %d kept", len(r.kept))
	}

	// A banner that could not be kept goes out without Ask.
	r.mu.Lock()
	r.nokey = true
	r.mu.Unlock()
	r.n.State(state(api.AgentStatusWorking))
	r.n.State(state(api.AgentStatusError))
	r.n.Sync()
	pa = alerts(r.prod.requests(), phone)
	if q := pa[len(pa)-1]; askCat(q) != CategoryOpen {
		t.Errorf("unkept banner category %v", askCat(q))
	} else if _, _, _, ok := askContext(q); ok {
		t.Errorf("unkept banner context %v", askMeta(q))
	}
}

func TestAskCategory(t *testing.T) {
	for in, want := range map[string]string{CategoryApproval: CategoryApprovalAsk, CategoryDeny: CategoryDenyAsk, CategoryOpen: CategoryOpenAsk, "": CategoryAsk, "question": "question"} {
		if got := AskCategory(in); got != want {
			t.Errorf("AskCategory(%q) = %q, want %q", in, got, want)
		}
	}
}
