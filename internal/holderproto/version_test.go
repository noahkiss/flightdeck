package holderproto_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

func TestNegotiate(t *testing.T) {
	cases := []struct {
		client, holder hp.Range
		want           hp.Version
		ok             bool
	}{
		{hp.Range{1, 1}, hp.Range{1, 1}, 1, true},
		{hp.Range{1, 2}, hp.Range{1, 1}, 1, true},
		{hp.Range{1, 1}, hp.Range{1, 2}, 1, true},
		{hp.Range{1, 3}, hp.Range{2, 5}, 3, true},
		{hp.Range{3, 4}, hp.Range{1, 2}, 0, false},
		{hp.Range{2, 1}, hp.Range{1, 2}, 0, false},
	}
	for _, c := range cases {
		v, ok := hp.Negotiate(c.client, c.holder)
		if v != c.want || ok != c.ok {
			t.Errorf("Negotiate(%v, %v) = %d, %v; want %d, %v", c.client, c.holder, v, ok, c.want, c.ok)
		}
	}
}

// testHolder is the holder side of a scripted session. It accepts the hello, welcomes, then
// answers each request with fixed frames until the client sends handover.
func testHolder(t *testing.T, conn net.Conn, reg *hp.Registry, got chan<- []hp.Frame) {
	defer conn.Close()
	c := hp.NewConn(conn, reg)
	if _, err := c.AcceptHello(); err != nil {
		t.Errorf("holder: %v", err)
		close(got)
		return
	}
	err := c.WriteFrame(&hp.Welcome{
		Version: c.Version(), Instance: "h", Pid: 1, Build: "holder", RingSize: 4 << 20,
		Previous: hp.PreviousCrash,
		Panes:    []hp.PaneInfo{{UUID: paneA, Handle: 3, Pid: 99, Cols: 80, Rows: 24, Count: 40, Oldest: 0}},
	})
	if err != nil {
		t.Errorf("holder welcome: %v", err)
	}
	var frames []hp.Frame
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Errorf("holder read: %v", err)
			break
		}
		frames = append(frames, f)
		switch f := f.(type) {
		case *hp.Replay:
			must(t, c.WriteFrame(&hp.ReplayData{Handle: f.Handle, Token: f.Token, Offset: f.From, Data: []byte("0123456789")}))
			must(t, c.WriteFrame(&hp.ReplayEnd{Handle: f.Handle, Token: f.Token, Start: f.From, End: f.From + 10}))
			must(t, c.WriteFrame(&hp.Output{Handle: f.Handle, Offset: f.From + 10, Data: []byte("live")}))
			// Stats exists only at version 2. A connection at version 1 must refuse to send it.
			err := c.WriteFrame(&Stats{Handle: f.Handle, Used: 50})
			if c.Version() >= 2 && err != nil {
				t.Errorf("holder stats at v%d: %v", c.Version(), err)
			}
			if c.Version() < 2 && !errors.Is(err, hp.ErrNotInVersion) {
				t.Errorf("holder stats at v%d: err %v, want ErrNotInVersion", c.Version(), err)
			}
			must(t, c.WriteFrame(&hp.Pong{Token: 1}))
		case *hp.Handover:
			got <- frames
			return
		}
	}
	got <- frames
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
}

// session runs one scripted client against testHolder and returns what each side received.
func session(t *testing.T, clientReg, holderReg *hp.Registry, resize hp.Frame) (hp.Version, []hp.Frame, []hp.Frame) {
	t.Helper()
	a, b := net.Pipe()
	got := make(chan []hp.Frame, 1)
	go testHolder(t, b, holderReg, got)
	defer a.Close()

	c := hp.NewConn(a, clientReg)
	w, err := c.Hello("gff-hab")
	if err != nil {
		t.Fatalf("client hello: %v", err)
	}
	if w.Version != c.Version() || len(w.Panes) != 1 || w.Previous != hp.PreviousCrash {
		t.Fatalf("bad welcome %+v", w)
	}
	must(t, c.WriteFrame(resize))
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Data: []byte("x")}))
	must(t, c.WriteFrame(&hp.Replay{Handle: 3, Token: 9, From: 30}))
	var recv []hp.Frame
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		recv = append(recv, f)
		if _, ok := f.(*hp.Pong); ok {
			break
		}
	}
	must(t, c.WriteFrame(&hp.Handover{}))
	return c.Version(), recv, <-got
}

func kinds(fs []hp.Frame) []hp.Kind {
	var ks []hp.Kind
	for _, f := range fs {
		ks = append(ks, f.Kind())
	}
	return ks
}

func TestCrossVersion(t *testing.T) {
	v1Recv := []hp.Kind{hp.KindReplayData, hp.KindReplayEnd, hp.KindOutput, hp.KindPong}
	v2Recv := []hp.Kind{hp.KindReplayData, hp.KindReplayEnd, hp.KindOutput, KindStats, hp.KindPong}
	sent := []hp.Kind{hp.KindResize, hp.KindInput, hp.KindReplay, hp.KindHandover}
	resizeV2 := &ResizeV2{Handle: 3, Cols: 100, Rows: 50, PixelW: 1000, PixelH: 700}

	t.Run("v1 client, v2 holder", func(t *testing.T) {
		v, recv, got := session(t, hp.V1(), v2Registry(), &hp.Resize{Handle: 3, Cols: 100, Rows: 50})
		if v != 1 {
			t.Fatalf("version %d, want 1", v)
		}
		if !reflect.DeepEqual(kinds(recv), v1Recv) || !reflect.DeepEqual(kinds(got), sent) {
			t.Fatalf("client got %v, holder got %v", kinds(recv), kinds(got))
		}
		// The v2 holder decodes a v1 resize with its v2 type and no pixel sizes.
		if r := got[0].(*ResizeV2); *r != (ResizeV2{Handle: 3, Cols: 100, Rows: 50}) {
			t.Errorf("holder resize %+v", r)
		}
	})

	t.Run("v2 client, v1 holder", func(t *testing.T) {
		v, recv, got := session(t, v2Registry(), hp.V1(), resizeV2)
		if v != 1 {
			t.Fatalf("version %d, want 1", v)
		}
		if !reflect.DeepEqual(kinds(recv), v1Recv) || !reflect.DeepEqual(kinds(got), sent) {
			t.Fatalf("client got %v, holder got %v", kinds(recv), kinds(got))
		}
		// The v2 client encoded its resize at version 1, so the pixel sizes stayed home.
		if r := got[0].(*hp.Resize); *r != (hp.Resize{Handle: 3, Cols: 100, Rows: 50}) {
			t.Errorf("holder resize %+v", r)
		}
	})

	t.Run("v2 client, v2 holder", func(t *testing.T) {
		v, recv, got := session(t, v2Registry(), v2Registry(), resizeV2)
		if v != 2 {
			t.Fatalf("version %d, want 2", v)
		}
		if !reflect.DeepEqual(kinds(recv), v2Recv) || !reflect.DeepEqual(kinds(got), sent) {
			t.Fatalf("client got %v, holder got %v", kinds(recv), kinds(got))
		}
		if r := got[0].(*ResizeV2); *r != *resizeV2 {
			t.Errorf("holder resize %+v", r)
		}
		if s := recv[3].(*Stats); s.Used != 50 {
			t.Errorf("stats %+v", s)
		}
	})

	t.Run("v1 client, v1 holder", func(t *testing.T) {
		v, recv, _ := session(t, hp.V1(), hp.V1(), &hp.Resize{Handle: 3, Cols: 1, Rows: 1})
		if v != 1 || !reflect.DeepEqual(kinds(recv), v1Recv) {
			t.Fatalf("version %d, client got %v", v, kinds(recv))
		}
		out := recv[0].(*hp.ReplayData)
		if out.Offset != 30 || string(out.Data) != "0123456789" {
			t.Errorf("replay data %+v", out)
		}
	})
}

// TestOneClientTwoVersions holds a v1 and a v2 holder connection open at once from one client
// registry, as gff-hab does during a drain, and uses both.
func TestOneClientTwoVersions(t *testing.T) {
	client := v2Registry()
	type conn struct {
		c   *hp.Conn
		got chan []hp.Frame
	}
	var conns []conn
	for _, reg := range []*hp.Registry{hp.V1(), v2Registry()} {
		a, b := net.Pipe()
		defer a.Close()
		got := make(chan []hp.Frame, 1)
		go testHolder(t, b, reg, got)
		c := hp.NewConn(a, client)
		if _, err := c.Hello("gff-hab"); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn{c, got})
	}
	if conns[0].c.Version() != 1 || conns[1].c.Version() != 2 {
		t.Fatalf("versions %d and %d, want 1 and 2", conns[0].c.Version(), conns[1].c.Version())
	}
	// Interleave traffic on both connections.
	for _, cn := range conns {
		must(t, cn.c.WriteFrame(&ResizeV2{Handle: 3, Cols: 10, Rows: 5, PixelW: 100, PixelH: 50}))
	}
	for _, cn := range conns {
		must(t, cn.c.WriteFrame(&hp.Replay{Handle: 3, Token: 1}))
	}
	for i, cn := range conns {
		n := 0
		for {
			f, err := cn.c.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			n++
			if f.Kind() == hp.KindPong {
				break
			}
		}
		if want := 4 + i; n != want { // the v2 connection also gets stats
			t.Errorf("connection %d: %d frames, want %d", i, n, want)
		}
		must(t, cn.c.WriteFrame(&hp.Handover{}))
		got := <-cn.got
		switch r := got[0].(type) {
		case *hp.Resize:
			if i != 0 {
				t.Errorf("v2 holder decoded a v1 resize")
			}
		case *ResizeV2:
			if i != 1 || r.PixelW != 100 {
				t.Errorf("connection %d resize %+v", i, r)
			}
		}
	}
}

func TestRefuse(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	holderErr := make(chan error, 1)
	go func() {
		defer b.Close()
		_, err := hp.NewConn(b, hp.V1()).AcceptHello()
		holderErr <- err
	}()
	future := hp.V1().Clone()
	future.Min, future.Max = 3, 4
	_, err := hp.NewConn(a, future).Hello("gff-hab")
	var re *hp.RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("client err %v, want RefusedError", err)
	}
	if re.Offered != (hp.Range{3, 4}) || re.Holder != (hp.Range{1, 1}) || re.Reason == "" {
		t.Errorf("refused %+v", re)
	}
	if err := <-holderErr; !errors.As(err, &re) {
		t.Errorf("holder err %v, want RefusedError", err)
	}
}

// rawConn is an in-memory peer: reads come from in, writes are dropped.
type rawConn struct{ io.Reader }

func (rawConn) Write(p []byte) (int, error) { return len(p), nil }

// handshaken returns a client Conn over a stream that starts with a v1 welcome, then rest.
func handshaken(t *testing.T, reg *hp.Registry, rest []byte) *hp.Conn {
	t.Helper()
	w, _ := hp.AppendFrame(nil, &hp.Welcome{Version: 1}, 1)
	c := hp.NewConn(rawConn{bytes.NewReader(append(w, rest...))}, reg)
	if _, err := c.Hello("x"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUnknownKindSkipped(t *testing.T) {
	var stream []byte
	// A kind no version knows.
	stream = append(stream, 0x7e, 0xee, 0, 0, 0, 3, 0, 0, 0, 5, 'a', 'b', 'c', 'd', 'e')
	// A kind the registry knows, but only from version 2, on a version 1 connection.
	stream, _ = hp.AppendFrame(stream, &Stats{Handle: 3, Used: 1}, 2)
	stream, _ = hp.AppendFrame(stream, &hp.Ping{Token: 42}, 1)

	c := handshaken(t, v2Registry(), stream)
	if c.Version() != 1 {
		t.Fatalf("version %d", c.Version())
	}
	f, err := c.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := f.(*hp.Ping); !ok || p.Token != 42 {
		t.Fatalf("got %#v, want ping 42", f)
	}
	if c.Skipped() != 2 {
		t.Errorf("skipped %d, want 2", c.Skipped())
	}
	if _, err := c.ReadFrame(); err != io.EOF {
		t.Errorf("end: %v, want io.EOF", err)
	}
}

func TestTrailingFieldsIgnored(t *testing.T) {
	// A version 2 resize carries pixel sizes after the version 1 fields; a version 1 decoder
	// reads its fields and ignores the rest.
	b, _ := hp.AppendFrame(nil, &ResizeV2{Handle: 3, Cols: 7, Rows: 8, PixelW: 1, PixelH: 2}, 2)
	f, n, err := hp.V1().DecodeFrame(1, b)
	if err != nil || n != len(b) {
		t.Fatalf("decode: %v, %d of %d", err, n, len(b))
	}
	if r := f.(*hp.Resize); *r != (hp.Resize{Handle: 3, Cols: 7, Rows: 8}) {
		t.Errorf("resize %+v", r)
	}
}

func TestBadFrames(t *testing.T) {
	// A length over MaxPayload ends the connection.
	huge := []byte{0x01, 0x04, 0, 0, 0, 3, 0x7f, 0xff, 0xff, 0xff}
	if _, err := handshaken(t, hp.V1(), huge).ReadFrame(); !errors.Is(err, hp.ErrTooLarge) {
		t.Errorf("huge: %v, want ErrTooLarge", err)
	}
	// A known kind with a payload too short for its fields is an error.
	short := []byte{0x01, 0x05, 0, 0, 0, 3, 0, 0, 0, 1, 0x00}
	if _, err := handshaken(t, hp.V1(), short).ReadFrame(); !errors.Is(err, hp.ErrShortPayload) {
		t.Errorf("short: %v, want ErrShortPayload", err)
	}
	// A stream that ends inside a frame.
	cut := []byte{0x01, 0x04, 0, 0, 0, 3, 0, 0, 0, 20, 1, 2}
	if _, err := handshaken(t, hp.V1(), cut).ReadFrame(); err != io.ErrUnexpectedEOF {
		t.Errorf("cut: %v, want io.ErrUnexpectedEOF", err)
	}
	// Pane frames are illegal before negotiation.
	c := hp.NewConn(rawConn{bytes.NewReader(nil)}, hp.V1())
	if err := c.WriteFrame(&hp.Input{Handle: 1, Data: []byte("x")}); !errors.Is(err, hp.ErrNotInVersion) {
		t.Errorf("pre-handshake write: %v, want ErrNotInVersion", err)
	}
}
