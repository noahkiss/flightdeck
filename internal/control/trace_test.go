package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// TestRequestTrace is diagnostics test 9 at the integration layer (5.2): a valid trace comes
// back on the response and on the failed-request line; a request without one gets a new h trace;
// a malformed one is replaced by a new h trace, and the line notes trace_replaced. A notification
// carries none.
func TestRequestTrace(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	h := startHab(t, "", socketPath(t), func(c *Config) {
		c.Logf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	})
	nc, err := net.Dial("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(nc)
	call := func(line string) message {
		t.Helper()
		fmt.Fprintln(nc, line)
		for {
			b, err := br.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var m message
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			if m.Method != "" {
				if m.Trace != "" {
					t.Errorf("notification %s carries trace %q", m.Method, m.Trace)
				}
				continue
			}
			return m
		}
	}
	const valid = "g0123456789A"
	if m := call(`{"jsonrpc":"2.0","id":1,"trace":"` + valid + `","method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"script"}}}`); m.Trace != valid || m.Error != nil {
		t.Fatalf("hello: trace %q, error %v; want %s", m.Trace, m.Error, valid)
	}
	if m := call(`{"jsonrpc":"2.0","id":2,"trace":"` + valid + `","method":"session.close","params":{"session":"nope"}}`); m.Trace != valid || m.Error == nil {
		t.Fatalf("valid trace: answer trace %q, error %v", m.Trace, m.Error)
	}
	none := call(`{"jsonrpc":"2.0","id":3,"method":"session.close","params":{"session":"nope"}}`)
	if !trace.Valid(string(none.Trace)) || none.Trace[0] != trace.Hab {
		t.Fatalf("no trace: answer trace %q, want a new h trace", none.Trace)
	}
	const bad = "../etc passwd"
	replaced := call(`{"jsonrpc":"2.0","id":4,"trace":"` + bad + `","method":"session.close","params":{"session":"nope"}}`)
	if !trace.Valid(string(replaced.Trace)) || replaced.Trace[0] != trace.Hab || replaced.Trace == none.Trace {
		t.Fatalf("malformed trace: answer trace %q, want a new h trace", replaced.Trace)
	}
	notString := call(`{"jsonrpc":"2.0","id":5,"trace":{"a":12},"method":"session.close","params":{"session":"nope"}}`)
	if !trace.Valid(string(notString.Trace)) || notString.Trace[0] != trace.Hab || notString.Error == nil || notString.Error.Data.Kind != "unavailable" && notString.Error.Data.Kind != "not_found" {
		t.Fatalf("a trace that is not a string: trace %q, error %+v; want a new h trace and the method's own error", notString.Trace, notString.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	var failed []string
	for _, l := range lines {
		if strings.HasPrefix(l, "request failed: method session.close") {
			failed = append(failed, l)
		}
		if strings.Contains(l, "etc passwd") {
			t.Errorf("a log line holds the malformed trace: %q", l)
		}
	}
	if len(failed) != 4 {
		t.Fatalf("want 4 failed session.close lines, got %q", failed)
	}
	for i, want := range []string{", trace " + valid, ", trace " + string(none.Trace), ", trace " + string(replaced.Trace) + ", trace_replaced",
		", trace " + string(notString.Trace) + ", trace_replaced"} {
		if !strings.HasSuffix(failed[i], want) {
			t.Errorf("line %q: want it to end %q", failed[i], want)
		}
	}
}

// TestClientSendsTrace: Client.Call sends the context's trace; without one it sends a new trace
// of its origin, and with no origin it sends none (5.2, 5.4).
func TestClientSendsTrace(t *testing.T) {
	got := make(chan string, 4)
	h := startHab(t, "", socketPath(t), func(c *Config) {
		c.Logf = func(f string, a ...any) {
			if l := fmt.Sprintf(f, a...); strings.HasPrefix(l, "request failed:") {
				got <- l[strings.LastIndex(l, "trace ")+len("trace "):]
			}
		}
	})
	c := dial(t, h.path)
	hello(t, c, "")
	fail := func(ctx context.Context) string {
		t.Helper()
		err := c.Call(ctx, "session.close", map[string]string{"session": "nope"}, nil)
		var ce *Error
		if !errors.As(err, &ce) {
			t.Fatalf("session.close of a session that does not exist: %v", err)
		}
		select {
		case s := <-got:
			// The failed call's error carries the trace the hab logged, and prints it.
			if ce.Trace != s || !strings.HasSuffix(ce.Error(), " (trace "+s+")") {
				t.Errorf("error %q, trace %q; the hab logged %s", ce.Error(), ce.Trace, s)
			}
			return s
		case <-time.After(10 * time.Second):
			t.Fatal("no failed-request line")
			return ""
		}
	}
	const valid = "a0123456789A"
	if s := fail(trace.With(ctx(t), valid)); s != valid {
		t.Errorf("context trace: the hab logged %q, want %s", s, valid)
	}
	if s := fail(ctx(t)); !trace.Valid(s) || s[0] != trace.Hab {
		t.Errorf("no origin: the hab logged %q, want its own h trace", s)
	}
	c.SetTraceOrigin(trace.Gff)
	if s := fail(ctx(t)); !trace.Valid(s) || s[0] != trace.Gff {
		t.Errorf("origin g: the hab logged %q, want a g trace", s)
	}
}

// TestNotificationMintsNoTrace: a client notification is dropped before the trace check, so it
// costs no new id; each request is checked once (5.2, 11).
func TestNotificationMintsNoTrace(t *testing.T) {
	var calls atomic.Int32
	old := checkTrace
	checkTrace = func(s string) (string, bool) { calls.Add(1); return old(s) }
	t.Cleanup(func() { checkTrace = old })
	h := startHab(t, "", socketPath(t), nil)
	nc, err := net.Dial("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(nc)
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":1,"method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"script"}}}`)
	for range 3 {
		fmt.Fprintln(nc, `{"jsonrpc":"2.0","method":"hab.ping","trace":"not a trace"}`)
		fmt.Fprintln(nc, `{"jsonrpc":"2.0","method":"hab.ping"}`)
	}
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":2,"method":"hab.ping","params":{}}`)
	for answered := 0; answered < 2; {
		b, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m message
		if json.Unmarshal(b, &m) == nil && len(m.ID) > 0 {
			answered++
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the trace check ran %d times for 2 requests and 6 notifications", n)
	}
}
