package holderproto_test

import (
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// Test-only version 2. It proves that one codebase speaks two versions: version 2 appends pixel
// sizes to resize and adds a stats frame kind. Neither exists in the real protocol.

// KindStats is a frame kind that exists only in test version 2.
const KindStats hp.Kind = 0x7f01

// ResizeV2 is resize with pixel sizes appended at version 2.
type ResizeV2 struct {
	Handle         hp.Handle
	Cols, Rows     uint16
	PixelW, PixelH uint16
}

func (*ResizeV2) Kind() hp.Kind     { return hp.KindResize }
func (f *ResizeV2) Pane() hp.Handle { return f.Handle }
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

func decodeResizeV2(v hp.Version, pane hp.Handle, p []byte) (hp.Frame, error) {
	d := hp.NewDec(p)
	f := &ResizeV2{Handle: pane, Cols: d.U16(), Rows: d.U16()}
	if v >= 2 {
		f.PixelW, f.PixelH = d.U16(), d.U16()
	}
	return f, d.Err()
}

// Stats reports a pane's ring use. Version 2 only.
type Stats struct {
	Handle hp.Handle
	Used   uint64
}

func (*Stats) Kind() hp.Kind     { return KindStats }
func (f *Stats) Pane() hp.Handle { return f.Handle }
func (f *Stats) MarshalPayload(hp.Version) []byte {
	var e hp.Enc
	e.U64(f.Used)
	return e.Bytes()
}

func decodeStats(_ hp.Version, pane hp.Handle, p []byte) (hp.Frame, error) {
	d := hp.NewDec(p)
	f := &Stats{Handle: pane, Used: d.U64()}
	return f, d.Err()
}

// v2Registry speaks versions 1 and 2.
func v2Registry() *hp.Registry {
	r := hp.V1().Clone()
	r.Max = 2
	r.Register(hp.KindResize, 1, decodeResizeV2)
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
				{UUID: paneA, Handle: 1, Pid: 4300, Cols: 120, Rows: 40, Count: 9000, Oldest: 100},
				{UUID: hp.UUID{0xff}, Handle: 2, Pid: 4301, Cols: 80, Rows: 24, Count: 12, Oldest: 0,
					Exited: true, Exit: hp.ExitStatus{Signaled: true, Code: 9}},
			},
		}},
		{"refuse", &hp.Refuse{Min: 1, Max: 1, Reason: "no common protocol version"}},
		{"ping", &hp.Ping{Token: 0x0102030405060708}},
		{"pong", &hp.Pong{Token: 0x0102030405060708}},
		{"handover", &hp.Handover{}},
		{"drain", &hp.Drain{}},
		{"shutdown", &hp.Shutdown{}},
		{"bye", &hp.Bye{}},
		{"error", &hp.Error{Handle: 3, Ref: hp.KindInput, Code: hp.CodeQueueFull, Message: "input queue full"}},
		{"spawn", &hp.Spawn{UUID: paneA, Cols: 100, Rows: 30, Cwd: "/work",
			Argv: []string{"sh", "-c", "echo hi"}, Env: []string{"TERM=xterm-256color", "LANG=C.UTF-8"}}},
		{"spawned", &hp.Spawned{Handle: 3, UUID: paneA, Pid: 5000, Existing: true}},
		{"spawn-failed", &hp.SpawnFailed{UUID: paneA, Code: hp.CodeDraining, Message: "holder is draining"}},
		{"input", &hp.Input{Handle: 3, Data: []byte("ls -l\r")}},
		{"output", &hp.Output{Handle: 3, Offset: 1 << 33, Data: []byte("\x1b[1mhi\x1b[0m\r\n")}},
		{"resize", &hp.Resize{Handle: 3, Cols: 132, Rows: 43}},
		{"signal", &hp.Signal{Handle: 3, Target: hp.TargetForeground, Signal: hp.SigINT}},
		{"foreground-query", &hp.ForegroundQuery{Handle: 3}},
		{"foreground", &hp.Foreground{Handle: 3, Pgid: 5012}},
		{"exited", &hp.Exited{Handle: 3, Status: hp.ExitStatus{Code: 2}, Final: 77777}},
		{"release", &hp.Release{Handle: 3}},
		{"replay", &hp.Replay{Handle: 3, Token: 7, From: 1000, HasTo: true, To: 2000}},
		{"replay-data", &hp.ReplayData{Handle: 3, Token: 7, Offset: 1000, Data: []byte("ring bytes")}},
		{"replay-end", &hp.ReplayEnd{Handle: 3, Token: 7, Start: 1500, End: 2000, Gap: true}},
	}
}

// v2Samples is v1Samples at test version 2: resize gains pixel sizes, stats is new, and the
// handshake frames name version 2.
func v2Samples() []sample {
	var out []sample
	for _, s := range v1Samples() {
		switch f := s.f.(type) {
		case *hp.Resize:
			s.f = &ResizeV2{Handle: f.Handle, Cols: f.Cols, Rows: f.Rows, PixelW: 1320, PixelH: 860}
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
