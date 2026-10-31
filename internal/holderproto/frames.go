package holderproto

import "fmt"

// Frame kinds. Handshake and connection kinds sit in 0x00xx, pane kinds in 0x01xx. A kind number
// is never reused for another meaning. Kinds 0x7f00 to 0x7fff are reserved for tests.
const (
	KindHello      Kind = 0x0001
	KindWelcome    Kind = 0x0002
	KindRefuse     Kind = 0x0003
	KindPing       Kind = 0x0004
	KindPong       Kind = 0x0005
	KindHandover   Kind = 0x0006
	KindDrain      Kind = 0x0007
	KindShutdown   Kind = 0x0008
	KindBye        Kind = 0x0009
	KindError      Kind = 0x000a
	KindSpawn      Kind = 0x0100
	KindSpawned    Kind = 0x0101
	KindSpawnFail  Kind = 0x0102
	KindInput      Kind = 0x0103
	KindOutput     Kind = 0x0104
	KindResize     Kind = 0x0105
	KindSignal     Kind = 0x0106
	KindFgQuery    Kind = 0x0107
	KindFg         Kind = 0x0108
	KindExited     Kind = 0x0109
	KindRelease    Kind = 0x010a
	KindReplay     Kind = 0x010b
	KindReplayData Kind = 0x010c
	KindReplayEnd  Kind = 0x010d
)

var kindNames = map[Kind]string{
	KindHello: "hello", KindWelcome: "welcome", KindRefuse: "refuse", KindPing: "ping",
	KindPong: "pong", KindHandover: "handover", KindDrain: "drain", KindShutdown: "shutdown",
	KindBye: "bye", KindError: "error", KindSpawn: "spawn", KindSpawned: "spawned",
	KindSpawnFail: "spawn-failed", KindInput: "input", KindOutput: "output", KindResize: "resize",
	KindSignal: "signal", KindFgQuery: "foreground-query", KindFg: "foreground",
	KindExited: "exited", KindRelease: "release", KindReplay: "replay",
	KindReplayData: "replay-data", KindReplayEnd: "replay-end",
}

// String returns the kind's name, or its number for a kind this build does not know.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("kind-%#04x", uint16(k))
}

// V1 returns a new registry for protocol version 1, the real protocol.
func V1() *Registry {
	r := NewRegistry(Version1, Version1)
	// The handshake is frozen across versions, so it is legal before negotiation.
	r.Register(KindHello, 0, decodeHello)
	r.Register(KindWelcome, 0, decodeWelcome)
	r.Register(KindRefuse, 0, decodeRefuse)
	r.Register(KindPing, 1, decodePing)
	r.Register(KindPong, 1, decodePong)
	r.Register(KindHandover, 1, empty(func(Handle) Frame { return &Handover{} }))
	r.Register(KindDrain, 1, empty(func(Handle) Frame { return &Drain{} }))
	r.Register(KindShutdown, 1, empty(func(Handle) Frame { return &Shutdown{} }))
	r.Register(KindBye, 1, empty(func(Handle) Frame { return &Bye{} }))
	r.Register(KindError, 1, decodeError)
	r.Register(KindSpawn, 1, decodeSpawn)
	r.Register(KindSpawned, 1, decodeSpawned)
	r.Register(KindSpawnFail, 1, decodeSpawnFailed)
	r.Register(KindInput, 1, decodeInput)
	r.Register(KindOutput, 1, decodeOutput)
	r.Register(KindResize, 1, DecodeResize)
	r.Register(KindSignal, 1, decodeSignal)
	r.Register(KindFgQuery, 1, empty(func(h Handle) Frame { return &ForegroundQuery{Handle: h} }))
	r.Register(KindFg, 1, decodeForeground)
	r.Register(KindExited, 1, decodeExited)
	r.Register(KindRelease, 1, empty(func(h Handle) Frame { return &Release{Handle: h} }))
	r.Register(KindReplay, 1, decodeReplay)
	r.Register(KindReplayData, 1, decodeReplayData)
	r.Register(KindReplayEnd, 1, decodeReplayEnd)
	return r
}

// empty decodes a kind with no payload fields.
func empty(mk func(Handle) Frame) DecodeFunc {
	return func(_ Version, pane Handle, _ []byte) (Frame, error) { return mk(pane), nil }
}

// --- Handshake -------------------------------------------------------------------------------

// Hello opens a connection: gff-hab offers a version range and names its build.
type Hello struct {
	Min, Max Version
	Build    string
}

func (*Hello) Kind() Kind   { return KindHello }
func (*Hello) Pane() Handle { return 0 }
func (f *Hello) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Min))
	e.U16(uint16(f.Max))
	e.String(f.Build)
	return e.Bytes()
}

func decodeHello(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Hello{Min: Version(d.U16()), Max: Version(d.U16()), Build: d.String()}
	return f, d.Err()
}

// Previous says how the last logic process left the holder.
type Previous uint8

const (
	PreviousNone     Previous = 0 // no logic process has connected since the holder started
	PreviousHandover Previous = 1 // it sent handover, then closed
	PreviousCrash    Previous = 2 // the connection ended without handover, hung, or was replaced
)

// ExitStatus is how a pane's child ended. Code is the exit code, or, when Signaled, the number of
// the signal that ended it on the holder's host.
type ExitStatus struct {
	Signaled bool
	Code     int32
}

// PaneInfo describes one pane in a welcome.
type PaneInfo struct {
	UUID       UUID
	Handle     Handle
	Pid        uint32
	Cols, Rows uint16
	Count      uint64 // total bytes the pane has produced since spawn
	Oldest     uint64 // offset of the oldest byte still in the ring
	Exited     bool
	Exit       ExitStatus // zero unless Exited
}

func (p *PaneInfo) marshal() []byte {
	var e Enc
	e.UUID(p.UUID)
	e.U32(uint32(p.Handle))
	e.U32(p.Pid)
	e.U16(p.Cols)
	e.U16(p.Rows)
	e.U64(p.Count)
	e.U64(p.Oldest)
	e.Bool(p.Exited)
	e.Bool(p.Exit.Signaled)
	e.I32(p.Exit.Code)
	return e.Bytes()
}

func decodePaneInfo(p []byte) (PaneInfo, error) {
	d := NewDec(p)
	pi := PaneInfo{
		UUID: d.UUID(), Handle: Handle(d.U32()), Pid: d.U32(), Cols: d.U16(), Rows: d.U16(),
		Count: d.U64(), Oldest: d.U64(), Exited: d.Bool(),
		Exit: ExitStatus{Signaled: d.Bool(), Code: d.I32()},
	}
	return pi, d.Err()
}

// Welcome answers a hello. It carries the chosen version, facts about the holder, and every pane.
// No output follows on its own; gff-hab asks for each pane's bytes with Replay.
type Welcome struct {
	Version  Version
	Instance string
	Pid      uint32
	Build    string
	RingSize uint64
	Draining bool
	Previous Previous
	Panes    []PaneInfo
}

func (*Welcome) Kind() Kind   { return KindWelcome }
func (*Welcome) Pane() Handle { return 0 }

// MarshalPayload ignores its argument: the welcome is laid out at its own Version field.
func (f *Welcome) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Version))
	e.String(f.Instance)
	e.U32(f.Pid)
	e.String(f.Build)
	e.U64(f.RingSize)
	e.Bool(f.Draining)
	e.U8(uint8(f.Previous))
	e.U32(uint32(len(f.Panes)))
	for i := range f.Panes {
		e.Blob(f.Panes[i].marshal())
	}
	return e.Bytes()
}

func decodeWelcome(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Welcome{
		Version: Version(d.U16()), Instance: d.String(), Pid: d.U32(), Build: d.String(),
		RingSize: d.U64(), Draining: d.Bool(), Previous: Previous(d.U8()),
	}
	n := d.U32()
	if d.Err() == nil && uint64(n)*4 > uint64(d.Len()) {
		return nil, ErrShortPayload
	}
	for i := uint32(0); i < n && d.Err() == nil; i++ {
		rec := d.Blob()
		if d.Err() != nil {
			break
		}
		pi, err := decodePaneInfo(rec)
		if err != nil {
			return nil, err
		}
		f.Panes = append(f.Panes, pi)
	}
	return f, d.Err()
}

// Refuse answers a hello when the two sides share no version. The holder closes after it.
type Refuse struct {
	Min, Max Version // the holder's range
	Reason   string
}

func (*Refuse) Kind() Kind   { return KindRefuse }
func (*Refuse) Pane() Handle { return 0 }
func (f *Refuse) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Min))
	e.U16(uint16(f.Max))
	e.String(f.Reason)
	return e.Bytes()
}

func decodeRefuse(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Refuse{Min: Version(d.U16()), Max: Version(d.U16()), Reason: d.String()}
	return f, d.Err()
}

// --- Connection ------------------------------------------------------------------------------

// Ping asks the peer to answer with a Pong carrying the same token. Either side may send it.
type Ping struct{ Token uint64 }

func (*Ping) Kind() Kind   { return KindPing }
func (*Ping) Pane() Handle { return 0 }
func (f *Ping) MarshalPayload(Version) []byte {
	var e Enc
	e.U64(f.Token)
	return e.Bytes()
}

func decodePing(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Ping{Token: d.U64()}
	return f, d.Err()
}

// Pong answers a Ping.
type Pong struct{ Token uint64 }

func (*Pong) Kind() Kind   { return KindPong }
func (*Pong) Pane() Handle { return 0 }
func (f *Pong) MarshalPayload(Version) []byte {
	var e Enc
	e.U64(f.Token)
	return e.Bytes()
}

func decodePong(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Pong{Token: d.U64()}
	return f, d.Err()
}

// Handover tells the holder that gff-hab saved its screens and will close on purpose.
type Handover struct{}

func (*Handover) Kind() Kind                    { return KindHandover }
func (*Handover) Pane() Handle                  { return 0 }
func (*Handover) MarshalPayload(Version) []byte { return nil }

// Drain tells the holder to refuse new spawns and exit after its last pane is released.
type Drain struct{}

func (*Drain) Kind() Kind                    { return KindDrain }
func (*Drain) Pane() Handle                  { return 0 }
func (*Drain) MarshalPayload(Version) []byte { return nil }

// Shutdown tells the holder to end every pane and exit.
type Shutdown struct{}

func (*Shutdown) Kind() Kind                    { return KindShutdown }
func (*Shutdown) Pane() Handle                  { return 0 }
func (*Shutdown) MarshalPayload(Version) []byte { return nil }

// Bye is the last frame the holder sends before it closes the connection.
type Bye struct{}

func (*Bye) Kind() Kind                    { return KindBye }
func (*Bye) Pane() Handle                  { return 0 }
func (*Bye) MarshalPayload(Version) []byte { return nil }

// ErrorCode classifies an Error or SpawnFailed frame. Receivers treat an unknown code as
// CodeUnknown.
type ErrorCode uint16

const (
	CodeUnknown    ErrorCode = 0 // anything else; see the message
	CodeQueueFull  ErrorCode = 1 // the pane's input queue is full
	CodeNoPane     ErrorCode = 2 // no pane has this handle
	CodeDraining   ErrorCode = 3 // the holder is draining and takes no new panes
	CodeBadRequest ErrorCode = 4 // a field is out of range, or the request makes no sense now
	CodeExited     ErrorCode = 5 // the pane's child was already reaped
	CodeStart      ErrorCode = 6 // the program could not start
)

// Error reports that the holder refused or failed a request. Ref is the kind of that request;
// the header's pane handle names the pane, or is 0. "Input refused" is an Error with Ref
// KindInput.
type Error struct {
	Handle  Handle
	Ref     Kind
	Code    ErrorCode
	Message string
}

func (*Error) Kind() Kind     { return KindError }
func (f *Error) Pane() Handle { return f.Handle }
func (f *Error) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Ref))
	e.U16(uint16(f.Code))
	e.String(f.Message)
	return e.Bytes()
}

func decodeError(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Error{Handle: pane, Ref: Kind(d.U16()), Code: ErrorCode(d.U16()), Message: d.String()}
	return f, d.Err()
}

// --- Panes -----------------------------------------------------------------------------------

// Spawn asks the holder to start a program in a new pane. The header handle is 0.
type Spawn struct {
	UUID       UUID
	Cols, Rows uint16
	Cwd        string
	Argv       []string
	Env        []string // the complete environment, as KEY=value
}

func (*Spawn) Kind() Kind   { return KindSpawn }
func (*Spawn) Pane() Handle { return 0 }
func (f *Spawn) MarshalPayload(Version) []byte {
	var e Enc
	e.UUID(f.UUID)
	e.U16(f.Cols)
	e.U16(f.Rows)
	e.String(f.Cwd)
	e.Strings(f.Argv)
	e.Strings(f.Env)
	return e.Bytes()
}

func decodeSpawn(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Spawn{UUID: d.UUID(), Cols: d.U16(), Rows: d.U16(), Cwd: d.String(), Argv: d.Strings(), Env: d.Strings()}
	return f, d.Err()
}

// Spawned answers a spawn that succeeded. The header handle is the new pane's handle. Existing is
// true when the holder already ran a pane with this uuid and answered with it.
type Spawned struct {
	Handle   Handle
	UUID     UUID
	Pid      uint32
	Existing bool
}

func (*Spawned) Kind() Kind     { return KindSpawned }
func (f *Spawned) Pane() Handle { return f.Handle }
func (f *Spawned) MarshalPayload(Version) []byte {
	var e Enc
	e.UUID(f.UUID)
	e.U32(f.Pid)
	e.Bool(f.Existing)
	return e.Bytes()
}

func decodeSpawned(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Spawned{Handle: pane, UUID: d.UUID(), Pid: d.U32(), Existing: d.Bool()}
	return f, d.Err()
}

// SpawnFailed answers a spawn that created no pane. The header handle is 0.
type SpawnFailed struct {
	UUID    UUID
	Code    ErrorCode
	Message string
}

func (*SpawnFailed) Kind() Kind   { return KindSpawnFail }
func (*SpawnFailed) Pane() Handle { return 0 }
func (f *SpawnFailed) MarshalPayload(Version) []byte {
	var e Enc
	e.UUID(f.UUID)
	e.U16(uint16(f.Code))
	e.String(f.Message)
	return e.Bytes()
}

func decodeSpawnFailed(_ Version, _ Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &SpawnFailed{UUID: d.UUID(), Code: ErrorCode(d.U16()), Message: d.String()}
	return f, d.Err()
}

// Input carries bytes to write to a pane's PTY. The payload is the bytes, nothing else.
type Input struct {
	Handle Handle
	Data   []byte
}

func (*Input) Kind() Kind                      { return KindInput }
func (f *Input) Pane() Handle                  { return f.Handle }
func (f *Input) MarshalPayload(Version) []byte { return f.Data }

func decodeInput(_ Version, pane Handle, p []byte) (Frame, error) {
	return &Input{Handle: pane, Data: p}, nil
}

// Output carries live bytes a pane produced. Offset is the pane's byte offset of Data[0].
type Output struct {
	Handle Handle
	Offset uint64
	Data   []byte
}

func (*Output) Kind() Kind     { return KindOutput }
func (f *Output) Pane() Handle { return f.Handle }
func (f *Output) MarshalPayload(Version) []byte {
	var e Enc
	e.U64(f.Offset)
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeOutput(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Output{Handle: pane, Offset: d.U64(), Data: d.Rest()}
	return f, d.Err()
}

// Resize sets a pane's size in cells.
type Resize struct {
	Handle     Handle
	Cols, Rows uint16
}

func (*Resize) Kind() Kind     { return KindResize }
func (f *Resize) Pane() Handle { return f.Handle }
func (f *Resize) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(f.Cols)
	e.U16(f.Rows)
	return e.Bytes()
}

// DecodeResize decodes a version 1 resize. It is exported so a later version can wrap it.
func DecodeResize(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Resize{Handle: pane, Cols: d.U16(), Rows: d.U16()}
	return f, d.Err()
}

// Target picks which processes a signal reaches.
type Target uint8

const (
	TargetChild      Target = 0 // the pane's child, the session leader
	TargetForeground Target = 1 // the PTY's foreground process group
)

// Sig is a signal in the protocol's own numbering. The holder maps it to the host's number, which
// differs between Linux and macOS for some signals.
type Sig uint8

const (
	SigHUP   Sig = 1
	SigINT   Sig = 2
	SigQUIT  Sig = 3
	SigTERM  Sig = 4
	SigKILL  Sig = 5
	SigWINCH Sig = 6
	SigCONT  Sig = 7
	SigSTOP  Sig = 8
	SigUSR1  Sig = 9
	SigUSR2  Sig = 10
)

// Signal sends a signal to a pane's child or foreground group.
type Signal struct {
	Handle Handle
	Target Target
	Signal Sig
}

func (*Signal) Kind() Kind     { return KindSignal }
func (f *Signal) Pane() Handle { return f.Handle }
func (f *Signal) MarshalPayload(Version) []byte {
	var e Enc
	e.U8(uint8(f.Target))
	e.U8(uint8(f.Signal))
	return e.Bytes()
}

func decodeSignal(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Signal{Handle: pane, Target: Target(d.U8()), Signal: Sig(d.U8())}
	return f, d.Err()
}

// ForegroundQuery asks for a pane's foreground process group.
type ForegroundQuery struct{ Handle Handle }

func (*ForegroundQuery) Kind() Kind                    { return KindFgQuery }
func (f *ForegroundQuery) Pane() Handle                { return f.Handle }
func (*ForegroundQuery) MarshalPayload(Version) []byte { return nil }

// Foreground answers a ForegroundQuery. Pgid 0 means the PTY has no foreground group.
type Foreground struct {
	Handle Handle
	Pgid   int32
}

func (*Foreground) Kind() Kind     { return KindFg }
func (f *Foreground) Pane() Handle { return f.Handle }
func (f *Foreground) MarshalPayload(Version) []byte {
	var e Enc
	e.I32(f.Pgid)
	return e.Bytes()
}

func decodeForeground(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Foreground{Handle: pane, Pgid: d.I32()}
	return f, d.Err()
}

// Exited reports that a pane's child was reaped. Final is the pane's byte count when the frame
// was sent: gff-hab holds all output once it holds every byte before Final.
type Exited struct {
	Handle Handle
	Status ExitStatus
	Final  uint64
}

func (*Exited) Kind() Kind     { return KindExited }
func (f *Exited) Pane() Handle { return f.Handle }
func (f *Exited) MarshalPayload(Version) []byte {
	var e Enc
	e.Bool(f.Status.Signaled)
	e.I32(f.Status.Code)
	e.U64(f.Final)
	return e.Bytes()
}

func decodeExited(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Exited{Handle: pane, Status: ExitStatus{Signaled: d.Bool(), Code: d.I32()}, Final: d.U64()}
	return f, d.Err()
}

// Release tells the holder to drop a pane: hang up its session if it still runs, and free its
// ring.
type Release struct{ Handle Handle }

func (*Release) Kind() Kind                    { return KindRelease }
func (f *Release) Pane() Handle                { return f.Handle }
func (*Release) MarshalPayload(Version) []byte { return nil }

// Replay asks for a pane's ring bytes in [From, To). With HasTo false the range is open: the
// holder sends every byte from From up to the pane's count, then live Output from there on.
// Token is chosen by gff-hab and comes back on every ReplayData and the ReplayEnd.
type Replay struct {
	Handle Handle
	Token  uint32
	From   uint64
	HasTo  bool
	To     uint64
}

func (*Replay) Kind() Kind     { return KindReplay }
func (f *Replay) Pane() Handle { return f.Handle }
func (f *Replay) MarshalPayload(Version) []byte {
	var e Enc
	e.U32(f.Token)
	e.U64(f.From)
	e.Bool(f.HasTo)
	e.U64(f.To)
	return e.Bytes()
}

func decodeReplay(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Replay{Handle: pane, Token: d.U32(), From: d.U64(), HasTo: d.Bool(), To: d.U64()}
	return f, d.Err()
}

// ReplayData carries ring bytes for a Replay. Offset is the pane's byte offset of Data[0].
type ReplayData struct {
	Handle Handle
	Token  uint32
	Offset uint64
	Data   []byte
}

func (*ReplayData) Kind() Kind     { return KindReplayData }
func (f *ReplayData) Pane() Handle { return f.Handle }
func (f *ReplayData) MarshalPayload(Version) []byte {
	var e Enc
	e.U32(f.Token)
	e.U64(f.Offset)
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeReplayData(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &ReplayData{Handle: pane, Token: d.U32(), Offset: d.U64(), Data: d.Rest()}
	return f, d.Err()
}

// ReplayEnd closes a Replay. The holder sent exactly the bytes in [Start, End). Gap is true when
// Start is later than the requested From because those bytes had left the ring.
type ReplayEnd struct {
	Handle Handle
	Token  uint32
	Start  uint64
	End    uint64
	Gap    bool
}

func (*ReplayEnd) Kind() Kind     { return KindReplayEnd }
func (f *ReplayEnd) Pane() Handle { return f.Handle }
func (f *ReplayEnd) MarshalPayload(Version) []byte {
	var e Enc
	e.U32(f.Token)
	e.U64(f.Start)
	e.U64(f.End)
	e.Bool(f.Gap)
	return e.Bytes()
}

func decodeReplayEnd(_ Version, pane Handle, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &ReplayEnd{Handle: pane, Token: d.U32(), Start: d.U64(), End: d.U64(), Gap: d.Bool()}
	return f, d.Err()
}
