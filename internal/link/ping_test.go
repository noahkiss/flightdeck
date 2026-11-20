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

// Attaching is true while the event loop attaches a holder's panes, the long task the stall
// watchdog excludes, and false once the attach ends (diagnostics.md 4.7).
func TestAttachingExcluded(t *testing.T) {
	hdir := shortDir(t)
	held, release := make(chan bool, 1), make(chan struct{})
	attachedSeam = func(string) {
		select {
		case held <- true:
			<-release
		default:
		}
	}
	t.Cleanup(func() { attachedSeam = nil })
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	startHolder(t, hdir, "one", 0) // after Open, whose first scan would wait on the seam
	select {
	case <-held:
	case <-time.After(waitLong):
		t.Fatal("the holder was never attached")
	}
	if !l.Attaching() {
		t.Error("Attaching is false during an attach")
	}
	close(release)
	waitFor(t, "the attach to end", func() bool { return !l.Attaching() })
}
