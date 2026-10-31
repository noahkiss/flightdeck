package control

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestBacklogCountsInFlight: bytes the writer is stuck writing count against the limit.
func TestBacklogCountsInFlight(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	cfg.BacklogLimit = 10 << 10
	cfg.CloseGrace = 200 * time.Millisecond
	srv := New(cfg)
	t.Cleanup(srv.Close)
	nc := pipeServe(t, srv) // net.Pipe: a write blocks until this end reads
	br := bufio.NewReader(nc)
	go func() {
		fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":1,"method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"script"}}}`)
		fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":2,"method":"events.subscribe","params":{"scope":{"kind":"host"}}}`)
	}()
	for range 2 {
		if _, err := br.ReadBytes('\n'); err != nil {
			t.Fatal(err)
		}
	}
	var c *conn
	srv.mu.Lock()
	for k := range srv.conns {
		c = k
	}
	srv.mu.Unlock()

	ev := func(n int) structure.Event {
		return structure.Event{Type: structure.ProjectUpdated, ID: "p", Revision: 1,
			Data: api.Project{ID: "p", Name: strings.Repeat("x", n), Sessions: []string{}}}
	}
	srv.Publish(ev(8 << 10)) // the writer takes it and blocks: nobody reads
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		inflight := c.inflight
		c.mu.Unlock()
		if inflight > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the writer never took the batch")
		}
		time.Sleep(time.Millisecond)
	}
	srv.Publish(ev(4 << 10)) // 8 KiB in flight + 4 KiB queued > 10 KiB
	c.mu.Lock()
	closing := c.closing
	c.mu.Unlock()
	if !closing {
		t.Fatal("in-flight bytes were not counted against the backlog limit")
	}
}
