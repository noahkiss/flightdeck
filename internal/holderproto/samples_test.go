package holderproto_test

import (
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// Test-only version 2. It proves that one codebase speaks two versions. Version 2 appends pixel
// sizes to resize, appends a fixed field to output (a kind that ends in raw bytes), adds a stats
// frame kind, and appends a feature list to hello. None of it exists in the real protocol.

// KindStats is a frame kind that exists only in test version 2.
const KindStats hp.Kind = 0x7f01

// ResizeV2 is resize with pixel sizes appended at version 2.
type ResizeV2 struct {
	Handle         hp.Handle
	Token          uint32
	Cols, Rows     uint16
	PixelW, PixelH uint16
}

func (f *ResizeV2) Header() hp.Header {
	return hp.Header{Kind: hp.KindResize, Pane: f.Handle, Token: f.Token}
}
func (f *ResizeV2) MarshalPayload(v hp.Version) []byte {
	var e hp.Enc
	e.U16(f.Cols)
	e.U16(f.Rows)
	if v >= 2 {
		e.U16(f.PixelW)
		e.U16(f.PixelH)
	}
	return e.Bytes()
}

func decodeResizeV2(v hp.Version, h hp.Header, p []byte) (hp.Frame, error) {
	d := hp.NewDec(p)
	f := &ResizeV2{Handle: h.Pane, Token: h.Token, Cols: d.U16(), Rows: d.U16()}
	if v >= 2 {
		f.PixelW, f.PixelH = d.U16(), d.U16()
	}
	return f, d.Err()
}

// OutputV2 is output with a sequence number appended to its fixed part at version 2.
type OutputV2 struct {
	Handle hp.Handle
	Offset uint64
	Seq    uint32
	Data   []byte
}

func (f *OutputV2) Header() hp.Header { return hp.Header{Kind: hp.KindOutput, Pane: f.Handle} }
func (f *OutputV2) MarshalPayload(v hp.Version) []byte {
	var fixed, e hp.Enc
	fixed.U64(f.Offset)
	if v >= 2 {
		fixed.U32(f.Seq)
	}
	e.Fixed(fixed.Bytes())
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeOutputV2(v hp.Version, h hp.Header, p []byte) (hp.Frame, error) {
	d := hp.NewDec(p)
	fx := d.Fixed()
	f := &OutputV2{Handle: h.Pane, Offset: fx.U64()}
	if v >= 2 {
		f.Seq = fx.U32()
	}
	f.Data = d.Rest()
	if fx.Err() != nil {
		return nil, fx.Err()
	}
	return f, d.Err()
}

// HelloV2 is hello with a feature list appended. The handshake is read by every version, so a
// version 1 holder must accept it.
type HelloV2 struct {
	Min, Max hp.Version
	Build    string
	Features []string
}

func (*HelloV2) Header() hp.Header { return hp.Header{Kind: hp.KindHello} }
func (f *HelloV2) MarshalPayload(hp.Version) []byte {
	var e hp.Enc
	e.U16(uint16(f.Min))
	e.U16(uint16(f.Max))
	e.String(f.Build)
	e.Strings(f.Features)
	return e.Bytes()
}

// Stats reports a pane's ring use. Version 2 only.
type Stats struct {
	Handle hp.Handle
	Used   uint64
}

func (f *Stats) Header() hp.Header { return hp.Header{Kind: KindStats, Pane: f.Handle} }
func (f *Stats) MarshalPayload(hp.Version) []byte {
	var e hp.Enc
	e.U64(f.Used)
	return e.Bytes()
}

func decodeStats(_ hp.Version, h hp.Header, p []byte) (hp.Frame, error) {
	d := hp.NewDec(p)
	f := &Stats{Handle: h.Pane, Used: d.U64()}
	return f, d.Err()
}

// v2Registry speaks versions 1 and 2.
func v2Registry() *hp.Registry {
	r := hp.V1().Clone()
	r.Max = 2
	r.Register(hp.KindResize, 1, decodeResizeV2)
	r.Register(hp.KindOutput, 1, decodeOutputV2)
	r.Register(KindStats, 2, decodeStats)
	return r
}

var paneA = hp.UUID{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe}

type sample struct {
	name string
	f    hp.Frame
}

// v1Samples has one frame per version 1 kind, with every field set to a non-zero value where it
// can be, so a field that silently stops being encoded changes the golden bytes.
func v1Samples() []sample {
	return []sample{
		{"hello", &hp.Hello{Min: 1, Max: 1, Build: "gff-hab test-build"}},
		{"welcome", &hp.Welcome{
			Version: 1, Instance: "h-0001", Pid: 4242, Build: "holder test-build", RingSize: 4 << 20,
			Draining: true, Previous: hp.PreviousHandover,
			Panes: []hp.PaneInfo{
				{UUID: paneA, Handle: 1, Pid: 4300, Cols: 120, Rows: 40, Count: 9000, Oldest: 100, InputCount: 55},
				{UUID: hp.UUID{0xff}, Handle: 2, Pid: 4301, Cols: 80, Rows: 24, Count: 12, Oldest: 0, InputCount: 3,
					Exited: true, Exit: hp.ExitStatus{Signaled: true, Signal: hp.SigKILL, HostSignal: 9}},
			},
		}},
		{"refuse", &hp.Refuse{Min: 1, Max: 1, Reason: "no common protocol version"}},
		{"ping", &hp.Ping{Token: 0x01020304}},
		{"pong", &hp.Pong{Token: 0x01020304}},
		{"handover", &hp.Handover{Token: 11}},
		{"drain", &hp.Drain{Token: 12}},
		{"shutdown", &hp.Shutdown{Token: 13}},
		{"bye", &hp.Bye{}},
		{"error", &hp.Error{Handle: 3, Token: 21, Ref: hp.KindInput, Code: hp.CodeInputOffset, Offset: 640,
			Message: "input offset 700, expected 640"}},
		{"spawn", &hp.Spawn{Token: 22, UUID: paneA, Cols: 100, Rows: 30, Cwd: "/work",
			Argv: []string{"sh", "-c", "echo hi"}, Env: []string{"TERM=xterm-256color", "LANG=C.UTF-8"}}},
		{"spawned", &hp.Spawned{Handle: 3, Token: 22, UUID: paneA, Pid: 5000, Existing: true}},
		{"spawn-failed", &hp.SpawnFailed{Token: 22, UUID: paneA, Code: hp.CodeDraining, Message: "holder is draining"}},
		{"input", &hp.Input{Handle: 3, Token: 23, Offset: 640, Data: []byte("ls -l\r")}},
		{"output", &hp.Output{Handle: 3, Offset: 1 << 33, Data: []byte("\x1b[1mhi\x1b[0m\r\n")}},
		{"resize", &hp.Resize{Handle: 3, Token: 24, Cols: 132, Rows: 43}},
		{"signal", &hp.Signal{Handle: 3, Token: 25, Target: hp.TargetForeground, Signal: hp.SigINT}},
		{"foreground-query", &hp.ForegroundQuery{Handle: 3, Token: 26}},
		{"foreground", &hp.Foreground{Handle: 3, Token: 26, Pgid: 5012}},
		{"exited", &hp.Exited{Handle: 3, Status: hp.ExitStatus{Signaled: true, Signal: hp.SigUSR1, HostSignal: 30}, Final: 77777}},
		{"release", &hp.Release{Handle: 3, Token: 27}},
		{"replay", &hp.Replay{Handle: 3, Token: 7, From: 1000, HasTo: true, To: 2000}},
		{"replay-data", &hp.ReplayData{Handle: 3, Token: 7, Offset: 1000, Data: []byte("ring bytes")}},
		{"replay-end", &hp.ReplayEnd{Handle: 3, Token: 7, Start: 1500, End: 2000, Gap: true}},
	}
}

// v2Samples is v1Samples at test version 2: resize gains pixel sizes, output gains a sequence
// number, stats is new, and the handshake frames name version 2.
func v2Samples() []sample {
	var out []sample
	for _, s := range v1Samples() {
		switch f := s.f.(type) {
		case *hp.Resize:
			s.f = &ResizeV2{Handle: f.Handle, Token: f.Token, Cols: f.Cols, Rows: f.Rows, PixelW: 1320, PixelH: 860}
		case *hp.Output:
			s.f = &OutputV2{Handle: f.Handle, Offset: f.Offset, Seq: 99, Data: f.Data}
		case *hp.Hello:
			s.f = &hp.Hello{Min: 1, Max: 2, Build: f.Build}
		case *hp.Welcome:
			w := *f
			w.Version = 2
			s.f = &w
		case *hp.Refuse:
			s.f = &hp.Refuse{Min: 1, Max: 2, Reason: f.Reason}
		}
		out = append(out, s)
	}
	return append(out, sample{"stats", &Stats{Handle: 3, Used: 123456}})
}
