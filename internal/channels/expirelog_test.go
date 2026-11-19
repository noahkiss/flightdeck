package channels

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOutboxExpiryLine: a message that waits 7 days in the outbox for a peer that never answers
// expires with one log line, which names the message and never its text (diagnostics.md 4.6).
func TestOutboxExpiryLine(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	var lines []string
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	b := newHab(t, "dev-b", "hostb", t.TempDir(), newLocal("bob"))
	a := newHabWith(t, "dev-a", "hosta", t.TempDir(), newLocal("alice"), func(c *Config) {
		c.Now = clock
		c.Logf = func(f string, args ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, args...)); mu.Unlock() }
	})
	var down atomic.Bool
	down.Store(true)
	link(a, b, "bee", &down)
	const text = "canary-3c9a"
	sent := send(t, a.s, "alice", "bee/bob", text)
	mu.Lock()
	now = now.Add(Keep)
	mu.Unlock()
	want := "channels: message " + sent.ID + " for dev-b expired unsent after 7 days"
	waitFor(t, "the expiry line", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(lines, "\n"), want)
	})
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, text) {
			t.Fatalf("a line holds the message text: %q", l)
		}
	}
}
