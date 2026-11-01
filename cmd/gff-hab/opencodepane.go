package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/opencode"
)

// opencodePasswordEnv carries a pane's opencode server password from gff-hab to the
// opencode-pane wrapper, which removes it from its own environment and hands it to opencode
// as OPENCODE_PASSWORD.
const opencodePasswordEnv = "GFF_OPENCODE_PASSWORD"

// opencodeReady is how long the wrapper waits for the private server to listen.
const opencodeReady = 30 * time.Second

// opencodeStop is how long the private server gets to end after the TUI exits.
const opencodeStop = 3 * time.Second

// opencodePaneCmd is `gff-hab opencode-pane -port n [--] [opencode argv]`, the program of an
// opencode pane gff-hab starts (harness-adapters.md, opencode, built choices). It runs a
// private opencode server for the pane, `opencode serve --stdio` on 127.0.0.1:<port> with the
// pane's password, then the TUI attached to it, `opencode --server <url>`. Both stay under the
// holder, so the agent loop and its tools see the pane's environment, and gff-hab talks to
// the same server the TUI shows. The server runs in its own process group with a pipe as its
// stdin: when the TUI exits the wrapper stops it, and when the wrapper dies the pipe closes and
// the server ends too. The wrapper exits with the TUI's status.
func opencodePaneCmd(args []string) int {
	fs := flag.NewFlagSet("opencode-pane", flag.ContinueOnError)
	port := fs.Int("port", 0, "the loopback port of the pane's opencode server")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		argv = []string{"opencode"}
	}
	password := os.Getenv(opencodePasswordEnv)
	os.Unsetenv(opencodePasswordEnv)
	if *port <= 0 || *port > 65535 || password == "" {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: needs -port and %s; gff-hab sets both when it starts opencode\n", opencodePasswordEnv)
		return 2
	}
	env := append(os.Environ(), opencode.PasswordEnv+"="+password)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))

	srv := exec.Command(argv[0], "serve", "--stdio", "--hostname", "127.0.0.1", "--port", strconv.Itoa(*port))
	srv.Env = env
	srv.Stdout = io.Discard
	tailErr := &tailBuffer{max: 4 << 10}
	srv.Stderr = tailErr
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := srv.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: %v\n", err)
		return 1
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: start the opencode server: %v\n", err)
		return 127
	}
	exited := make(chan struct{})
	go func() { srv.Wait(); close(exited) }()
	stop := func() {
		stdin.Close()
		syscall.Kill(-srv.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(opencodeStop):
			syscall.Kill(-srv.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	}
	if err := waitListening(addr, exited, opencodeReady); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: the opencode server on %s did not start: %v\n%s", addr, err, tailErr.String())
		return 1
	}

	tui := exec.Command(argv[0], append([]string{"--server", "http://" + addr}, argv[1:]...)...)
	tui.Env = env
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal's own signals reach the TUI directly (it is in the foreground group); the
	// wrapper only passes on what is sent to it alone, and never dies of them first.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := tui.Start(); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: start opencode: %v\n", err)
		return 127
	}
	go func() {
		for s := range sigs {
			if s == syscall.SIGHUP || s == syscall.SIGTERM {
				tui.Process.Signal(s)
			}
		}
	}()
	err = tui.Wait()
	stop()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	return 1
}

// waitListening waits until addr takes a TCP connection, the server exits, or the time is up.
func waitListening(addr string, exited <-chan struct{}, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return errors.New("it exited")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not listening after %v", limit)
		}
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.b))
	if s == "" {
		return ""
	}
	return s + "\n"
}
