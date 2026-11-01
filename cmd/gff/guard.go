//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/noahkiss/flightdeck/internal/gffclient"
	"golang.org/x/sys/unix"
)

// The guard sets the terminal back when gff dies without doing it: a panic on any goroutine,
// SIGQUIT, SIGKILL. Go has no process-wide panic hook, so the guard is a small child process
// (gff itself, run with guardEnv set). It saves the terminal's settings before gff enters raw
// mode, then waits on a pipe. gff writes one byte on it once it has set the terminal back; when
// the pipe ends without that byte, gff died, and the guard puts back the saved settings and
// writes gffclient.HostReset.
const guardEnv = "GFF_TERMINAL_GUARD"

// guardDone is the byte gff sends when it ended cleanly.
const guardDone = 'd'

type guard struct {
	w   *os.File
	cmd *exec.Cmd
}

// startGuard starts the guard and returns once it has saved the terminal's settings. The
// terminal is gff's standard input, which must still be in its normal mode.
func startGuard() (*guard, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	rr, rw, err := os.Pipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), guardEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pr, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.Stdin, rw} // fd 3: the terminal; fd 4: ready
	err = cmd.Start()
	pr.Close()
	rw.Close()
	if err != nil {
		pw.Close()
		rr.Close()
		return nil, err
	}
	b := make([]byte, 1)
	_, err = rr.Read(b)
	rr.Close()
	if err != nil {
		pw.Close()
		cmd.Wait()
		return nil, fmt.Errorf("terminal guard: %w", err)
	}
	return &guard{w: pw, cmd: cmd}, nil
}

// stop tells the guard gff set the terminal back, and waits for it to end.
func (g *guard) stop() {
	g.w.Write([]byte{guardDone})
	g.w.Close()
	g.cmd.Wait()
}

// runGuard is the guard process. Its standard input is the pipe from gff, fd 3 the terminal
// and fd 4 the ready pipe.
func runGuard() int {
	// The terminal's keys and a signal to gff's process group must not end the guard first.
	// SIGTTOU too: once gff dies the shell takes the foreground back, and a background
	// tcsetattr (or a write with tostop set) would stop the guard before it restores.
	// With SIGTTOU ignored the kernel lets both go through.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGTSTP,
		syscall.SIGTTOU)
	saved, err := unix.IoctlGetTermios(3, ioctlGetTermios)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gff: terminal guard:", err)
		return 1
	}
	ready := os.NewFile(4, "ready")
	ready.Write([]byte{1})
	ready.Close()
	b := make([]byte, 1)
	n, _ := os.Stdin.Read(b)
	if n == 1 && b[0] == guardDone {
		return 0
	}
	// gff died with the terminal as it left it.
	unix.IoctlSetTermios(3, ioctlSetTermios, saved)
	if _, err := os.Stdout.WriteString(gffclient.HostReset + "\r\n"); err != nil {
		return 1 // the terminal is gone
	}
	fmt.Fprintln(os.Stderr, "gff: ended without setting the terminal back; it is set back now")
	return 0
}
