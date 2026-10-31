package control

import (
	"bytes"
	"context"
	"net"
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
