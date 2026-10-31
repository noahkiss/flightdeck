//go:build unix

package holderproto_test

import (
	"syscall"
	"testing"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// TestExitSignals: an exit by signal carries the protocol number, so a receiver never mistakes a
// host number for a protocol one (protocol 9 is USR1; host 9 is KILL).
func TestExitSignals(t *testing.T) {
	cases := []struct {
		host syscall.Signal
		want hp.Sig
	}{
		{syscall.SIGKILL, hp.SigKILL},
		{syscall.SIGUSR1, hp.SigUSR1},
		{syscall.SIGSEGV, hp.SigSEGV},
		{syscall.SIGTERM, hp.SigTERM},
	}
	for _, c := range cases {
		// On Linux and macOS, a wait status for a signal death holds the signal in its low bits.
		st := hp.ExitFromHost(syscall.WaitStatus(c.host))
		if !st.Signaled || st.Signal != c.want || st.HostSignal != int32(c.host) || st.Code != 0 {
			t.Errorf("%v: %+v, want signal %d", c.host, st, c.want)
		}
		if h, ok := c.want.Host(); !ok || h != c.host {
			t.Errorf("%d.Host() = %v, %v", c.want, h, ok)
		}
	}
	if st := hp.ExitFromHost(syscall.WaitStatus(3 << 8)); st.Signaled || st.Code != 3 || st.Signal != 0 {
		t.Errorf("exit 3: %+v", st)
	}
	if s := hp.SigFromHost(syscall.Signal(0x7e)); s != hp.SigOther {
		t.Errorf("unmapped signal: %d", s)
	}
	if hp.SigOther.Sendable() || hp.SigSEGV.Sendable() || !hp.SigUSR2.Sendable() {
		t.Error("Sendable")
	}
}
