package link

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestStructureNotices checks the structure store's view of the link: a spawned pane is added
// and listed, and its exit is reported with the exit code.
func TestStructureNotices(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "st", 0)
	l, err := Open(Config{HolderDir: hdir, StateDir: shortDir(t), Build: "test", Parser: testParser(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var mu sync.Mutex
	var got []structure.Notice
	cancel := l.Structure().Subscribe(func(n structure.Notice) {
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	})
	defer cancel()
	ctx, stop := context.WithTimeout(context.Background(), waitLong)
	defer stop()
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(4)
	if _, err := l.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", "echo hi; exit 3"}, Env: testEnv}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "added and exited notices", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	})
	mu.Lock()
	added, exited := got[0], got[1]
	mu.Unlock()
	if added.Kind != structure.PaneAdded || added.Pane.ID != u.String() || added.Pane.HolderID != "st" {
		t.Errorf("first notice %+v", added)
	}
	if exited.Kind != structure.PaneExited || exited.Exit == nil || exited.Exit.Code == nil || *exited.Exit.Code != 3 {
		t.Errorf("second notice %+v", exited)
	}
	panes, err := l.Structure().Panes()
	if err != nil || len(panes) != 1 || panes[0].ID != u.String() || panes[0].Exit == nil {
		t.Errorf("Panes() = %+v, %v", panes, err)
	}
}

// TestStructureComplete: the structure's view of the link is complete only while the link has
// reached every holder in the registry. A registered holder it cannot reach (here: a live pid
// whose socket does not exist) makes it partial, so the store keeps a session whose pane may
// run there.
func TestStructureComplete(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	if !structure.Complete(l.Structure()) {
		t.Fatal("partial with every holder reached")
	}
	slow := holderreg.Entry{Instance: "slow", Build: "test", Protocol: holderreg.Protocol{Min: 1, Max: 1},
		Pid: os.Getpid(), Socket: filepath.Join(hdir, "slow.sock"), Started: time.Now()}
	if err := holderreg.Write(hdir, slow); err != nil {
		t.Fatal(err)
	}
	if structure.Complete(l.Structure()) {
		t.Fatal("complete with a registered holder not reached")
	}
	if err := os.Remove(holderreg.Path(hdir, "slow")); err != nil {
		t.Fatal(err)
	}
	if !structure.Complete(l.Structure()) {
		t.Fatal("partial after the unreached holder left the registry")
	}
}

// TestStructureCompleteWaitsForAttach: the client connects to a holder before the link attaches
// its panes, and Panes lists only attached panes. The list counts as complete only once the
// holder is attached, so a pane on a holder connected a moment ago is never taken as missing.
func TestStructureCompleteWaitsForAttach(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	held, release := make(chan struct{}), make(chan struct{})
	attachedSeam = func(instance string) {
		if instance == "two" {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { attachedSeam = nil }) // runs after the link closes
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "holder one attached", func() bool { return structure.Complete(l.Structure()) })
	startHolder(t, hdir, "two", 0)
	select {
	case <-held:
	case <-time.After(waitLong):
		t.Fatal("holder two was never attached")
	}
	connected := false
	for _, h := range l.client.Holders() {
		connected = connected || h.Instance() == "two"
	}
	if !connected {
		t.Fatal("holder two is not connected")
	}
	if structure.Complete(l.Structure()) {
		t.Error("complete while holder two is connected but its panes are not attached")
	}
	close(release)
	waitFor(t, "holder two attached", func() bool { return structure.Complete(l.Structure()) })
}
