package holderclient

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// TestUpgradeByDrain covers acceptance 1 and 2 with real holder processes of two builds.
//
// gff-hab build A starts holder build A and spawns two panes on it, then hands over. gff-hab
// build B finds holder A, starts holder B, and drains A. A new pane lands on B. Both panes on
// A keep running and stay reachable: replay and input and output go through A. When the
// last pane on A ends and is released, A exits cleanly and its registry file and socket are
// gone.
func TestUpgradeByDrain(t *testing.T) {
	dir := regDir(t)
	ps := newProcs(t, dir)

	// gff-hab build A.
	var startedA []*proc
	ca := New(Config{Dir: dir, Build: "A", Start: ps.starter("A", &startedA)})
	eva := collect(t, ca)
	ha, err := ca.Upgrade(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(startedA) != 1 || ha.Build() != "A" || ha.Instance() != startedA[0].instance {
		t.Fatalf("holder A: started %d, build %s", len(startedA), ha.Build())
	}
	var onA []*Pane
	for n := 1; n <= 2; n++ {
		p, err := ca.Spawn(ctx(t), echoSpawn(n))
		if err != nil {
			t.Fatal(err)
		}
		eva.waitOutput(t, p, "ready-")
		onA = append(onA, p)
	}
	ca.Close() // handover

	// gff-hab build B: finds A, starts B, drains A.
	var startedB []*proc
	cb := New(Config{Dir: dir, Build: "B", Start: ps.starter("B", &startedB)})
	evb := collect(t, cb)
	hb, err := cb.Upgrade(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(startedB) != 1 || hb.Build() != "B" || hb.Instance() != startedB[0].instance {
		t.Fatalf("holder B: started %d, build %s", len(startedB), hb.Build())
	}
	var ha2 *Holder
	for _, h := range cb.Holders() {
		if h.Instance() == ha.Instance() {
			ha2 = h
		}
	}
	if ha2 == nil || !ha2.Draining() || len(cb.Holders()) != 2 {
		t.Fatalf("holders after upgrade: %d, A %+v", len(cb.Holders()), ha2)
	}
	if e, err := holderreg.Read(holderreg.Path(dir, ha.Instance())); err != nil || !e.Draining || e.Build != "A" {
		t.Fatalf("A's registry file after drain: %+v %v", e, err)
	}
	if ha2.Welcome().Previous != hp.PreviousHandover {
		t.Fatalf("A's welcome to gff-hab B: previous %d", ha2.Welcome().Previous)
	}

	// A refuses new panes; the client puts them on B.
	if _, err := ha2.Spawn(ctx(t), echoSpawn(9)); !isCode(err, hp.CodeDraining) {
		t.Fatalf("spawn on draining A: %v", err)
	}
	pb, err := cb.Spawn(ctx(t), echoSpawn(3))
	if err != nil {
		t.Fatal(err)
	}
	if pb.Holder != hb {
		t.Fatalf("new pane landed on %s, want B %s", pb.Holder.Instance(), hb.Instance())
	}
	evb.waitOutput(t, pb, "ready-3")

	// Status: which holder each pane lives on.
	where := map[hp.UUID]string{}
	for _, s := range cb.Panes() {
		where[s.UUID] = s.Build
	}
	if where[uuidN(1)] != "A" || where[uuidN(2)] != "A" || where[uuidN(3)] != "B" || len(where) != 3 {
		t.Fatalf("pane placement %v", where)
	}

	// Both panes on A stay reachable through A.
	var reA []*Pane
	for n, p := range onA {
		rp := cb.Pane(p.UUID)
		if rp == nil || rp.Holder != ha2 {
			t.Fatalf("pane %d not found on A", n+1)
		}
		data, end, err := ha2.Replay(ctx(t), rp.Handle, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "ready-") || end.Gap {
			t.Fatalf("replay of pane %d: %q %+v", n+1, data, end)
		}
		if err := ha2.Input(ctx(t), rp.Handle, []byte("hello\n")); err != nil {
			t.Fatal(err)
		}
		evb.waitOutput(t, rp, "got-"+string(rune('1'+n))+":hello")
		reA = append(reA, rp)
	}
	if err := hb.Input(ctx(t), pb.Handle, []byte("there\n")); err != nil {
		t.Fatal(err)
	}
	evb.waitOutput(t, pb, "got-3:there")

	// Acceptance 2: the first pane on A ends; A stays. The second ends; A exits.
	pA := startedA[0]
	for i, rp := range reA {
		if err := ha2.Input(ctx(t), rp.Handle, []byte("quit\n")); err != nil {
			t.Fatal(err)
		}
		if x := evb.waitExited(t, rp); x.Status.Code != 0 {
			t.Fatalf("pane exit %+v", x.Status)
		}
		if err := ha2.Release(ctx(t), rp.Handle); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := ha2.Ping(ctx(t)); err != nil {
				t.Fatalf("A ended with a pane left: %v", err)
			}
			if !exists(holderreg.Path(dir, ha.Instance())) {
				t.Fatal("A's registry file went with a pane left")
			}
		}
	}
	if err := pA.exited(t); err != nil {
		t.Fatalf("holder A exit: %v", err)
	}
	if e := evb.waitEnd(t, ha2); e.Err != nil {
		t.Fatalf("A's connection ended with %v, want bye", e.Err)
	}
	evb.wait(t, "bye from A", func(e Event) bool { _, ok := e.Frame.(*hp.Bye); return ok && e.Holder == ha2 })
	waitGone(t, holderreg.Path(dir, ha.Instance()))
	waitGone(t, holderreg.SocketPath(dir, ha.Instance()))

	// B is untouched and alone.
	if hs := cb.Holders(); len(hs) != 1 || hs[0] != hb {
		t.Fatalf("holders after A exit: %d", len(hs))
	}
	if err := hb.Input(ctx(t), pb.Handle, []byte("still\n")); err != nil {
		t.Fatal(err)
	}
	evb.waitOutput(t, pb, "got-3:still")
	if es, _ := holderreg.List(dir); len(es) != 1 || es[0].Instance != hb.Instance() {
		t.Fatalf("registry after A exit: %+v", es)
	}
}

// TestCrashDuringDrain (spec 6.7): the draining holder A dies. Its panes are lost, and the
// client reports A's connection ended with an error. The init system restarts A, because it
// failed. The restarted A finds its own registry file marked draining, so it exits at once
// with status 0 and removes its files. A later scan finds nothing stale.
func TestCrashDuringDrain(t *testing.T) {
	dir := regDir(t)
	ps := newProcs(t, dir)
	pa := ps.start("A", "hcrash")

	var startedB []*proc
	cb := New(Config{Dir: dir, Build: "B", Start: ps.starter("B", &startedB)})
	ev := collect(t, cb)
	// Wait until A is listed, then let gff-hab B take over: start B and drain A.
	ca := New(Config{Dir: dir, Build: "A"})
	waitFor(t, "holder A listed", func() bool { _ = ca.Scan(ctx(t)); return ca.Current() != nil })
	p, err := ca.Spawn(ctx(t), echoSpawn(1))
	if err != nil {
		t.Fatal(err)
	}
	ca.Close()
	if _, err := cb.Upgrade(ctx(t)); err != nil {
		t.Fatal(err)
	}
	ha := cb.Pane(p.UUID).Holder
	if !ha.Draining() || ha.Instance() != "hcrash" {
		t.Fatalf("A not draining")
	}

	if err := pa.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := pa.exited(t); err == nil {
		t.Fatal("killed holder exited cleanly")
	}
	if e := ev.waitEnd(t, ha); e.Err == nil {
		t.Fatal("crash reported as a clean end")
	}
	if cb.Pane(p.UUID) != nil {
		t.Fatal("pane on the crashed holder still listed")
	}
	// The crash left A's files behind.
	e, err := holderreg.Read(holderreg.Path(dir, "hcrash"))
	if err != nil || !e.Draining {
		t.Fatalf("crashed A's registry file: %+v %v", e, err)
	}

	// The init system restarts the failed instance.
	pa2 := ps.start("A", "hcrash")
	if err := pa2.exited(t); err != nil {
		t.Fatalf("restarted draining holder: %v\n%s", err, pa2.stderr.String())
	}
	waitGone(t, holderreg.Path(dir, "hcrash"))
	waitGone(t, holderreg.SocketPath(dir, "hcrash"))
	if err := cb.Scan(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if hs := cb.Holders(); len(hs) != 1 || hs[0].Build() != "B" {
		t.Fatalf("holders: %d", len(hs))
	}
}

// TestCrashDuringDrainPruned: the other order. gff-hab scans before the init system restarts
// the crashed holder, so it removes the stale registry file and socket. The restarted holder
// then cannot know it was draining: it comes up empty and not draining. The next Upgrade
// drains it, and with no panes it exits at once.
func TestCrashDuringDrainPruned(t *testing.T) {
	dir := regDir(t)
	ps := newProcs(t, dir)
	pa := ps.start("A", "hprune")
	ca := New(Config{Dir: dir, Build: "A"})
	waitFor(t, "holder A listed", func() bool { _ = ca.Scan(ctx(t)); return ca.Current() != nil })
	if _, err := ca.Spawn(ctx(t), echoSpawn(1)); err != nil {
		t.Fatal(err)
	}
	ca.Close()

	var startedB []*proc
	cb := New(Config{Dir: dir, Build: "B", Start: ps.starter("B", &startedB)})
	collect(t, cb)
	if _, err := cb.Upgrade(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := pa.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = pa.exited(t)
	waitFor(t, "A's connection to end", func() bool { return len(cb.Holders()) == 1 })
	if err := cb.Scan(ctx(t)); err != nil { // prunes A's files: its pid is gone
		t.Fatal(err)
	}
	if exists(holderreg.Path(dir, "hprune")) || exists(holderreg.SocketPath(dir, "hprune")) {
		t.Fatal("stale files not pruned")
	}

	pa = ps.start("A", "hprune")
	waitFor(t, "restarted A listed", func() bool { return exists(holderreg.Path(dir, "hprune")) })
	if _, err := cb.Upgrade(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := pa.exited(t); err != nil {
		t.Fatalf("restarted empty A after drain: %v", err)
	}
	waitGone(t, holderreg.Path(dir, "hprune"))
	waitGone(t, holderreg.SocketPath(dir, "hprune"))
}

func isCode(err error, code hp.ErrorCode) bool {
	var re *RequestError
	return errors.As(err, &re) && re.Code == code
}
