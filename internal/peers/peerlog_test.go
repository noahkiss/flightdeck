package peers

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/habconn"
)

// TestPeerStateLines: a peer that comes online and then goes away gets one line for each
// change, naming its device id and never its label; its link's retries add none
// (diagnostics.md 4.6, 10.1).
func TestPeerStateLines(t *testing.T) {
	d := shortDir(t)
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	var mu sync.Mutex
	var lines []string
	file := filepath.Join(d, "a.toml")
	write(t, file, "[[peers]]\nlabel = \"b\"\nendpoints = [\""+b.sock+"\"]\n")
	m, err := Open(Config{Path: file, Self: "SHA256:aaaaaaaaaaaa", Host: "a", Build: "test", Poll: 50 * time.Millisecond,
		Logf: func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() },
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
			c.PingInterval, c.DeadAfter = 200*time.Millisecond, 2*time.Second
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	count := func(s string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if l == s {
				n++
			}
		}
		return n
	}
	wait(t, "b online", func() bool { return count("peers: device "+b.id+" online") == 1 })
	b.stop()
	wait(t, "b offline", func() bool { return count("peers: device "+b.id+" offline (offline)") == 1 })
	// The link keeps retrying; none of its tries is a change.
	m.Retry()
	m.Retry()
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "peers: device ") {
			n++
		}
	}
	if n != 2 || strings.Contains(strings.Join(lines, "\n"), "peers: b ") {
		t.Fatalf("want one online and one offline line, got %q", lines)
	}
}
