//go:build unix

package holderproto

import "syscall"

var sigHost = map[Sig]syscall.Signal{
	SigHUP: syscall.SIGHUP, SigINT: syscall.SIGINT, SigQUIT: syscall.SIGQUIT,
	SigTERM: syscall.SIGTERM, SigKILL: syscall.SIGKILL, SigWINCH: syscall.SIGWINCH,
	SigCONT: syscall.SIGCONT, SigSTOP: syscall.SIGSTOP, SigUSR1: syscall.SIGUSR1,
	SigUSR2: syscall.SIGUSR2, SigPIPE: syscall.SIGPIPE, SigALRM: syscall.SIGALRM,
	SigSEGV: syscall.SIGSEGV, SigABRT: syscall.SIGABRT, SigBUS: syscall.SIGBUS,
	SigFPE: syscall.SIGFPE, SigILL: syscall.SIGILL, SigTRAP: syscall.SIGTRAP,
	SigSYS: syscall.SIGSYS, SigTSTP: syscall.SIGTSTP, SigTTIN: syscall.SIGTTIN,
	SigTTOU: syscall.SIGTTOU, SigXCPU: syscall.SIGXCPU, SigXFSZ: syscall.SIGXFSZ,
	SigVTALRM: syscall.SIGVTALRM, SigPROF: syscall.SIGPROF,
}

var hostSig = func() map[syscall.Signal]Sig {
	m := make(map[syscall.Signal]Sig, len(sigHost))
	for s, h := range sigHost {
		m[h] = s
	}
	return m
}()

// Host returns this host's number for s, and false when s has none.
func (s Sig) Host() (syscall.Signal, bool) {
	h, ok := sigHost[s]
	return h, ok
}

// SigFromHost returns the protocol number for a host signal, or SigOther.
func SigFromHost(h syscall.Signal) Sig {
	if s, ok := hostSig[h]; ok {
		return s
	}
	return SigOther
}

// ExitFromHost builds an ExitStatus from a host wait status.
func ExitFromHost(ws syscall.WaitStatus) ExitStatus {
	if ws.Signaled() {
		return ExitStatus{Signaled: true, Signal: SigFromHost(ws.Signal()), HostSignal: int32(ws.Signal())}
	}
	return ExitStatus{Code: int32(ws.ExitStatus())}
}
