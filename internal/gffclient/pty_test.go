package gffclient

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/noahkiss/flightdeck/api"
)

// The manual check, automated: the gff binary in a real terminal (a pty) starts vim and a
// small full-screen program side by side, the terminal resizes, and both panes redraw at
// their new sizes from gff's parsed screens. Then gff detaches and exits 0.
func TestSplitsRedrawAfterResize(t *testing.T) {
	vim, err := exec.LookPath("vim")
	if err != nil {
		t.Skip("no vim on PATH")
	}
	h := startHab(t)
	dir := shortDir(t)
	tui := filepath.Join(dir, "tui.sh")
	os.WriteFile(tui, []byte(`draw() { printf '\033[H\033[2Jtui %s' "$(stty size)"; }
trap draw WINCH
printf '\033[?1049h'
draw
while :; do sleep 0.1; done
`), 0o600)

	cmd := exec.Command(gffBin, "-socket", h.sock, "-new", vim+" -u NONE -N -n", "-new", "sh "+tui)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	hst := newHost(t, 120, 30, false)
	go io.Copy(hst, ptmx)

	// gff starts each new pane at its view's size: 120 columns give vim 60x29 and the other
	// program 59x29, with no resize while they start.
	hst.waitScreen("both panes at the first size", func(s string) bool {
		return strings.Contains(s, "tui 29 59") && rowStarts(s, 28, "~")
	})
	other := h.other()
	var res api.StructureResult
	call(t, other, "structure.get", api.StructureGetParams{}, &res)
	for _, p := range res.Structure.Panes {
		if p.Rows != 29 || (p.Cols != 60 && p.Cols != 59) {
			t.Errorf("pane %s is %dx%d, want started at its view's size", p.ID, p.Cols, p.Rows)
		}
	}

	hst.mu.Lock()
	hst.cols, hst.rows = 100, 40
	hst.term.Resize(100, 40)
	hst.mu.Unlock()
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 100, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	// 100 columns: vim gets 50x39, the other program 49x39. vim redraws on SIGWINCH at once,
	// and gff-hab may parse that redraw before it resizes its own parser (the link resizes the
	// parser after the holder answers the resize), which leaves every client's copy of vim's
	// screen at the old size. That is a gff-hab race, not the client's; Ctrl+L makes vim draw
	// again once the hab has the new sizes, so this test checks the client alone.
	waitUntil(t, "the hab to apply both new sizes", func() bool {
		var res api.StructureResult
		call(t, other, "structure.get", api.StructureGetParams{}, &res)
		sizes := map[[2]int]bool{}
		for _, p := range res.Structure.Panes {
			sizes[[2]int{p.Cols, p.Rows}] = true
		}
		return len(sizes) == 2 && sizes[[2]int{50, 39}] && sizes[[2]int{49, 39}]
	})
	ptmx.Write([]byte("\x0c")) // vim is the focused pane
	// A tilde on row 38 is past the old bottom: vim drew at its new size and gff composed it.
	hst.waitScreen("both panes at the new size", func(s string) bool {
		return strings.Contains(s, "tui 39 49") && rowStarts(s, 38, "~")
	})

	ptmx.Write([]byte("\x1cd"))
	select {
	case err := <-done:
		done <- err
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("gff exited %v", err)
		}
	case <-time.After(waitLong()):
		t.Fatal("gff did not exit after detach")
	}
}

// rowStarts reports whether row y of a screen dump starts with prefix.
func rowStarts(screen string, y int, prefix string) bool {
	rows := strings.Split(screen, "\n")
	return y < len(rows) && strings.HasPrefix(rows[y], prefix)
}
