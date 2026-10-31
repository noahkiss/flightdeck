package link

// In-process tests of pane bookkeeping: a late end event from an old holder connection, pruning
// of panes and checkpoints no holder lists, and checkpoints or panes left by another holder.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/vt"
)

func openLink(t *testing.T, cfg Config) *Link {
	t.Helper()
	cfg.Build, cfg.Parser, cfg.Logf = "test", testParser(), t.Logf
	if cfg.StateDir == "" {
		cfg.StateDir = shortDir(t)
	}
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func spawn(t *testing.T, l *Link, u hp.UUID, script string) PaneStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	st, err := l.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", script}, Env: testEnv})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func screenHas(t *testing.T, l *Link, u hp.UUID, s string) bool {
	t.Helper()
	sc, err := l.Screen(u)
	return err == nil && strings.Contains(sc.Text, s)
}

func paneStatus(l *Link, u hp.UUID) (PaneStatus, bool) {
	for _, st := range l.Panes() {
		if st.UUID == u {
			return st, true
		}
	}
	return PaneStatus{}, false
}

// TestLateEndEvent: a pane moves to a new connection to its holder before the old connection's
// end event is handled. That late event must not mark the pane lost.
func TestLateEndEvent(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "race", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: time.Hour})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(20)
	spawn(t, l, u, "echo hi; exec sleep 1000")
	var gone atomic.Int32
	defer l.Watch(func(n Notice) {
		if n.Kind == PaneGone && n.Pane.UUID == u {
			gone.Add(1)
		}
	})()
	old := l.client.Current()

	// On the event loop, so the old connection's end event waits behind this task: a second
	// client connects (the holder drops the old connection) and the pane attaches to it.
	c2 := holderclient.New(holderclient.Config{Dir: hdir, Build: "test", Logf: t.Logf})
	defer c2.Close()
	var attached bool
	l.run(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLong)
		defer cancel()
		if err := c2.Scan(ctx); err != nil || len(c2.Holders()) != 1 {
			t.Errorf("second connection: %v", err)
			return
		}
		h2 := c2.Holders()[0]
		for _, pi := range h2.Welcome().Panes {
			if pi.UUID == u {
				l.mu.Lock()
				l.holders[h2] = true
				l.mu.Unlock()
				l.attach(h2, pi)
				attached = true
			}
		}
	})
	if !attached {
		t.Fatal("the second connection does not list the pane")
	}
	waitFor(t, "the old connection's end event", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return old.Closed() && !l.holders[old]
	})
	l.run(func() {}) // the end event's handling has finished
	if n := gone.Load(); n != 0 {
		t.Errorf("%d gone notices for a pane that moved to a new connection", n)
	}
	if st, _ := paneStatus(l, u); st.Lost || st.Gone {
		t.Errorf("pane after the late end event: lost %v, gone %v", st.Lost, st.Gone)
	}
}

// TestPrune: after a scan that reached every holder, a lost pane is dropped and its parser
// freed, and the checkpoints of panes no holder lists are removed. A live pane and its
// checkpoint stay.
func TestPrune(t *testing.T) {
	hdir, state := shortDir(t), shortDir(t)
	stale := uuidN(30) // a checkpoint left by a pane that no longer exists
	if err := writeCheckpoint(state, stale, &checkpoint{Version: checkpointVersion, Pane: stale.String(),
		Holder: "gone", Encoding: vt.EncodingVT, Cols: 80, Rows: 24, Snapshot: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	doomed := startHolder(t, hdir, "a", 0)
	l := openLink(t, Config{HolderDir: hdir, StateDir: state, Rescan: 50 * time.Millisecond, KeepLost: time.Millisecond})
	if _, err := readCheckpoint(state, stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale checkpoint after open: %v", err)
	}
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(31)
	spawn(t, l, u, "echo a; exec sleep 1000")
	waitFor(t, "output on a", func() bool { return screenHas(t, l, u, "a") })

	startHolder(t, hdir, "b", 0)
	waitFor(t, "holder b current", func() bool {
		h := l.client.Current()
		return h != nil && h.Instance() == "b"
	})
	v := uuidN(32)
	spawn(t, l, v, "echo b; exec sleep 1000")
	waitFor(t, "output on b", func() bool { return screenHas(t, l, v, "b") })
	if err := l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	for _, x := range []hp.UUID{u, v} {
		if _, err := readCheckpoint(state, x); err != nil {
			t.Fatal(err)
		}
	}
	p := l.pane(u)

	doomed.Process.Signal(syscall.SIGKILL)
	waitFor(t, "the lost pane dropped", func() bool { _, ok := paneStatus(l, u); return !ok })
	if _, err := readCheckpoint(state, u); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkpoint of the dropped pane: %v", err)
	}
	p.mu.Lock()
	freed := p.term == nil
	p.mu.Unlock()
	if !freed {
		t.Error("the dropped pane's parser was not freed")
	}
	if st, ok := paneStatus(l, v); !ok || st.Lost {
		t.Errorf("live pane: listed %v, lost %v", ok, st.Lost)
	}
	if _, err := readCheckpoint(state, v); err != nil {
		t.Errorf("checkpoint of the live pane: %v", err)
	}
}

// TestStaleCheckpointIgnored: a checkpoint written for another holder's pane with the same uuid
// is not restored onto the pane.
func TestStaleCheckpointIgnored(t *testing.T) {
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "fresh", 0)
	u := uuidN(40)
	// Another gff-hab spawns the pane, then hands over.
	c := holderclient.New(holderclient.Config{Dir: hdir, Build: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := c.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", "echo fresh-output; exec sleep 1000"}, Env: testEnv}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := writeCheckpoint(state, u, &checkpoint{Version: checkpointVersion, Pane: u.String(),
		Holder: "someone-else", Encoding: vt.EncodingVT, Cols: 80, Rows: 24, Snapshot: []byte("STALE SCREEN")}); err != nil {
		t.Fatal(err)
	}
	l := openLink(t, Config{HolderDir: hdir, StateDir: state})
	if err := l.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pane's output", func() bool { return screenHas(t, l, u, "fresh-output") })
	if screenHas(t, l, u, "STALE") {
		t.Error("the other holder's checkpoint was restored onto the pane")
	}
	if st, _ := paneStatus(l, u); st.Restored {
		t.Error("pane reports a restore from a checkpoint it should ignore")
	}
}

// TestSpawnReplacesStalePane: spawning a uuid the link still lists as lost, on another holder,
// makes a new pane rather than returning the lost one as a retry.
func TestSpawnReplacesStalePane(t *testing.T) {
	hdir := shortDir(t)
	one := startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(50)
	spawn(t, l, u, "echo first; exec sleep 1000")
	waitFor(t, "first output", func() bool { return screenHas(t, l, u, "first") })
	one.Process.Signal(syscall.SIGKILL)
	waitFor(t, "the pane lost", func() bool { st, _ := paneStatus(l, u); return st.Lost })

	startHolder(t, hdir, "two", 0)
	waitFor(t, "holder two current", func() bool {
		h := l.client.Current()
		return h != nil && h.Instance() == "two"
	})
	st := spawn(t, l, u, "echo second; exec sleep 1000")
	if st.Lost || st.Instance != "two" {
		t.Fatalf("spawn returned lost %v on holder %q, want a live pane on two", st.Lost, st.Instance)
	}
	waitFor(t, "second output", func() bool { return screenHas(t, l, u, "second") })
	if screenHas(t, l, u, "first") {
		t.Error("the new pane shows the old pane's screen")
	}
}

// TestAddedBeforeExited: a child that exits at once is reported added before exited. Its exit
// frame can reach the link before the spawn answer, so it waits as an orphan frame; the add must
// still come first.
func TestAddedBeforeExited(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "ae", 0)
	l := openLink(t, Config{HolderDir: hdir})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	var mu sync.Mutex
	seen := map[hp.UUID][]NoticeKind{}
	stop := l.Watch(func(n Notice) {
		mu.Lock()
		seen[n.Pane.UUID] = append(seen[n.Pane.UUID], n.Kind)
		mu.Unlock()
	})
	defer stop()
	// Spawn from many goroutines at once: the more the event loop has to do, the more often an
	// exit frame overtakes the spawn's bookkeeping.
	const n = 64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), waitLong)
			defer cancel()
			if _, err := l.Spawn(ctx, hp.Spawn{UUID: uuidN(100 + i), Cols: 80, Rows: 24, Cwd: "/",
				Argv: []string{"/bin/sh", "-c", "exit 0"}, Env: testEnv}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	waitFor(t, "every exit", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for i := range n {
			if len(seen[uuidN(100+i)]) < 2 {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	for i := range n {
		if ks := seen[uuidN(100+i)]; ks[0] != PaneAdded || ks[1] != PaneExited {
			t.Errorf("pane %d: notices %v, want added then exited", i, ks)
		}
	}
}

// TestDrainReleasesEndedPanes: on a draining holder, the link releases each pane once it has
// ended, and not before; after the last release the holder exits and leaves the registry. Each
// released pane is gone, with its last output on its screen.
func TestDrainReleasesEndedPanes(t *testing.T) {
	hdir := shortDir(t)
	old := startHolder(t, hdir, "old", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	var mu sync.Mutex
	var gone []hp.UUID
	stop := l.Watch(func(n Notice) {
		if n.Kind == PaneGone {
			mu.Lock()
			gone = append(gone, n.Pane.UUID)
			mu.Unlock()
		}
	})
	defer stop()
	ended, live := uuidN(60), uuidN(61)
	goFile := filepath.Join(shortDir(t), "go")
	spawn(t, l, ended, "echo ended-early; exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if _, err := l.Spawn(ctx, hp.Spawn{UUID: live, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", `while [ ! -e "$GO" ]; do sleep 0.02; done; echo ended-late`},
		Env:  append(testEnv, "GO="+goFile)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first pane exited", func() bool { st, _ := paneStatus(l, ended); return st.Exited })
	for range 3 { // whole scans, each of which would release it: a current holder keeps ended panes
		select {
		case <-l.pruned:
		case <-time.After(waitLong):
			t.Fatal("no scan finished")
		}
	}
	if st, _ := paneStatus(l, ended); st.Gone {
		t.Fatal("an ended pane on the current holder was released")
	}

	startHolder(t, hdir, "new", 0)
	waitFor(t, "the ended pane released", func() bool { st, _ := paneStatus(l, ended); return st.Gone })
	if !screenHas(t, l, ended, "ended-early") {
		t.Error("the released pane lost its last output")
	}
	if st, _ := paneStatus(l, live); st.Gone || st.Lost {
		t.Fatalf("a running pane on the draining holder was released: %+v", st)
	}
	if err := syscall.Kill(old.Process.Pid, 0); err != nil {
		t.Fatal("the draining holder exited with a pane still running")
	}

	os.WriteFile(goFile, nil, 0o600)
	waitFor(t, "the second pane released", func() bool { st, _ := paneStatus(l, live); return st.Gone })
	if !screenHas(t, l, live, "ended-late") {
		t.Error("the released pane lost its last output")
	}
	waitFor(t, "the draining holder exits", func() bool {
		return errors.Is(syscall.Kill(old.Process.Pid, 0), syscall.ESRCH)
	})
	if _, err := os.Stat(holderreg.Path(hdir, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drained holder's registry file: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gone) < 2 {
		t.Errorf("gone notices for %v, want both panes", gone)
	}
}
