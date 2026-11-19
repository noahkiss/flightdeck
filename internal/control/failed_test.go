package control

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestFailedRequestLine: each error answer writes one line with the method, the error kind, the
// client kind and the duration; never the params or the error text. A method the API does not
// list is written as unknown (diagnostics.md 4.6).
func TestFailedRequestLine(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	h := startHab(t, "", socketPath(t), func(c *Config) {
		c.Logf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	})
	c := dial(t, h.path)
	hello(t, c, "")
	const secret = "canary-7d1f"
	var out any
	if err := c.Call(ctx(t), "session.close", map[string]string{"session": secret}, &out); err == nil {
		t.Fatal("session.close of a session that does not exist succeeded")
	}
	if err := c.Call(ctx(t), "no.such."+secret, nil, &out); err == nil {
		t.Fatal("an unknown method succeeded")
	}
	if err := c.Call(ctx(t), "hab.stats", nil, &out); err != nil && !strings.Contains(err.Error(), "unavailable") {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var failed []string
	for _, l := range lines {
		if strings.HasPrefix(l, "request failed:") {
			failed = append(failed, l)
		}
		if strings.Contains(l, secret) {
			t.Errorf("a log line holds the request's text: %q", l)
		}
	}
	if len(failed) != 3 {
		t.Fatalf("want 3 failed-request lines, got %q", failed)
	}
	if !strings.HasPrefix(failed[0], "request failed: method session.close, error unavailable, client script, ") {
		t.Errorf("line %q", failed[0])
	}
	if !strings.HasPrefix(failed[1], "request failed: method unknown, error ") {
		t.Errorf("line %q", failed[1])
	}
}
