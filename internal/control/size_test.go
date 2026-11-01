package control

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// countingSize counts each client's control connections the way panesize does. When a client's
// connection is counted, it records whether another connection of that client was already
// finished: that connection's Disconnected could then run first and drop the count to zero.
type countingSize struct {
	srv atomic.Pointer[Server]

	mu         sync.Mutex
	conns      map[string]int
	zero       map[string]int // times a client's count fell to zero
	lateCounts int            // Connected calls made after the client's old connection was finished
}

func (f *countingSize) Connected(client string) {
	if s := f.srv.Load(); s != nil {
		s.mu.Lock()
		for c := range s.conns {
			c.mu.Lock()
			late := c.clientID == client && c.closing
			c.mu.Unlock()
			if late {
				f.mu.Lock()
				f.lateCounts++
				f.mu.Unlock()
			}
		}
		s.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[client]++
}

func (f *countingSize) Disconnected(client string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[client]--
	if f.conns[client] == 0 {
		f.zero[client]++
	}
}

func (f *countingSize) Typed(client, pane string)                             {}
func (f *countingSize) Created(client, pane string)                           {}
func (f *countingSize) Take(client, pane string) (string, error)              { return client, nil }
func (f *countingSize) Request(client, pane string) (string, error)           { return client, nil }
func (f *countingSize) Disown(client, pane string) (string, error)            { return "", nil }
func (f *countingSize) Redraw(client, pane string, mode api.RedrawMode) error { return nil }
func (f *countingSize) SetDesired(client, pane string, cols, rows int) (bool, error) {
	return true, nil
}

// TestReconnectCountsNewConnFirst checks that a reconnect whose token names a connection that
// is still open counts the new connection before it closes the old one (spec 6.5, 6.8). Were
// it the other way round, the old connection's Disconnected could run first: the client would
// leave ownership and get it back, and in the gap another client's size would apply.
//
// Which goroutine wins that race is up to the scheduler, so the test does not wait for it.
// countingSize looks at the order directly: when the new connection is counted, the old one
// must not be finished yet.
func TestReconnectCountsNewConnFirst(t *testing.T) {
	size := &countingSize{conns: map[string]int{}, zero: map[string]int{}}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Size = size })
	size.srv.Store(h.srv)

	c1 := dial(t, h.path)
	w1 := hello(t, c1, "")
	c2 := dial(t, h.path)
	w2 := hello(t, c2, w1.ReconnectToken) // c1 is still open: it is presumed dead and closed
	if w2.ClientID != w1.ClientID {
		t.Fatalf("reconnect: client %s -> %s", w1.ClientID, w2.ClientID)
	}
	waitConns(t, h.srv, 1)

	size.mu.Lock()
	defer size.mu.Unlock()
	if size.lateCounts != 0 {
		t.Fatalf("the new connection was counted after the old one was finished (%d times)", size.lateCounts)
	}
	if n := size.zero[w1.ClientID]; n != 0 {
		t.Fatalf("the client's connection count fell to zero %d times during a reconnect", n)
	}
	if n := size.conns[w1.ClientID]; n != 1 {
		t.Fatalf("connections counted: %d, want 1", n)
	}
}
