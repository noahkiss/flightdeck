package control

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestClientWriteDeadline: a peer that never reads cannot hold a call forever.
func TestClientWriteDeadline(t *testing.T) {
	a, b := net.Pipe() // writes block until the other end reads
	defer b.Close()
	c := NewClient(a)
	cx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Call(cx, "hab.ping", nil, nil); err == nil {
		t.Fatal("call to a peer that never reads succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("call took %v", d)
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("connection stayed open after a stalled write")
	}
}

// TestClientLineCap: a server line longer than MaxMessage ends the connection with an error.
func TestClientLineCap(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a)
	go func() {
		chunk := bytes.Repeat([]byte{'x'}, 1<<20)
		if _, err := b.Write([]byte("{")); err != nil {
			return
		}
		for range MaxMessage>>20 + 2 {
			if _, err := b.Write(chunk); err != nil {
				return
			}
		}
	}()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("client kept reading an endless line")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("err %v", err)
	}
}

// pumps counts the goroutines running Client.pump.
func pumps() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "control.(*Client).pump(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestClientCloseStopsPump: a client closed while notifications wait unread leaves no
// delivery goroutine behind, across many dropped connections.
func TestClientCloseStopsPump(t *testing.T) {
	before := pumps()
	note := []byte(`{"jsonrpc":"2.0","method":"heartbeat","params":{"seq":0}}` + "\n")
	for range 50 {
		a, b := net.Pipe()
		c := NewClient(a)
		// net.Pipe has no buffer: the second write returns only after the client read the
		// first line whole, so at least one notification waits in the client unread.
		for range 2 {
			if _, err := b.Write(note); err != nil {
				t.Fatal(err)
			}
		}
		c.Close()
		b.Close()
		<-c.Done()
	}
	deadline := time.Now().Add(10 * time.Second)
	for pumps() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d delivery goroutines left after Close", pumps()-before)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Notifications still close after Close, so a range over them ends.
	a, b := net.Pipe()
	defer b.Close()
	c := NewClient(a)
	c.Close()
	for range c.Notifications() {
	}
}
