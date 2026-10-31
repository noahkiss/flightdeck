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
	r.Register(KindPing, 1, empty(func(h Header) Frame { return &Ping{Token: h.Token} }))
	r.Register(KindPong, 1, empty(func(h Header) Frame { return &Pong{Token: h.Token} }))
	r.Register(KindHandover, 1, empty(func(h Header) Frame { return &Handover{Token: h.Token} }))
	r.Register(KindDrain, 1, empty(func(h Header) Frame { return &Drain{Token: h.Token} }))
	r.Register(KindShutdown, 1, empty(func(h Header) Frame { return &Shutdown{Token: h.Token} }))
	r.Register(KindBye, 1, empty(func(Header) Frame { return &Bye{} }))
	r.Register(KindError, 1, decodeError)
	r.Register(KindSpawn, 1, decodeSpawn)
	r.Register(KindSpawned, 1, decodeSpawned)
	r.Register(KindSpawnFail, 1, decodeSpawnFailed)
	r.Register(KindInput, 1, decodeInput)
	r.Register(KindOutput, 1, decodeOutput)
	r.Register(KindResize, 1, DecodeResize)
	r.Register(KindSignal, 1, decodeSignal)
	r.Register(KindFgQuery, 1, empty(func(h Header) Frame { return &ForegroundQuery{Handle: h.Pane, Token: h.Token} }))
	r.Register(KindFg, 1, decodeForeground)
	r.Register(KindExited, 1, decodeExited)
	r.Register(KindRelease, 1, empty(func(h Header) Frame { return &Release{Handle: h.Pane, Token: h.Token} }))
	r.Register(KindReplay, 1, decodeReplay)
	r.Register(KindReplayData, 1, decodeReplayData)
	r.Register(KindReplayEnd, 1, decodeReplayEnd)
	return r
}

// empty decodes a kind with no payload fields.
func empty(mk func(Header) Frame) DecodeFunc {
	return func(_ Version, h Header, _ []byte) (Frame, error) { return mk(h), nil }
}

// firstErr returns the first non-nil decoder error.
func firstErr(ds ...*Dec) error {
	for _, d := range ds {
		if d.Err() != nil {
			return d.Err()
		}
	}
	return nil
}

// --- Handshake -------------------------------------------------------------------------------

// Hello opens a connection: gff-hab offers a version range and names its build.
type Hello struct {
	Min, Max Version
	Build    string
}

func (*Hello) Header() Header { return Header{Kind: KindHello} }
func (f *Hello) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Min))
	e.U16(uint16(f.Max))
	e.String(f.Build)
	return e.Bytes()
}

func decodeHello(_ Version, _ Header, p []byte) (Frame, error) {
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

// ExitStatus is how a pane's child ended.
type ExitStatus struct {
	// Signaled is true when a signal ended the child.
	Signaled bool
	// Code is the exit code. It is 0 when Signaled.
	Code int32
	// Signal is the ending signal in protocol numbers, SigOther when the protocol has no number
	// for it, and 0 when not Signaled.
	Signal Sig
	// HostSignal is the ending signal as the holder's host numbers it, for logs only. Signal
	// numbers differ between Linux and macOS; compare Signal, never HostSignal.
	HostSignal int32
}

func (s *ExitStatus) marshal(e *Enc) {
	e.Bool(s.Signaled)
	e.I32(s.Code)
	e.U8(uint8(s.Signal))
	e.I32(s.HostSignal)
}

func decodeExitStatus(d *Dec) ExitStatus {
	return ExitStatus{Signaled: d.Bool(), Code: d.I32(), Signal: Sig(d.U8()), HostSignal: d.I32()}
}

// PaneInfo describes one pane in a welcome.
type PaneInfo struct {
	UUID       UUID
	Handle     Handle
	Pid        uint32
	Cols, Rows uint16
	Count      uint64 // total bytes the pane has produced since spawn
	Oldest     uint64 // offset of the oldest byte still in the ring
	InputCount uint64 // total input bytes the holder has accepted for the pane
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
	e.U64(p.InputCount)
	e.Bool(p.Exited)
	p.Exit.marshal(&e)
	return e.Bytes()
}

func decodePaneInfo(p []byte) (PaneInfo, error) {
	d := NewDec(p)
	pi := PaneInfo{
		UUID: d.UUID(), Handle: Handle(d.U32()), Pid: d.U32(), Cols: d.U16(), Rows: d.U16(),
		Count: d.U64(), Oldest: d.U64(), InputCount: d.U64(), Exited: d.Bool(),
	}
	pi.Exit = decodeExitStatus(d)
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

func (*Welcome) Header() Header { return Header{Kind: KindWelcome} }

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

func decodeWelcome(_ Version, _ Header, p []byte) (Frame, error) {
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

func (*Refuse) Header() Header { return Header{Kind: KindRefuse} }
func (f *Refuse) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Min))
	e.U16(uint16(f.Max))
	e.String(f.Reason)
	return e.Bytes()
}

func decodeRefuse(_ Version, _ Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Refuse{Min: Version(d.U16()), Max: Version(d.U16()), Reason: d.String()}
	return f, d.Err()
}

// --- Connection ------------------------------------------------------------------------------

// Ping asks the peer to answer with a Pong carrying the same token. Either side may send it.
type Ping struct{ Token uint32 }

func (f *Ping) Header() Header              { return Header{Kind: KindPing, Token: f.Token} }
func (*Ping) MarshalPayload(Version) []byte { return nil }

// Pong answers a Ping.
type Pong struct{ Token uint32 }

func (f *Pong) Header() Header              { return Header{Kind: KindPong, Token: f.Token} }
func (*Pong) MarshalPayload(Version) []byte { return nil }

// Handover tells the holder that gff-hab saved its screens and will close on purpose.
type Handover struct{ Token uint32 }

func (f *Handover) Header() Header              { return Header{Kind: KindHandover, Token: f.Token} }
func (*Handover) MarshalPayload(Version) []byte { return nil }

// Drain tells the holder to refuse new spawns and exit after its last pane is released.
type Drain struct{ Token uint32 }

func (f *Drain) Header() Header              { return Header{Kind: KindDrain, Token: f.Token} }
func (*Drain) MarshalPayload(Version) []byte { return nil }

// Shutdown tells the holder to end every pane and exit.
type Shutdown struct{ Token uint32 }

func (f *Shutdown) Header() Header              { return Header{Kind: KindShutdown, Token: f.Token} }
func (*Shutdown) MarshalPayload(Version) []byte { return nil }

// Bye is the last frame the holder sends before it closes the connection.
type Bye struct{}

func (*Bye) Header() Header                { return Header{Kind: KindBye} }
func (*Bye) MarshalPayload(Version) []byte { return nil }

// ErrorCode classifies an Error or SpawnFailed frame. Receivers treat an unknown code as
// CodeUnknown.
type ErrorCode uint16

const (
	CodeUnknown     ErrorCode = 0 // anything else; see the message
	CodeQueueFull   ErrorCode = 1 // the pane's input queue cannot take the whole input frame
	CodeNoPane      ErrorCode = 2 // no pane has this handle
	CodeDraining    ErrorCode = 3 // the holder is draining and takes no new panes
	CodeBadRequest  ErrorCode = 4 // a field is out of range, or the request makes no sense now
	CodeExited      ErrorCode = 5 // the pane's child was already reaped
	CodeStart       ErrorCode = 6 // the program could not start
	CodeUnknownKind ErrorCode = 7 // the holder skipped the request: unknown kind, or newer than the version
	CodeInputOffset ErrorCode = 8 // the input frame's offset is not the pane's input count
)

// Error reports that the holder refused or failed a request. The header carries the request's
// pane handle (or 0) and its token. Ref is the request's kind. For a refused input, Offset is the
// input offset the holder expects next: gff-hab resends from there.
type Error struct {
	Handle  Handle
	Token   uint32
	Ref     Kind
	Code    ErrorCode
	Offset  uint64
	Message string
}

func (f *Error) Header() Header { return Header{Kind: KindError, Pane: f.Handle, Token: f.Token} }
func (f *Error) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(uint16(f.Ref))
	e.U16(uint16(f.Code))
	e.U64(f.Offset)
	e.String(f.Message)
	return e.Bytes()
}

func decodeError(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Error{Handle: h.Pane, Token: h.Token, Ref: Kind(d.U16()), Code: ErrorCode(d.U16()),
		Offset: d.U64(), Message: d.String()}
	return f, d.Err()
}

// UnknownKind returns the error the holder sends for a request it skipped (Conn.OnSkip).
func UnknownKind(h Header) *Error {
	return &Error{Handle: h.Pane, Token: h.Token, Ref: h.Kind, Code: CodeUnknownKind,
		Message: "unknown frame kind " + h.Kind.String()}
}

// --- Panes -----------------------------------------------------------------------------------

// Spawn asks the holder to start a program in a new pane. The header handle is 0.
type Spawn struct {
	Token      uint32
	UUID       UUID
	Cols, Rows uint16
	Cwd        string
	Argv       []string
	Env        []string // the complete environment, as KEY=value
}

func (f *Spawn) Header() Header { return Header{Kind: KindSpawn, Token: f.Token} }
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

func decodeSpawn(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Spawn{Token: h.Token, UUID: d.UUID(), Cols: d.U16(), Rows: d.U16(), Cwd: d.String(),
		Argv: d.Strings(), Env: d.Strings()}
	return f, d.Err()
}

// Spawned answers a spawn that succeeded. The header handle is the new pane's handle. Existing is
// true when the holder already ran a pane with this uuid and answered with it.
type Spawned struct {
	Handle   Handle
	Token    uint32
	UUID     UUID
	Pid      uint32
	Existing bool
}

func (f *Spawned) Header() Header {
	return Header{Kind: KindSpawned, Pane: f.Handle, Token: f.Token}
}
func (f *Spawned) MarshalPayload(Version) []byte {
	var e Enc
	e.UUID(f.UUID)
	e.U32(f.Pid)
	e.Bool(f.Existing)
	return e.Bytes()
}

func decodeSpawned(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Spawned{Handle: h.Pane, Token: h.Token, UUID: d.UUID(), Pid: d.U32(), Existing: d.Bool()}
	return f, d.Err()
}

// SpawnFailed answers a spawn that created no pane. The header handle is 0.
type SpawnFailed struct {
	Token   uint32
	UUID    UUID
	Code    ErrorCode
	Message string
}

func (f *SpawnFailed) Header() Header { return Header{Kind: KindSpawnFail, Token: f.Token} }
func (f *SpawnFailed) MarshalPayload(Version) []byte {
	var e Enc
	e.UUID(f.UUID)
	e.U16(uint16(f.Code))
	e.String(f.Message)
	return e.Bytes()
}

func decodeSpawnFailed(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &SpawnFailed{Token: h.Token, UUID: d.UUID(), Code: ErrorCode(d.U16()), Message: d.String()}
	return f, d.Err()
}

// Input carries bytes to write to a pane's PTY. Offset is the pane's input count before Data:
// the holder accepts the frame only when Offset equals the input bytes it has accepted so far,
// and then takes all of Data or none of it.
type Input struct {
	Handle Handle
	Token  uint32
	Offset uint64
	Data   []byte
}

func (f *Input) Header() Header { return Header{Kind: KindInput, Pane: f.Handle, Token: f.Token} }
func (f *Input) validate() error {
	if len(f.Data) > MaxInputData {
		return ErrTooLarge
	}
	return nil
}
func (f *Input) MarshalPayload(Version) []byte {
	var fixed, e Enc
	fixed.U64(f.Offset)
	e.Fixed(fixed.Bytes())
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeInput(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	fx := d.Fixed()
	f := &Input{Handle: h.Pane, Token: h.Token, Offset: fx.U64()}
	f.Data = d.Rest()
	return f, firstErr(d, fx)
}

// Output carries live bytes a pane produced. Offset is the pane's byte offset of Data[0].
type Output struct {
	Handle Handle
	Offset uint64
	Data   []byte
}

func (f *Output) Header() Header { return Header{Kind: KindOutput, Pane: f.Handle} }
func (f *Output) validate() error {
	if len(f.Data) > MaxOutputData {
		return ErrTooLarge
	}
	return nil
}
func (f *Output) MarshalPayload(Version) []byte {
	var fixed, e Enc
	fixed.U64(f.Offset)
	e.Fixed(fixed.Bytes())
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeOutput(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	fx := d.Fixed()
	f := &Output{Handle: h.Pane, Offset: fx.U64()}
	f.Data = d.Rest()
	return f, firstErr(d, fx)
}

// Resize sets a pane's size in cells.
type Resize struct {
	Handle     Handle
	Token      uint32
	Cols, Rows uint16
}

func (f *Resize) Header() Header { return Header{Kind: KindResize, Pane: f.Handle, Token: f.Token} }
func (f *Resize) MarshalPayload(Version) []byte {
	var e Enc
	e.U16(f.Cols)
	e.U16(f.Rows)
	return e.Bytes()
}

// DecodeResize decodes a version 1 resize. It is exported so a later version can wrap it.
func DecodeResize(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Resize{Handle: h.Pane, Token: h.Token, Cols: d.U16(), Rows: d.U16()}
	return f, d.Err()
}

// Target picks which processes a signal reaches.
type Target uint8

const (
	TargetChild      Target = 0 // the pane's child, the session leader
	TargetForeground Target = 1 // the PTY's foreground process group
)

// Signal sends a signal to a pane's child or foreground group. Only the signals in Sendable are
// allowed; the holder answers any other with CodeBadRequest.
type Signal struct {
	Handle Handle
	Token  uint32
	Target Target
	Signal Sig
}

func (f *Signal) Header() Header { return Header{Kind: KindSignal, Pane: f.Handle, Token: f.Token} }
func (f *Signal) MarshalPayload(Version) []byte {
	var e Enc
	e.U8(uint8(f.Target))
	e.U8(uint8(f.Signal))
	return e.Bytes()
}

func decodeSignal(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Signal{Handle: h.Pane, Token: h.Token, Target: Target(d.U8()), Signal: Sig(d.U8())}
	return f, d.Err()
}

// ForegroundQuery asks for a pane's foreground process group.
type ForegroundQuery struct {
	Handle Handle
	Token  uint32
}

func (f *ForegroundQuery) Header() Header {
	return Header{Kind: KindFgQuery, Pane: f.Handle, Token: f.Token}
}
func (*ForegroundQuery) MarshalPayload(Version) []byte { return nil }

// Foreground answers a ForegroundQuery. Pgid 0 means the PTY has no foreground group.
type Foreground struct {
	Handle Handle
	Token  uint32
	Pgid   int32
}

func (f *Foreground) Header() Header { return Header{Kind: KindFg, Pane: f.Handle, Token: f.Token} }
func (f *Foreground) MarshalPayload(Version) []byte {
	var e Enc
	e.I32(f.Pgid)
	return e.Bytes()
}

func decodeForeground(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Foreground{Handle: h.Pane, Token: h.Token, Pgid: d.I32()}
	return f, d.Err()
}

// Exited reports that a pane's child was reaped. The holder sends it for every pane whose child
// is reaped while a logic process is connected. Final is the pane's byte count when the frame
// was sent: gff-hab holds all output once it holds every byte before Final.
type Exited struct {
	Handle Handle
	Status ExitStatus
	Final  uint64
}

func (f *Exited) Header() Header { return Header{Kind: KindExited, Pane: f.Handle} }
func (f *Exited) MarshalPayload(Version) []byte {
	var e Enc
	f.Status.marshal(&e)
	e.U64(f.Final)
	return e.Bytes()
}

func decodeExited(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Exited{Handle: h.Pane, Status: decodeExitStatus(d), Final: d.U64()}
	return f, d.Err()
}

// Release tells the holder to drop a pane: hang up its session if it still runs, and free its
// ring.
type Release struct {
	Handle Handle
	Token  uint32
}

func (f *Release) Header() Header {
	return Header{Kind: KindRelease, Pane: f.Handle, Token: f.Token}
}
func (*Release) MarshalPayload(Version) []byte { return nil }

// Replay asks for a pane's ring bytes in [From, To). With HasTo false the range is open: the
// holder sends every byte from From up to the pane's count, then live Output from there on.
// Token, from the header, comes back on every ReplayData and the ReplayEnd.
type Replay struct {
	Handle Handle
	Token  uint32
	From   uint64
	HasTo  bool
	To     uint64
}

func (f *Replay) Header() Header { return Header{Kind: KindReplay, Pane: f.Handle, Token: f.Token} }
func (f *Replay) MarshalPayload(Version) []byte {
	var e Enc
	e.U64(f.From)
	e.Bool(f.HasTo)
	e.U64(f.To)
	return e.Bytes()
}

func decodeReplay(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &Replay{Handle: h.Pane, Token: h.Token, From: d.U64(), HasTo: d.Bool(), To: d.U64()}
	return f, d.Err()
}

// ReplayData carries ring bytes for a Replay. Offset is the pane's byte offset of Data[0].
type ReplayData struct {
	Handle Handle
	Token  uint32
	Offset uint64
	Data   []byte
}

func (f *ReplayData) Header() Header {
	return Header{Kind: KindReplayData, Pane: f.Handle, Token: f.Token}
}
func (f *ReplayData) validate() error {
	if len(f.Data) > MaxOutputData {
		return ErrTooLarge
	}
	return nil
}
func (f *ReplayData) MarshalPayload(Version) []byte {
	var fixed, e Enc
	fixed.U64(f.Offset)
	e.Fixed(fixed.Bytes())
	e.Raw(f.Data)
	return e.Bytes()
}

func decodeReplayData(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	fx := d.Fixed()
	f := &ReplayData{Handle: h.Pane, Token: h.Token, Offset: fx.U64()}
	f.Data = d.Rest()
	return f, firstErr(d, fx)
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

func (f *ReplayEnd) Header() Header {
	return Header{Kind: KindReplayEnd, Pane: f.Handle, Token: f.Token}
}
func (f *ReplayEnd) MarshalPayload(Version) []byte {
	var e Enc
	e.U64(f.Start)
	e.U64(f.End)
	e.Bool(f.Gap)
	return e.Bytes()
}

func decodeReplayEnd(_ Version, h Header, p []byte) (Frame, error) {
	d := NewDec(p)
	f := &ReplayEnd{Handle: h.Pane, Token: h.Token, Start: d.U64(), End: d.U64(), Gap: d.Bool()}
	return f, d.Err()
}
