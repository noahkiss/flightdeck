package link

import (
	"sync"
	"syscall"
	"testing"
)

// TestLostHolderTold: a holder killed by SIGKILL is told to Lost once, with its instance, its
// build and the panes lost, for gff-hab's health record (diagnostics.md 6.3).
func TestLostHolderTold(t *testing.T) {
	hdir := shortDir(t)
	doomed := startHolder(t, hdir, "doomed", 0)
	type lost struct {
		inst, build string
		panes       int
	}
	var mu sync.Mutex
	var got []lost
	l := openLink(t, Config{HolderDir: hdir, Lost: func(inst, build string, panes int) {
		mu.Lock()
		got = append(got, lost{inst, build, panes})
		mu.Unlock()
	}})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	spawn(t, l, uuidN(51), "exec sleep 1000")
	doomed.Process.Signal(syscall.SIGKILL)
	waitFor(t, "Lost", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) > 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].inst != "doomed" || got[0].panes != 1 || got[0].build == "" {
		t.Fatalf("Lost got %+v", got)
	}
}
