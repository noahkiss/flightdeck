package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

// opencodeReady is how long the wrapper waits for the private server to report its URL.
const opencodeReady = 30 * time.Second

// opencodeStop is how long the private server gets to end after the TUI exits.
const opencodeStop = 3 * time.Second

// ocServer is a pane's server file, <state>/opencode/<pane uuid>.server.json. Only the pane's
// wrapper writes it: the URL its server reported once it listened, and the server's pid. Ended
// says the server exited. gff-hab reads it before each connection, so it never sends the
// password to a port the pane's server does not hold.
type ocServer struct {
	URL   string `json:"url"`
	PID   int    `json:"pid"`
	Ended bool   `json:"ended,omitempty"`
}

// opencodePaneCmd is `gff-hab opencode-pane -report path [--] [opencode argv]`, the program of
// an opencode pane gff-hab starts (harness-adapters.md, opencode, built choices). It runs a
// private opencode server for the pane, `opencode serve --stdio --port 0` on 127.0.0.1 with the
// pane's password, reads the URL the server prints once it listens, checks that the server
// answering there is its own child, writes the URL to the report file, then runs the TUI
// attached to it, `opencode --server <url>`. Both stay under the holder, so the agent loop and
// its tools see the pane's environment, and gff-hab talks to the same server the TUI shows.
// The server runs in its own process group with a pipe as its stdin: when the TUI exits the
// wrapper stops it, and when the wrapper dies the pipe closes and the server ends too. When the
// server exits the report says so. The wrapper exits with the TUI's status.
func opencodePaneCmd(args []string) int {
	fs := flag.NewFlagSet("opencode-pane", flag.ContinueOnError)
	report := fs.String("report", "", "the file to write the pane's server URL to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		argv = []string{"opencode"}
	}
	password := os.Getenv(opencodePasswordEnv)
	os.Unsetenv(opencodePasswordEnv)
	if *report == "" || !filepath.IsAbs(*report) || password == "" || argv[0] == "" {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: needs an absolute -report path, %s and a program; gff-hab sets them when it starts opencode\n", opencodePasswordEnv)
		return 2
	}
	env := append(os.Environ(), opencode.PasswordEnv+"="+password)

	srv := exec.Command(argv[0], "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "0")
	srv.Env = env
	tailErr := &tailBuffer{max: 4 << 10}
	srv.Stderr = tailErr
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := srv.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: %v\n", err)
		return 1
	}
	stdout, err := srv.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: %v\n", err)
		return 1
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: start the opencode server: %v\n", err)
		return 127
	}
	urls := make(chan string, 1)
	go readServerURL(stdout, urls)
	// exited closes once the server exited and the report says so.
	exited := make(chan struct{})
	var reported sync.Mutex // holds rec, the report once written, so the exit can mark it ended
	var rec *ocServer
	gone := false // under reported: the server exited
	go func() {
		srv.Wait()
		reported.Lock()
		gone = true
		if rec != nil {
			rec.Ended = true
			if err := writeServerFile(*report, *rec); err != nil {
				os.Remove(*report)
			}
		}
		reported.Unlock()
		close(exited)
	}()
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
	url, pid, err := serverReady(urls, exited, srv.Process.Pid, password, opencodeReady)
	if err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: the opencode server did not start: %v\n%s", err, tailErr.String())
		return 1
	}
	reported.Lock()
	if gone {
		reported.Unlock()
		stop()
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: the opencode server exited\n%s", tailErr.String())
		return 1
	}
	rec = &ocServer{URL: url, PID: pid}
	if err := writeServerFile(*report, *rec); err != nil {
		// The pane still works; gff-hab cannot follow it: no state, sends or chat.
		fmt.Fprintf(os.Stderr, "gff-hab opencode-pane: gff-hab will not see this opencode: %v\n", err)
	}
	reported.Unlock()

	tui := exec.Command(argv[0], append([]string{"--server", url}, argv[1:]...)...)
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
	os.Remove(*report) // the pane ends with the wrapper: nothing is left to follow
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

// readServerURL sends the URL of the first stdout line of the form {"url": ...}, which
// `opencode serve` prints once it listens, then discards the rest so the server never blocks
// on a full pipe.
func readServerURL(r io.Reader, urls chan<- string) {
	br := bufio.NewReaderSize(r, 4<<10)
	sent := false
	for !sent {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			for err == bufio.ErrBufferFull { // a long line names no URL: skip it
				_, err = br.ReadSlice('\n')
			}
			continue
		}
		var v struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(line, &v) == nil && v.URL != "" {
			urls <- v.URL
			sent = true
		}
		if err != nil {
			return
		}
	}
	io.Copy(io.Discard, br)
}

// serverReady waits for the server's URL, then checks that the server answering there is the
// wrapper's child, pid, or a process in its group (a launcher may run the real server as its
// child). It returns the URL and the pid of the process that answered.
func serverReady(urls <-chan string, exited <-chan struct{}, pid int, password string, limit time.Duration) (string, int, error) {
	var url string
	select {
	case url = <-urls:
	case <-exited:
		return "", 0, errors.New("it exited before it reported a URL")
	case <-time.After(limit):
		return "", 0, fmt.Errorf("no URL after %v", limit)
	}
	c, err := opencode.New(url, password)
	if err != nil {
		return "", 0, err
	}
	select {
	case <-exited:
		return "", 0, errors.New("it exited")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeReady)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		return "", 0, err
	}
	switch {
	case info.PID == 0:
		// A server that does not report its pid: the URL came from the child itself.
		return url, pid, nil
	case info.PID == pid:
		return url, pid, nil
	}
	if g, err := syscall.Getpgid(info.PID); err == nil && g == pid {
		return url, info.PID, nil
	}
	return "", 0, fmt.Errorf("the server at %s is process %d, not this pane's opencode server (process %d)", url, info.PID, pid)
}

// writeServerFile writes the pane's server file through a temporary file and a rename.
func writeServerFile(path string, s ocServer) error {
	b, _ := json.Marshal(s)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".srv-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// readServerFile reads a pane's server file.
func readServerFile(path string) (ocServer, error) {
	var s ocServer
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
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
