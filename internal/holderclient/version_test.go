package holderclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/holder"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// kindProbe is a frame kind that exists only in the test version 2 below.
const kindProbe hp.Kind = 0x7f02

type probe struct{ Token uint32 }

func (f *probe) Header() hp.Header              { return hp.Header{Kind: kindProbe, Token: f.Token} }
func (*probe) MarshalPayload(hp.Version) []byte { return nil }

// v2 is a test-only protocol version 2: version 1 plus one kind that only version 2 carries.
// holderproto's own test version 2 also changes resize and output, which the real holder core
// does not implement; this one changes nothing the holder must understand, so a real holder can
// run it.
func v2() *hp.Registry {
	r := hp.V1().Clone()
	r.Max = 2
	r.Register(kindProbe, 2, func(_ hp.Version, h hp.Header, _ []byte) (hp.Frame, error) {
		return &probe{Token: h.Token}, nil
	})
	return r
}

// TestTwoProtocolVersions is acceptance 3: holder A speaks version 1, holder B version 1..2.
// One gff-hab client of build B, which speaks 1..2, holds both connections, each at its own
// version, and drives both: a pane on A and a pane on B, input and output on each, and the
// drain of A to its exit.
func TestTwoProtocolVersions(t *testing.T) {
	dir := regDir(t)
	a := inproc(t, dir, "A", "hv1", hp.V1())

	// gff-hab build A puts a pane on A.
	ca := New(Config{Dir: dir, Build: "A"})
	eva := collect(t, ca)
	if _, err := ca.Upgrade(ctx(t)); err != nil {
		t.Fatal(err)
	}
	pa, err := ca.Spawn(ctx(t), echoSpawn(1))
	if err != nil {
		t.Fatal(err)
	}
	eva.waitOutput(t, pa, "ready-1")
	ca.Close()

	// gff-hab build B, speaking 1..2, starts an in-process holder B at 1..2.
	var b *holder.Holder
	cb := New(Config{Dir: dir, Build: "B", Protocol: v2(), Start: func(context.Context) error {
		b = inproc(t, dir, "B", "hv2", v2())
		return nil
	}})
	ev := collect(t, cb)
	hb, err := cb.Upgrade(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	ha := cb.Pane(uuidN(1)).Holder
	if ha.Version() != 1 || hb.Version() != 2 || hb.Instance() != "hv2" || !ha.Draining() {
		t.Fatalf("A at v%d draining=%v, B %s at v%d", ha.Version(), ha.Draining(), hb.Instance(), hb.Version())
	}
	if e, err := holderreg.Read(holderreg.Path(dir, "hv2")); err != nil || e.Protocol != (holderreg.Protocol{Min: 1, Max: 2}) {
		t.Fatalf("B's registry entry %+v %v", e, err)
	}

	// Each connection encodes at its own version: the v2-only kind cannot go to A, and B
	// decodes it as a version 2 kind (it answers bad request, not unknown kind).
	if err := ha.Send(&probe{Token: 77}); !errors.Is(err, hp.ErrNotInVersion) {
		t.Fatalf("v2 kind to the v1 holder: %v", err)
	}
	_, err = hb.call(ctx(t), func(tok uint32) hp.Frame { return &probe{Token: tok} }, func(hp.Frame) bool { return true })
	if !isCode(err, hp.CodeBadRequest) {
		t.Fatalf("v2 kind to the v2 holder: %v", err)
	}

	// Drive both holders.
	pb, err := cb.Spawn(ctx(t), echoSpawn(2))
	if err != nil {
		t.Fatal(err)
	}
	if pb.Holder != hb {
		t.Fatalf("new pane on %s", pb.Holder.Instance())
	}
	ev.waitOutput(t, pb, "ready-2")
	pa2 := cb.Pane(uuidN(1))
	if data, _, err := ha.Replay(ctx(t), pa2.Handle, 0); err != nil || !strings.Contains(string(data), "ready-1") {
		t.Fatalf("replay on A: %q %v", data, err)
	}
	for _, p := range []*Pane{pa2, pb} {
		if err := p.Holder.Input(ctx(t), p.Handle, []byte("ping\n")); err != nil {
			t.Fatal(err)
		}
	}
	ev.waitOutput(t, pa2, "got-1:ping")
	ev.waitOutput(t, pb, "got-2:ping")
	for _, s := range cb.Panes() {
		want := map[hp.UUID]hp.Version{uuidN(1): 1, uuidN(2): 2}[s.UUID]
		if s.Version != want {
			t.Fatalf("pane %v on %s at v%d, want v%d", s.UUID, s.Instance, s.Version, want)
		}
	}

	// A's last pane ends; A exits; B serves on.
	if err := ha.Input(ctx(t), pa2.Handle, []byte("quit\n")); err != nil {
		t.Fatal(err)
	}
	ev.waitExited(t, pa2)
	if err := ha.Release(ctx(t), pa2.Handle); err != nil {
		t.Fatal(err)
	}
	<-a.Done()
	ev.waitEnd(t, ha)
	waitGone(t, holderreg.Path(dir, "hv1"))
	waitGone(t, holderreg.SocketPath(dir, "hv1"))
	if err := hb.Input(ctx(t), pb.Handle, []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	ev.waitOutput(t, pb, "got-2:after")
	select {
	case <-b.Done():
		t.Fatal("B exited")
	default:
	}
}

// TestUnreachableHolder: a holder that shares no protocol version with gff-hab is reported as
// unreachable and left alone; gff-hab starts its own holder beside it (spec 4.3).
func TestUnreachableHolder(t *testing.T) {
	dir := regDir(t)
	only2 := v2()
	only2.Min = 2
	old := inproc(t, dir, "Z", "hz", only2)
	c := New(Config{Dir: dir, Build: "B", Start: func(context.Context) error {
		inproc(t, dir, "B", "hb", hp.V1())
		return nil
	}})
	collect(t, c)
	h, err := c.Upgrade(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.Instance() != "hb" || len(c.Holders()) != 1 {
		t.Fatalf("current %s, %d holders", h.Instance(), len(c.Holders()))
	}
	if u := c.Unreachable(); len(u) != 1 || u[0].Instance != "hz" {
		t.Fatalf("unreachable %+v", u)
	}
	select {
	case <-old.Done():
		t.Fatal("unreachable holder was ended")
	default:
	}
	if !exists(holderreg.Path(dir, "hz")) {
		t.Fatal("unreachable holder's registry file removed")
	}
}

// TestScanPrunesStale: a registry file whose pid is gone is removed on scan, with its socket.
func TestScanPrunesStale(t *testing.T) {
	dir := regDir(t)
	dead := holderreg.Entry{Instance: "hdead", Build: "A", Pid: 1 << 30, Socket: holderreg.SocketPath(dir, "hdead")}
	if err := holderreg.Write(dir, dead); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Dir: dir, Build: "B"})
	if err := c.Scan(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if exists(holderreg.Path(dir, "hdead")) || len(c.Holders()) != 0 {
		t.Fatal("stale entry kept")
	}
	if _, err := c.Upgrade(ctx(t)); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("upgrade with no Start: %v", err)
	}
}
