package link

import (
	"github.com/noahkiss/flightdeck/api"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Structure returns the link as the structure store's view of the holders.
func (l *Link) Structure() structure.Holders { return structureHolders{l} }

type structureHolders struct{ l *Link }

// Panes lists every pane the link follows. Lost and released panes are left out.
func (s structureHolders) Panes() ([]structure.HolderPane, error) {
	var out []structure.HolderPane
	for _, st := range s.l.Panes() {
		if st.Lost || st.Gone {
			continue
		}
		out = append(out, holderPane(st))
	}
	return out, nil
}

// Subscribe passes pane notices on. A lost pane is reported gone: its holder died, so its
// program did too (holder spec 6.6).
func (s structureHolders) Subscribe(fn func(structure.Notice)) func() {
	return s.l.Watch(func(n Notice) {
		pn := holderPane(n.Pane)
		switch n.Kind {
		case PaneAdded:
			fn(structure.Notice{Kind: structure.PaneAdded, Pane: pn})
		case PaneExited:
			fn(structure.Notice{Kind: structure.PaneExited, Pane: pn, Exit: pn.Exit})
		case PaneGone:
			fn(structure.Notice{Kind: structure.PaneGone, Pane: pn})
		}
	})
}

func holderPane(st PaneStatus) structure.HolderPane {
	p := structure.HolderPane{
		ID: st.UUID.String(), HolderID: st.Instance, HolderBuild: st.Build,
		Cols: st.Cols, Rows: st.Rows, Offset: st.Offset,
	}
	if st.Exited {
		p.Exit = exitStatus(st.Exit)
	}
	return p
}

// APIExit returns an exit status as the API carries it.
func APIExit(e hp.ExitStatus) api.ExitStatus { return *exitStatus(e) }

func exitStatus(e hp.ExitStatus) *api.ExitStatus {
	if !e.Signaled {
		code := int(e.Code)
		return &api.ExitStatus{Code: &code}
	}
	return &api.ExitStatus{Signal: signalName(e.Signal)}
}

var signalNames = map[hp.Sig]string{
	hp.SigHUP: "SIGHUP", hp.SigINT: "SIGINT", hp.SigQUIT: "SIGQUIT", hp.SigTERM: "SIGTERM",
	hp.SigKILL: "SIGKILL", hp.SigWINCH: "SIGWINCH", hp.SigCONT: "SIGCONT", hp.SigSTOP: "SIGSTOP",
	hp.SigUSR1: "SIGUSR1", hp.SigUSR2: "SIGUSR2", hp.SigPIPE: "SIGPIPE", hp.SigALRM: "SIGALRM",
	hp.SigSEGV: "SIGSEGV", hp.SigABRT: "SIGABRT", hp.SigBUS: "SIGBUS", hp.SigFPE: "SIGFPE",
	hp.SigILL: "SIGILL", hp.SigTRAP: "SIGTRAP", hp.SigSYS: "SIGSYS", hp.SigTSTP: "SIGTSTP",
	hp.SigTTIN: "SIGTTIN", hp.SigTTOU: "SIGTTOU", hp.SigXCPU: "SIGXCPU", hp.SigXFSZ: "SIGXFSZ",
	hp.SigVTALRM: "SIGVTALRM", hp.SigPROF: "SIGPROF",
}

func signalName(s hp.Sig) string {
	if n, ok := signalNames[s]; ok {
		return n
	}
	return "other"
}
