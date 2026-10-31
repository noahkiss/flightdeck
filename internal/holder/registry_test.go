package holder

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// regHolder starts a holder that keeps its registry file in a fresh directory, with its socket
// at <dir>/<instance>.sock. before runs on the directory first. It returns the holder, the
// directory, and a channel that yields Serve's result.
func regHolder(t *testing.T, instance string, before func(dir string)) (*Holder, string, <-chan error) {
	t.Helper()
	return regHolderCfg(t, instance, Config{}, before)
}

// regHolderCfg is regHolder with more of the config set.
func regHolderCfg(t *testing.T, instance string, cfg Config, before func(dir string)) (*Holder, string, <-chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if before != nil {
		before(dir)
	}
	l, err := Listen(holderreg.SocketPath(dir, instance))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Build, cfg.Instance, cfg.RegistryDir = "reg-test", instance, dir
	if cfg.ShutdownGrace == 0 {
		cfg.ShutdownGrace = 200 * time.Millisecond
	}
	h := New(cfg)
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	t.Cleanup(func() {
		h.Shutdown()
		<-h.Done()
	})
	return h, dir, served
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists", path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRegistryLifecycle: the holder lists itself while it serves, marks its file draining on
// drain, and removes its registry file and socket when the drain's last release ends it.
func TestRegistryLifecycle(t *testing.T) {
	h, dir, served := regHolder(t, "inst-a", nil)
	sock := holderreg.SocketPath(dir, "inst-a")
	c := dial(t, sock, true)

	e, err := holderreg.Read(holderreg.Path(dir, "inst-a"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Instance != "inst-a" || e.Build != "reg-test" || e.Pid != os.Getpid() || e.Socket != sock ||
		e.Draining || e.Protocol != (holderreg.Protocol{Min: 1, Max: 1}) || e.Started.IsZero() {
		t.Fatalf("registry entry %+v", e)
	}
	if fi, err := os.Stat(holderreg.Path(dir, "inst-a")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("registry file mode: %v %v", fi.Mode(), err)
	}

	s := c.sh(1, `echo ready; while :; do sleep 0.05; done`)
	c.waitOutput(s.Handle, "ready")
	c.send(&hp.Drain{Token: c.tok()})
	c.sync()
	if e, err := holderreg.Read(holderreg.Path(dir, "inst-a")); err != nil || !e.Draining {
		t.Fatalf("after drain: %+v, %v", e, err)
	}

	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	c.until(func(f hp.Frame) bool { _, ok := f.(*hp.Bye); return ok })
	<-h.Done()
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	waitGone(t, holderreg.Path(dir, "inst-a"))
	waitGone(t, sock)
	if des, _ := os.ReadDir(dir); len(des) != 0 {
		t.Fatalf("left behind: %v", des)
	}
}

// TestRegistryShutdownRemovesFile: shutdown removes the registry file too, and only this
// holder's file.
func TestRegistryShutdownRemovesFile(t *testing.T) {
	other := holderreg.Entry{Instance: "inst-other", Build: "x", Pid: os.Getpid(), Socket: "/nowhere"}
	h, dir, _ := regHolder(t, "inst-b", func(dir string) {
		if err := holderreg.Write(dir, other); err != nil {
			t.Fatal(err)
		}
	})
	h.Shutdown()
	waitGone(t, holderreg.Path(dir, "inst-b"))
	waitGone(t, holderreg.SocketPath(dir, "inst-b"))
	if _, err := os.Stat(holderreg.Path(dir, "inst-other")); err != nil {
		t.Fatalf("another holder's file was touched: %v", err)
	}
}

// TestRestartAfterCrashWhileDraining (spec 6.7): a holder that crashed while draining left its
// registry file, marked draining, and its socket. The init system restarts the same instance.
// The restarted holder has no panes and is not the current build, so it exits at once and
// cleanly, removing both files.
func TestRestartAfterCrashWhileDraining(t *testing.T) {
	h, dir, served := regHolder(t, "inst-c", func(dir string) {
		dead := holderreg.Entry{Instance: "inst-c", Build: "old", Pid: 1 << 30, Draining: true,
			Socket: holderreg.SocketPath(dir, "inst-c"), Started: time.Now().Add(-time.Hour)}
		if err := holderreg.Write(dir, dead); err != nil {
			t.Fatal(err)
		}
		// The crashed holder's socket file: present, nobody listening.
		if err := os.WriteFile(filepath.Join(dir, "inst-c.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	select {
	case <-h.Done():
	case <-time.After(waitLong):
		t.Fatal("restarted draining holder did not exit")
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	waitGone(t, holderreg.Path(dir, "inst-c"))
	waitGone(t, holderreg.SocketPath(dir, "inst-c"))
}

// TestRestartAfterCrashNotDraining: a holder restarted after a crash outside a drain serves as
// usual, and its registry file replaces the dead one.
func TestRestartAfterCrashNotDraining(t *testing.T) {
	_, dir, _ := regHolder(t, "inst-d", func(dir string) {
		dead := holderreg.Entry{Instance: "inst-d", Build: "reg-test", Pid: 1 << 30,
			Socket: holderreg.SocketPath(dir, "inst-d"), Started: time.Now().Add(-time.Hour)}
		if err := holderreg.Write(dir, dead); err != nil {
			t.Fatal(err)
		}
	})
	c := dial(t, holderreg.SocketPath(dir, "inst-d"), true)
	if c.w.Draining {
		t.Fatal("restarted holder is draining")
	}
	c.sh(1, `echo hi`)
	e, err := holderreg.Read(holderreg.Path(dir, "inst-d"))
	if err != nil || e.Pid != os.Getpid() || e.Draining {
		t.Fatalf("registry entry %+v, %v", e, err)
	}
}

// TestRegistryRewrittenWhenPruned: a reader removed a live holder's registry file (a prune that
// raced a restart). The holder notices and writes the file again, with the same content.
func TestRegistryRewrittenWhenPruned(t *testing.T) {
	_, dir, _ := regHolderCfg(t, "inst-p", Config{RegistryCheck: 10 * time.Millisecond}, nil)
	path := holderreg.Path(dir, "inst-p")
	dial(t, holderreg.SocketPath(dir, "inst-p"), true) // Serve has registered once it answers
	want, err := holderreg.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitLong)
	for {
		got, err := holderreg.Read(path)
		if err == nil {
			if got.Pid != want.Pid || !got.Started.Equal(want.Started) || got.Socket != want.Socket {
				t.Fatalf("rewritten entry %+v, want %+v", got, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry file not rewritten: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestDrainWaitsForReleasedChild (spec 6.1, 4.3): a pane whose program ignores SIGHUP is
// released while the holder drains. The release leaves the program running, so the holder must
// not exit yet: nothing else would reap it. After ShutdownGrace the holder kills it, reaps it,
// and only then exits and removes its files.
func TestDrainWaitsForReleasedChild(t *testing.T) {
	h, dir, served := regHolder(t, "inst-h", nil)
	c := dial(t, holderreg.SocketPath(dir, "inst-h"), true)
	s := c.sh(1, `trap '' HUP; echo ready; while :; do sleep 0.05; done`)
	c.waitOutput(s.Handle, "ready")
	pid := int(s.Pid)
	c.send(&hp.Drain{Token: c.tok()})
	c.sync()
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})

	// Right after the release the program still runs, and the holder still owns it.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("program ended at once on release: %v", err)
	}
	select {
	case <-h.Done():
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("holder exited with its released program still running (kill 0: %v)", err)
		}
	case <-time.After(waitLong):
		t.Fatal("draining holder did not exit")
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("program's group left behind: %v", err)
	}
	waitGone(t, holderreg.Path(dir, "inst-h"))
	waitGone(t, holderreg.SocketPath(dir, "inst-h"))
}
