package push

import (
	"strings"
	"testing"
)

// TestCapcomMessage: a Capcom message (capcom-service.md 7.6) goes to the device that
// registered the capcom kind, titled Capcom with no subtitle, the reply text cut to its own
// budget, the Reply category, collapsed by item id, with the hab, session and item in its
// metadata; an event turn's message is passive, any other active; none while a client
// focuses the service session; none to a device that did not register the kind.
func TestCapcomMessage(t *testing.T) {
	r := newNotifierRig(t)
	r.store.Register("dev-phone", phone, Production, []string{KindCapcom}, false)
	r.store.Register("dev-old", "old-token", Production, nil, false)
	long := strings.Repeat("word ", 60)
	r.n.Raise(CapcomMessage("i1", "svc", "Your table is at 8.", false))
	r.n.Raise(CapcomMessage("i2", "svc", long, true))
	r.n.Sync()
	ps := roundPushes(r.prod.requests(), phone)
	if len(ps) != 2 {
		t.Fatalf("%d pushes, want 2", len(ps))
	}
	p := ps[0]
	if p.title != "Capcom" || p.subtitle != "" || p.body != "Your table is at 8." || p.kind != KindCapcom ||
		p.category != CategoryCapcomReply || p.collapse != "capcom-i1" || p.session != "svc" || p.level != "active" {
		t.Fatalf("message %+v", p)
	}
	if n := len(strings.Fields(ps[1].body)); n > capcomBodyWords || len(ps[1].body) > capcomBodyChars+3 || ps[1].level != "passive" {
		t.Fatalf("event message %q (%d words) %s", ps[1].body, n, ps[1].level)
	}
	if got := len(alerts(r.prod.requests(), "old-token")); got != 0 {
		t.Fatalf("a device without the kind got %d", got)
	}
	var item string
	for _, q := range alerts(r.prod.requests(), phone) {
		if fd, ok := q.Body["flightdeck"].(map[string]any); ok {
			if s, _ := fd["item"].(string); s != "" {
				item = s
				break
			}
		}
	}
	if item != "i1" {
		t.Fatalf("item in the metadata %q", item)
	}
	// The Capcom view in focus on a client: no push.
	r.connect("client-mac", true)
	r.n.Focus("client-mac", "svc", true)
	if n := r.n.Raise(CapcomMessage("i3", "svc", "while watched", false)); n != 0 {
		t.Fatalf("pushed to %d while watched", n)
	}
}
