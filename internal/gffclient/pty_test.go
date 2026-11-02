package gffclient

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/noahkiss/flightdeck/api"
	"golang.org/x/sys/unix"
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
	// 100 columns: vim gets 50x39, the other program 49x39. vim redraws on SIGWINCH at once;
	// gff-hab's parser takes each size before the resize reaches the PTY, so that redraw is
	// parsed at the new size with no nudge. A tilde on row 38 is past the old bottom: vim drew at its new size and gff composed it.
	hst.waitScreen("both panes at the new size", func(s string) bool {
		return strings.Contains(s, "tui 39 49") && rowStarts(s, 38, "~")
	})

	ptmx.Write([]byte("\x0fd"))
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

// When gff dies without setting the terminal back (SIGQUIT ends a Go program the way a panic
// on any goroutine does: no deferred call runs), its guard puts back the terminal's settings
// and leaves the alternate screen.
//
// gff runs the way a terminal runs it: under a shell that leads the session and owns the
// terminal. When a session leader exits, macOS revokes its controlling terminal and every
// open of it then fails, so gff must not be the leader here. The shell reports the settings
// with stty -g before gff starts and, once gff has ended, again every 100 ms.
func TestCrashRestoresTerminal(t *testing.T) {
	h := startHab(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	rep, repw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	// A non-interactive shell gives a background job /dev/null as its input, so the job takes
	// the terminal from fd 4. Fd 3 reports, one line each: the settings, gff's pid, then the
	// settings again in a loop once gff has ended.
	const script = `stty -g >&3
exec 4<&0
"$@" <&4 3>&- 4<&- &
echo $! >&3
wait $!
while :; do stty -g >&3; sleep 0.1; done`
	// Two panes: the composed view, on the host's alternate screen.
	cmd := exec.Command("/bin/sh", "-c", script, "sh", gffBin, "-socket", h.sock,
		"-new", "echo left; exec sleep 600", "-new", "echo right; exec sleep 600")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.ExtraFiles = []*os.File{repw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = cmd.Start()
	repw.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the shell's group: gff, its guard, sleep
		<-done
	})
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(rep)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(what string) string {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("the shell ended before it reported %s", what)
			}
			return l
		case <-time.After(waitLong()):
			t.Fatalf("timed out waiting for %s", what)
		}
		return ""
	}
	cooked := next("the settings")
	pid, err := strconv.Atoi(next("gff's pid"))
	if err != nil {
		t.Fatal(err)
	}
	hst := newHost(t, 80, 24, false)
	go io.Copy(hst, ptmx)
	hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "left") && strings.Contains(s, "right") })
	if raw, _ := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios); raw.Lflag&unix.ECHO != 0 {
		t.Fatal("gff did not put the terminal in raw mode")
	}

	syscall.Kill(pid, syscall.SIGQUIT)
	// The shell reports again only once gff has ended.
	deadline := time.Now().Add(waitLong())
	for now := next("the settings after gff ended"); now != cooked; now = next("the settings after gff ended") {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the terminal's settings put back: have %s, want %s", now, cooked)
		}
	}
	waitUntil(t, "the alternate screen left", func() bool { return bytes.Contains(hst.output(), []byte(HostReset)) })
	if s := hst.screen(); strings.Contains(s, "left") {
		t.Errorf("the host is still on the composed screen:\n%s", s)
	}
}

// rowStarts reports whether row y of a screen dump starts with prefix.
func rowStarts(screen string, y int, prefix string) bool {
	rows := strings.Split(screen, "\n")
	return y < len(rows) && strings.HasPrefix(rows[y], prefix)
}
