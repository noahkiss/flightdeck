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
// answers each request with fixed frames until the client sends handover. It keeps an input
// count per connection and refuses input at any other offset, as the real holder must. Like the
// real holder, it writes through one queue, so its reader never blocks on a write.
func testHolder(t *testing.T, conn net.Conn, reg *hp.Registry, got chan<- []hp.Frame) {
	defer conn.Close()
	c := hp.NewConn(conn, reg)
	out := make(chan hp.Frame, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range out {
			if err := c.WriteFrame(f); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("holder write: %v", err)
			}
		}
	}()
	defer func() { close(out); <-done }()
	c.OnSkip = func(h hp.Header) { out <- hp.UnknownKind(h) }
	if _, err := c.AcceptHello(); err != nil {
		t.Errorf("holder: %v", err)
		close(got)
		return
	}
	out <- &hp.Welcome{
		Version: c.Version(), Instance: "h", Pid: 1, Build: "holder", RingSize: 4 << 20,
		Previous: hp.PreviousCrash,
		Panes:    []hp.PaneInfo{{UUID: paneA, Handle: 3, Pid: 99, Cols: 80, Rows: 24, Count: 40, Oldest: 0}},
	}
	var frames []hp.Frame
	var inputCount uint64
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Errorf("holder read: %v", err)
			break
		}
		frames = append(frames, f)
		switch f := f.(type) {
		case *hp.Input:
			if f.Offset != inputCount {
				out <- &hp.Error{Handle: f.Handle, Token: f.Token, Ref: hp.KindInput,
					Code: hp.CodeInputOffset, Offset: inputCount, Message: "wrong input offset"}
				continue
			}
			inputCount += uint64(len(f.Data))
		case *hp.Ping:
			out <- &hp.Pong{Token: f.Token}
		case *hp.Replay:
			out <- &hp.ReplayData{Handle: f.Handle, Token: f.Token, Offset: f.From, Data: []byte("0123456789")}
			out <- &hp.ReplayEnd{Handle: f.Handle, Token: f.Token, Start: f.From, End: f.From + 10}
			// OutputV2 lays itself out at the connection's version.
			out <- &OutputV2{Handle: f.Handle, Offset: f.From + 10, Seq: 1, Data: []byte("live")}
			// Stats exists only at version 2. A connection at version 1 must refuse to send it.
			if c.Version() >= 2 {
				out <- &Stats{Handle: f.Handle, Used: 50}
			} else if err := c.WriteFrame(&Stats{Handle: f.Handle, Used: 50}); !errors.Is(err, hp.ErrNotInVersion) {
				t.Errorf("holder stats at v%d: err %v, want ErrNotInVersion", c.Version(), err)
			}
			out <- &hp.Pong{Token: 1}
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
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 2, Offset: 0, Data: []byte("x")}))
	must(t, c.WriteFrame(&hp.Replay{Handle: 3, Token: 9, From: 30}))
	recv := readUntilPong(t, c)
	must(t, c.WriteFrame(&hp.Handover{}))
	return c.Version(), recv, <-got
}

func readUntilPong(t *testing.T, c *hp.Conn) []hp.Frame {
	t.Helper()
	var recv []hp.Frame
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		recv = append(recv, f)
		if _, ok := f.(*hp.Pong); ok {
			return recv
		}
	}
}

func kinds(fs []hp.Frame) []hp.Kind {
	var ks []hp.Kind
	for _, f := range fs {
		ks = append(ks, f.Header().Kind)
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
		// The v2 client decodes v1 output, whose fixed part is shorter, with its v2 type.
		if o := recv[2].(*OutputV2); o.Seq != 0 || string(o.Data) != "live" || o.Offset != 40 {
			t.Errorf("client output %+v", o)
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
		if o := recv[2].(*OutputV2); o.Seq != 1 || string(o.Data) != "live" {
			t.Errorf("client output %+v", o)
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
		if out.Offset != 30 || out.Token != 9 || string(out.Data) != "0123456789" {
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
		recv := readUntilPong(t, cn.c)
		if want := 4 + i; len(recv) != want { // the v2 connection also gets stats
			t.Errorf("connection %d: %d frames, want %d", i, len(recv), want)
		}
		if o := recv[2].(*OutputV2); o.Data == nil {
			t.Errorf("connection %d output %+v", i, o)
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

// TestHelloAppendedFields sends a version 2 hello with a field appended, raw, to a version 1
// holder. The handshake is read by every version, so the holder must accept it and pick 1.
func TestHelloAppendedFields(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	type result struct {
		h   *hp.Hello
		v   hp.Version
		err error
	}
	res := make(chan result, 1)
	go func() {
		defer b.Close()
		c := hp.NewConn(b, hp.V1())
		h, err := c.AcceptHello()
		if err == nil {
			err = c.WriteFrame(&hp.Welcome{Version: c.Version()})
		}
		res <- result{h, c.Version(), err}
	}()
	hello, _ := hp.AppendFrame(nil, &HelloV2{Min: 1, Max: 2, Build: "next", Features: []string{"a", "b"}}, 0)
	if _, err := a.Write(hello); err != nil {
		t.Fatal(err)
	}
	f, n, err := hp.V1().DecodeFrame(0, mustRead(t, a))
	if err != nil || f == nil {
		t.Fatalf("welcome: %v %d", err, n)
	}
	r := <-res
	if r.err != nil || r.v != 1 || r.h.Max != 2 || r.h.Build != "next" {
		t.Errorf("holder: %+v", r)
	}
	if w := f.(*hp.Welcome); w.Version != 1 {
		t.Errorf("welcome version %d", w.Version)
	}
}

func mustRead(t *testing.T, r io.Reader) []byte {
	t.Helper()
	hdr := make([]byte, hp.HeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Fatal(err)
	}
	n := int(hdr[10])<<24 | int(hdr[11])<<16 | int(hdr[12])<<8 | int(hdr[13])
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatal(err)
	}
	return append(hdr, p...)
}

// TestInputOffset: input carries the pane's input offset. The holder refuses a frame at the
// wrong offset whole, names the expected offset and echoes the token; gff-hab resends from there.
func TestInputOffset(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	got := make(chan []hp.Frame, 1)
	go testHolder(t, b, hp.V1(), got)
	c := hp.NewConn(a, hp.V1())
	if _, err := c.Hello("gff-hab"); err != nil {
		t.Fatal(err)
	}
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 1, Offset: 0, Data: []byte("abc")}))
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 2, Offset: 10, Data: []byte("x")})) // wrong
	must(t, c.WriteFrame(&hp.Ping{Token: 3}))
	recv := readUntilPong(t, c)
	if len(recv) != 2 {
		t.Fatalf("got %v", kinds(recv))
	}
	e := recv[0].(*hp.Error)
	if e.Token != 2 || e.Handle != 3 || e.Ref != hp.KindInput || e.Code != hp.CodeInputOffset || e.Offset != 3 {
		t.Fatalf("error %+v", e)
	}
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 4, Offset: e.Offset, Data: []byte("x")}))
	must(t, c.WriteFrame(&hp.Ping{Token: 5}))
	if recv := readUntilPong(t, c); len(recv) != 1 {
		t.Fatalf("resend refused: %v", kinds(recv))
	}
	must(t, c.WriteFrame(&hp.Handover{}))
	var accepted []byte
	for _, f := range <-got {
		if in, ok := f.(*hp.Input); ok && in.Token != 2 {
			accepted = append(accepted, in.Data...)
		}
	}
	if string(accepted) != "abcx" {
		t.Errorf("accepted %q", accepted)
	}
}

// TestUnknownKindReply: a request the holder skips gets an error with code unknown kind, its
// pane and its token.
func TestUnknownKindReply(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	got := make(chan []hp.Frame, 1)
	go testHolder(t, b, hp.V1(), got)
	c := hp.NewConn(a, hp.V1())
	if _, err := c.Hello("gff-hab"); err != nil {
		t.Fatal(err)
	}
	// A request kind no version knows, written raw under the connection.
	raw := []byte{0x7f, 0x02, 0, 0, 0, 3, 0, 0, 0, 77, 0, 0, 0, 2, 'h', 'i'}
	if _, err := a.Write(raw); err != nil {
		t.Fatal(err)
	}
	must(t, c.WriteFrame(&hp.Ping{Token: 78}))
	recv := readUntilPong(t, c)
	e, ok := recv[0].(*hp.Error)
	if !ok || e.Code != hp.CodeUnknownKind || e.Ref != 0x7f02 || e.Token != 77 || e.Handle != 3 {
		t.Fatalf("got %#v", recv[0])
	}
	must(t, c.WriteFrame(&hp.Handover{}))
	<-got
}

// rawConn is an in-memory peer: reads come from in, writes are dropped.
type rawConn struct{ io.Reader }

func (rawConn) Write(p []byte) (int, error) { return len(p), nil }

// handshaken returns a client Conn over a stream that starts with a welcome at version v, then
// rest.
func handshaken(t *testing.T, reg *hp.Registry, v hp.Version, rest []byte) *hp.Conn {
	t.Helper()
	w, _ := hp.AppendFrame(nil, &hp.Welcome{Version: v}, v)
	c := hp.NewConn(rawConn{bytes.NewReader(append(w, rest...))}, reg)
	if _, err := c.Hello("x"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUnknownKindSkipped(t *testing.T) {
	var stream []byte
	// A kind no version knows.
	stream = append(stream, 0x7e, 0xee, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 5, 'a', 'b', 'c', 'd', 'e')
	// A kind the registry knows, but only from version 2, on a version 1 connection.
	stream, _ = hp.AppendFrame(stream, &Stats{Handle: 3, Used: 1}, 2)
	stream, _ = hp.AppendFrame(stream, &hp.Ping{Token: 42}, 1)

	c := handshaken(t, v2Registry(), 1, stream)
	var skipped []hp.Kind
	c.OnSkip = func(h hp.Header) { skipped = append(skipped, h.Kind) }
	f, err := c.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := f.(*hp.Ping); !ok || p.Token != 42 {
		t.Fatalf("got %#v, want ping 42", f)
	}
	if c.Skipped() != 2 || !reflect.DeepEqual(skipped, []hp.Kind{0x7eee, KindStats}) {
		t.Errorf("skipped %d %v", c.Skipped(), skipped)
	}
	if _, err := c.ReadFrame(); err != io.EOF {
		t.Errorf("end: %v, want io.EOF", err)
	}
}

// TestFixedPartGrows: kinds that end in raw bytes put a fixed-part length first, so version 2
// can append a fixed field and the raw bytes still decode exactly.
func TestFixedPartGrows(t *testing.T) {
	data := []byte("payload bytes")
	for _, v := range []hp.Version{1, 2} {
		b, _ := hp.AppendFrame(nil, &OutputV2{Handle: 3, Offset: 5, Seq: 9, Data: data}, v)
		f, _, err := v2Registry().DecodeFrame(v, b)
		if err != nil {
			t.Fatal(err)
		}
		want := &OutputV2{Handle: 3, Offset: 5, Data: data}
		if v == 2 {
			want.Seq = 9
		}
		if !reflect.DeepEqual(f, want) {
			t.Errorf("v%d: %+v", v, f)
		}
	}
}

func TestSizeCaps(t *testing.T) {
	cases := []struct {
		name string
		f    func(n int) hp.Frame
		max  int
	}{
		{"output", func(n int) hp.Frame { return &hp.Output{Handle: 1, Data: make([]byte, n)} }, hp.MaxOutputData},
		{"replay-data", func(n int) hp.Frame { return &hp.ReplayData{Handle: 1, Data: make([]byte, n)} }, hp.MaxOutputData},
		{"input", func(n int) hp.Frame { return &hp.Input{Handle: 1, Data: make([]byte, n)} }, hp.MaxInputData},
	}
	for _, c := range cases {
		ok, err := hp.AppendFrame(nil, c.f(c.max), 1)
		if err != nil {
			t.Errorf("%s at the cap: %v", c.name, err)
		}
		if _, err := hp.AppendFrame(nil, c.f(c.max+1), 1); !errors.Is(err, hp.ErrTooLarge) {
			t.Errorf("%s over the cap: %v, want ErrTooLarge", c.name, err)
		}
		// A receiver rejects an oversized frame too. Build one by growing the length by hand.
		big := append(ok, 0)
		n := len(big) - hp.HeaderSize
		big[10], big[11], big[12], big[13] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		if _, _, err := hp.V1().DecodeFrame(1, big); !errors.Is(err, hp.ErrTooLarge) {
			t.Errorf("%s decode over the cap: %v, want ErrTooLarge", c.name, err)
		}
		// The same frame on a live connection ends it with ErrTooLarge.
		if _, err := handshaken(t, hp.V1(), 1, big).ReadFrame(); !errors.Is(err, hp.ErrTooLarge) {
			t.Errorf("%s read over the cap: %v, want ErrTooLarge", c.name, err)
		}
	}
}

func TestBadFrames(t *testing.T) {
	// A length over MaxPayload ends the connection.
	huge := []byte{0x01, 0x04, 0, 0, 0, 3, 0, 0, 0, 0, 0x7f, 0xff, 0xff, 0xff}
	if _, err := handshaken(t, hp.V1(), 1, huge).ReadFrame(); !errors.Is(err, hp.ErrTooLarge) {
		t.Errorf("huge: %v, want ErrTooLarge", err)
	}
	// A known kind with a payload too short for its fields is an error.
	short := []byte{0x01, 0x05, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 1, 0x00}
	if _, err := handshaken(t, hp.V1(), 1, short).ReadFrame(); !errors.Is(err, hp.ErrShortPayload) {
		t.Errorf("short: %v, want ErrShortPayload", err)
	}
	// A fixed-part length longer than the payload is an error.
	fixed := []byte{0x01, 0x04, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 4, 0, 50, 1, 2}
	if _, err := handshaken(t, hp.V1(), 1, fixed).ReadFrame(); !errors.Is(err, hp.ErrShortPayload) {
		t.Errorf("fixed: %v, want ErrShortPayload", err)
	}
	// A stream that ends inside a frame.
	cut := []byte{0x01, 0x04, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 20, 1, 2}
	if _, err := handshaken(t, hp.V1(), 1, cut).ReadFrame(); err != io.ErrUnexpectedEOF {
		t.Errorf("cut: %v, want io.ErrUnexpectedEOF", err)
	}
	// Pane frames are illegal before negotiation.
	c := hp.NewConn(rawConn{bytes.NewReader(nil)}, hp.V1())
	if err := c.WriteFrame(&hp.Input{Handle: 1, Data: []byte("x")}); !errors.Is(err, hp.ErrNotInVersion) {
		t.Errorf("pre-handshake write: %v, want ErrNotInVersion", err)
	}
}

// TestInputQueueFull: after a queue-full refusal gff-hab waits for input ready, then resends
// from the offset it names.
func TestInputQueueFull(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	// A holder whose input queue holds 4 bytes and drains when gff-hab pings.
	go func() {
		defer b.Close()
		c := hp.NewConn(b, hp.V1())
		if _, err := c.AcceptHello(); err != nil {
			t.Error(err)
			return
		}
		out := make(chan hp.Frame, 16)
		go func() {
			for f := range out {
				_ = c.WriteFrame(f)
			}
		}()
		defer close(out)
		out <- &hp.Welcome{Version: c.Version()}
		var count uint64
		var queued int
		full := false
		for {
			f, err := c.ReadFrame()
			if err != nil {
				return
			}
			switch f := f.(type) {
			case *hp.Input:
				if f.Offset != count || queued+len(f.Data) > 4 {
					code := hp.CodeInputOffset
					if f.Offset == count {
						code, full = hp.CodeQueueFull, true
					}
					out <- &hp.Error{Handle: f.Handle, Token: f.Token, Ref: hp.KindInput, Code: code, Offset: count}
					continue
				}
				count += uint64(len(f.Data))
				queued += len(f.Data)
			case *hp.Ping: // the child read everything
				queued = 0
				if full {
					full = false
					out <- &hp.InputReady{Handle: 3, Offset: count}
				}
				out <- &hp.Pong{Token: f.Token}
			}
		}
	}()
	c := hp.NewConn(a, hp.V1())
	if _, err := c.Hello("gff-hab"); err != nil {
		t.Fatal(err)
	}
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 1, Offset: 0, Data: []byte("abc")}))
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 2, Offset: 3, Data: []byte("de")}))
	f, err := c.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	e := f.(*hp.Error)
	if e.Code != hp.CodeQueueFull || e.Token != 2 || e.Offset != 3 {
		t.Fatalf("error %+v", e)
	}
	must(t, c.WriteFrame(&hp.Ping{Token: 9}))
	recv := readUntilPong(t, c)
	r, ok := recv[0].(*hp.InputReady)
	if !ok || r.Handle != 3 || r.Offset != 3 {
		t.Fatalf("got %v", kinds(recv))
	}
	must(t, c.WriteFrame(&hp.Input{Handle: 3, Token: 3, Offset: r.Offset, Data: []byte("de")}))
	must(t, c.WriteFrame(&hp.Ping{Token: 10}))
	if recv := readUntilPong(t, c); len(recv) != 1 {
		t.Fatalf("resend refused: %v", kinds(recv))
	}
}
