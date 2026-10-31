package holderproto

// Sig is a signal in the protocol's own numbering. Linux and macOS number some signals
// differently (USR1 is 10 on Linux and 30 on macOS), so no host number crosses the wire as a
// signal identity. The holder maps with SigFromHost and Sig.Host.
type Sig uint8

const (
	SigHUP    Sig = 1
	SigINT    Sig = 2
	SigQUIT   Sig = 3
	SigTERM   Sig = 4
	SigKILL   Sig = 5
	SigWINCH  Sig = 6
	SigCONT   Sig = 7
	SigSTOP   Sig = 8
	SigUSR1   Sig = 9
	SigUSR2   Sig = 10
	SigPIPE   Sig = 11
	SigALRM   Sig = 12
	SigSEGV   Sig = 13
	SigABRT   Sig = 14
	SigBUS    Sig = 15
	SigFPE    Sig = 16
	SigILL    Sig = 17
	SigTRAP   Sig = 18
	SigSYS    Sig = 19
	SigTSTP   Sig = 20
	SigTTIN   Sig = 21
	SigTTOU   Sig = 22
	SigXCPU   Sig = 23
	SigXFSZ   Sig = 24
	SigVTALRM Sig = 25
	SigPROF   Sig = 26
	// SigOther marks an exit by a signal the protocol has no number for. It is never sent in a
	// Signal frame.
	SigOther Sig = 255
)

// Sendable reports whether gff-hab may send s in a Signal frame (spec 2.5).
func (s Sig) Sendable() bool { return s >= SigHUP && s <= SigUSR2 }
