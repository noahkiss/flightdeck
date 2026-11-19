package link

import (
	"testing"
	"time"
)

// Ping returns once the event loop ran it, waits while the loop is busy, and returns once the
// link closes (gff-hab's stall watchdog, docs/specs/diagnostics.md 4.7).
func TestPing(t *testing.T) {
	l := openLink(t, Config{HolderDir: shortDir(t)})
	l.Ping() // an idle loop answers

	release := make(chan struct{})
	busy := make(chan struct{})
	go l.run(func() { close(busy); <-release })
	<-busy
	done := make(chan struct{})
	go func() { l.Ping(); close(done) }()
	select {
	case <-done:
		t.Fatal("Ping returned while the loop was busy")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Ping did not return once the loop was free")
	}

	l.Close()
	closed := make(chan struct{})
	go func() { l.Ping(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Ping did not return after Close")
	}
}
