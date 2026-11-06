package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// TestClientDeadAfter: a connection on which nothing arrives for the dead window ends with
// ErrDead (spec 3.5), so a stream the hab stopped feeding never sits silent forever. One that
// answers the client's pings stays open well past the window.
func TestClientDeadAfter(t *testing.T) {
	const window = 200 * time.Millisecond
	// serve reads the client's lines; with answer set it answers each request.
	serve := func(b net.Conn, answer bool) {
		br := bufio.NewReader(b)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			var m message
			if json.Unmarshal(line, &m) != nil || !answer {
				continue
			}
			out, _ := json.Marshal(message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{"seq":0}`)})
			if _, err := b.Write(append(out, '\n')); err != nil {
				return
			}
		}
	}

	t.Run("silent", func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		go serve(b, false)
		c := NewClient(a)
		defer c.Close()
		c.KeepAlive(window / 4)
		c.DeadAfter(window)
		start := time.Now()
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("a connection that delivers nothing stayed open")
		}
		if d := time.Since(start); d < window {
			t.Fatalf("ended after %v, before the %v window", d, window)
		}
		if err := c.Err(); !errors.Is(err, ErrDead) {
			t.Fatalf("err %v, want ErrDead", err)
		}
		// The notifications channel closes, so a reader such as `gff-hab events` ends.
		for range c.Notifications() {
		}
	})

	t.Run("answering", func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		go serve(b, true)
		c := NewClient(a)
		defer c.Close()
		c.KeepAlive(window / 4)
		c.DeadAfter(window)
		select {
		case <-c.Done():
			t.Fatalf("a connection that answers pings ended: %v", c.Err())
		case <-time.After(4 * window):
		}
	})
}
