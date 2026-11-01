package holderclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// TestRegistryNotTrustedBlindly: registry files that point at another holder's socket are
// skipped, so one holder never gets two connections that take each other's place by hello
// takeover. Covered: a file naming another instance's socket path, a symlink to that socket, and
// a hard link to it whose holder's welcome then names another instance.
func TestRegistryNotTrustedBlindly(t *testing.T) {
	dir := regDir(t)
	inproc(t, dir, "B", "hreal", hp.V1())
	real, err := holderreg.Read(holderreg.Path(dir, "hreal"))
	if err != nil {
		t.Fatal(err)
	}
	craft := func(id, sock string) {
		t.Helper()
		e := real
		e.Instance, e.Socket = id, sock
		e.Started = real.Started.Add(-time.Hour) // listed, and dialled, before the real one
		if err := holderreg.Write(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	craft("hpath", real.Socket)
	craft("hsym", holderreg.SocketPath(dir, "hsym"))
	if err := os.Symlink(real.Socket, holderreg.SocketPath(dir, "hsym")); err != nil {
		t.Fatal(err)
	}
	craft("hlink", holderreg.SocketPath(dir, "hlink"))
	if err := os.Link(real.Socket, holderreg.SocketPath(dir, "hlink")); err != nil {
		t.Skipf("no hard link to a socket here: %v", err)
	}

	c := New(Config{Dir: dir, Build: "B"})
	ev := collect(t, c)
	for i := 0; i < 3; i++ {
		if err := c.Scan(ctx(t)); err != nil {
			t.Fatal(err)
		}
	}
	hs := c.Holders()
	if len(hs) != 1 || hs[0].Instance() != "hreal" {
		t.Fatalf("connected holders: %d", len(hs))
	}
	h := hs[0]
	if err := h.Ping(ctx(t)); err != nil {
		t.Fatalf("real holder after scans: %v", err)
	}
	// The real holder's one connection was never replaced.
	ev.mu.Lock()
	for _, e := range ev.all {
		if e.Frame == nil {
			t.Fatalf("a connection ended: %v", e.Err)
		}
	}
	ev.mu.Unlock()
	// Skipped files are not pruned: their pid is alive and they are not this client's to judge.
	for _, id := range []string{"hpath", "hsym", "hlink"} {
		if !exists(holderreg.Path(dir, id)) {
			t.Fatalf("%s's file was removed", id)
		}
	}
}

// TestScanChecksDir: a registry directory other users can open is not trusted.
func TestScanChecksDir(t *testing.T) {
	dir := regDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Dir: dir, Build: "B"})
	if err := c.Scan(ctx(t)); err == nil {
		t.Fatal("scan of an open directory passed")
	}
	if _, err := c.Upgrade(ctx(t)); err == nil {
		t.Fatal("upgrade on an open directory passed")
	}
}

// TestScanPrunesJunk: temporary and unreadable files a crash left behind are removed on scan
// once they are old enough; fresh ones stay, as a holder may be writing one.
func TestScanPrunesJunk(t *testing.T) {
	dir := regDir(t)
	old := time.Now().Add(-2 * holderreg.TmpGrace)
	for _, n := range []string{".tmp-x-1", "bad.json", ".tmp-fresh-2"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if n != ".tmp-fresh-2" {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := New(Config{Dir: dir, Build: "B"})
	if err := c.Scan(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, ".tmp-x-1")) || exists(filepath.Join(dir, "bad.json")) {
		t.Fatal("old junk kept")
	}
	if !exists(filepath.Join(dir, ".tmp-fresh-2")) {
		t.Fatal("fresh temporary file removed")
	}
}

// TestUpgradeDrainsOlderOwnBuild: two holders of this build run (a slow start was retried).
// The newer takes new panes; the older is drained, and with no panes it exits. The log says a
// second holder of this build was drained.
func TestUpgradeDrainsOlderOwnBuild(t *testing.T) {
	dir := regDir(t)
	older := inproc(t, dir, "B", "hb1", hp.V1())
	time.Sleep(time.Millisecond) // distinct start times
	inproc(t, dir, "B", "hb2", hp.V1())
	var mu sync.Mutex
	var logs []string
	c := New(Config{Dir: dir, Build: "B", Logf: func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	}})
	collect(t, c)
	cur, err := c.Upgrade(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if cur.Instance() != "hb2" {
		t.Fatalf("current %s, want the newer hb2", cur.Instance())
	}
	mu.Lock()
	logged := slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, "hb1") && strings.Contains(l, "second holder of this build")
	})
	mu.Unlock()
	if !logged {
		t.Errorf("no log line for the second holder of this build: %q", logs)
	}
	select {
	case <-older.Done():
	case <-time.After(waitLong):
		t.Fatal("older holder of this build was not drained")
	}
	waitGone(t, holderreg.Path(dir, "hb1"))
}

// TestWriteAfterByeIsClosed: a holder sent bye and stopped reading, so the next request's write
// fails with a broken pipe before the connection has ended. The client reports ErrClosed, the
// normal end of a connection, not the pipe error.
func TestWriteAfterByeIsClosed(t *testing.T) {
	dir := regDir(t)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: holderreg.SocketPath(dir, "hbye"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e := holderreg.Entry{Instance: "hbye", Build: "B", Protocol: holderreg.Protocol{Min: 1, Max: 1},
		Pid: os.Getpid(), Socket: holderreg.SocketPath(dir, "hbye"), Started: time.Now()}
	if err := holderreg.Write(dir, e); err != nil {
		t.Fatal(err)
	}
	served := make(chan *net.UnixConn, 1)
	go func() {
		nc, err := l.AcceptUnix()
		if err != nil {
			return
		}
		pc := hp.NewConn(nc, hp.V1())
		if _, err := pc.AcceptHello(); err != nil {
			t.Error(err)
			return
		}
		_ = pc.WriteFrame(&hp.Welcome{Version: pc.Version(), Instance: "hbye", Pid: uint32(os.Getpid()),
			Build: "B", RingSize: 1024, Panes: []hp.PaneInfo{}})
		_ = pc.WriteFrame(&hp.Bye{})
		_ = nc.CloseRead() // the holder reads nothing more; its end stays open
		served <- nc
	}()
	c := New(Config{Dir: dir, Build: "B"})
	collect(t, c)
	if err := c.Scan(ctx(t)); err != nil {
		t.Fatal(err)
	}
	nc := <-served
	t.Cleanup(func() { nc.Close() })
	hs := c.Holders()
	if len(hs) != 1 {
		t.Fatalf("holders: %d", len(hs))
	}
	h := hs[0]
	waitFor(t, "bye read", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.bye })
	if err := h.Ping(ctx(t)); !errors.Is(err, ErrClosed) {
		t.Fatalf("ping after bye: %v, want ErrClosed", err)
	}
	if err := h.Err(); err != nil {
		t.Fatalf("connection end after bye: %v, want a clean end", err)
	}
}
