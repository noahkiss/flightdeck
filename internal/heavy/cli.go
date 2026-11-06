package heavy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// Env is what the CLI reads from its process. Tests fill it.
type Env struct {
	Args   []string // after the heavy word
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Client api.ClientInfo // the hello's client
	Name   string         // the command name in messages: "gff heavy"
	// Patience is how long a waiting CLI tries to reach gff-hab again after its connection
	// dropped, before it runs the command without the lock. Default 2 minutes.
	Patience time.Duration
	// Retry is the pause between tries to reach gff-hab again. Default 250 ms.
	Retry time.Duration
}

const cliUsage = `usage: %[1]s [--socket path] [--] <command> [args...]

Run a build or test command under this host's heavy-run lock: wait for the lock in arrival
order, run the command, give the lock back when it exits. The command's output and exit code
pass through unchanged. Without a running gff-hab the command runs without the lock.
`

// Main runs the CLI and returns the command's exit code; 64 on a usage error, 127 when the
// command is not found, 128 plus the signal when a signal ended the wait or the command.
func Main(env Env) int {
	if env.Patience <= 0 {
		env.Patience = 2 * time.Minute
	}
	if env.Retry <= 0 {
		env.Retry = 250 * time.Millisecond
	}
	socket, argv, help, err := parseArgs(env.Args)
	if help {
		fmt.Fprintf(env.Stdout, cliUsage, env.Name)
		return 0
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", env.Name, err)
		fmt.Fprintf(env.Stderr, cliUsage, env.Name)
		return 64
	}
	r := &runner{env: env, argv: argv, command: strings.Join(argv, " ")}
	r.socket, r.token = r.where(socket)
	return r.run()
}

func parseArgs(args []string) (socket string, argv []string, help bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			argv = args[i+1:]
		case a == "-h" || a == "-help" || a == "--help":
			return "", nil, true, nil
		case a == "-socket" || a == "--socket":
			if i+1 >= len(args) {
				return "", nil, false, errors.New("--socket needs a path")
			}
			i++
			socket = args[i]
			continue
		case strings.HasPrefix(a, "-socket=") || strings.HasPrefix(a, "--socket="):
			socket = a[strings.Index(a, "=")+1:]
			continue
		case strings.HasPrefix(a, "-"):
			return "", nil, false, fmt.Errorf("unknown flag %s; put -- before the command", a)
		default:
			argv = args[i:]
		}
		break
	}
	if len(argv) == 0 {
		return "", nil, false, errors.New("needs a command")
	}
	return socket, argv, false, nil
}

type runner struct {
	env     Env
	argv    []string
	command string
	socket  string
	token   string // the pane's token from GFF_HAB, else ""

	mu     sync.Mutex
	client *control.Client
	last   api.HeavyTaken
}

// where picks the control socket and the pane token. In a pane, the control socket sits beside
// the hook socket unless --socket or GFF_HAB_SOCKET says otherwise.
func (r *runner) where(flagSocket string) (socket, token string) {
	socket = flagSocket
	if addr := r.env.Getenv(hook.Env); addr != "" {
		if hs, tok, err := hook.ParseAddr(addr); err == nil {
			token = tok
			if socket == "" && r.env.Getenv("GFF_HAB_SOCKET") == "" {
				if c := filepath.Join(filepath.Dir(hs), "hab.sock"); exists(c) {
					socket = c
				}
			}
		}
	}
	if socket == "" {
		socket, _ = control.DefaultSocket(runtime.GOOS, r.env.Getenv, os.UserConfigDir)
	}
	return socket, token
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (r *runner) say(format string, args ...any) {
	fmt.Fprintf(r.env.Stderr, "%s: %s\n", r.env.Name, fmt.Sprintf(format, args...))
}

// errNoLock is a hab that serves no heavy-run lock: run without it.
var errNoLock = errors.New("gff-hab has no heavy-run lock")

// connect dials gff-hab and says hello. The client pings so a long hold outlives the idle
// timeout.
func (r *runner) connect(ctx context.Context) (*control.Client, error) {
	if r.socket == "" {
		return nil, errors.New("no control socket")
	}
	c, err := control.Dial(r.socket)
	if err != nil {
		return nil, err
	}
	w, err := c.Hello(ctx, r.env.Client, "", "")
	if err != nil {
		c.Close()
		return nil, err
	}
	has := false
	for _, cap := range w.Capabilities {
		has = has || cap == Capability
	}
	if !has {
		c.Close()
		return nil, errNoLock
	}
	c.KeepAlive(control.DefaultHeartbeat)
	return c, nil
}

// take sends one heavy.take on c, carrying the ticket's last state so a hab that restarted
// gives back its place.
func (r *runner) take(ctx context.Context, c *control.Client, wait time.Duration) (api.HeavyTaken, error) {
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()
	p := api.HeavyTakeParams{Ticket: last.Ticket, PaneToken: r.token, Command: r.command, WaitMS: int(wait.Milliseconds())}
	if last.Ticket != "" {
		p.Holding, p.Since, p.Granted = last.State == "holding", last.Since, last.Granted
	}
	res, err := api.Client{Caller: c}.HeavyTake(ctx, p)
	if err == nil {
		r.mu.Lock()
		r.last = res
		r.mu.Unlock()
	}
	return res, err
}

// signalled is a signal that ended a wait for gff-hab.
type signalled struct{ sig syscall.Signal }

func (s signalled) Error() string { return "stopped by " + s.sig.String() }

// reconnect dials again until it reaches gff-hab and the take answers, or patience runs out
// (0: until stop closes), or a signal comes on sigs (nil: none read). It returns the new client.
func (r *runner) reconnect(stop <-chan struct{}, sigs <-chan os.Signal, patience time.Duration) (*control.Client, api.HeavyTaken, error) {
	var deadline time.Time
	if patience > 0 {
		deadline = time.Now().Add(patience)
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := r.connect(ctx)
		if err == nil {
			var res api.HeavyTaken
			res, err = r.take(ctx, c, 0)
			cancel()
			if err == nil {
				return c, res, nil
			}
			c.Close()
		} else {
			cancel()
		}
		if errors.Is(err, errNoLock) || isAnswer(err) {
			return nil, api.HeavyTaken{}, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, api.HeavyTaken{}, err
		}
		select {
		case <-stop:
			return nil, api.HeavyTaken{}, errors.New("stopped")
		case sg := <-sigs:
			return nil, api.HeavyTaken{}, signalled{sg.(syscall.Signal)}
		case <-time.After(r.env.Retry):
		}
	}
}

// isAnswer reports an error the hab answered with, as opposed to a connection that failed.
func isAnswer(err error) bool {
	var ce *control.Error
	return errors.As(err, &ce)
}

func (r *runner) run() int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, err := r.connect(ctx)
	var res api.HeavyTaken
	if err == nil {
		res, err = r.take(ctx, c, 0)
		if err != nil {
			c.Close()
			c = nil
		}
	}
	cancel()
	if err != nil {
		if isAnswer(err) {
			r.say("%v", err)
			return 1
		}
		r.say("running without the heavy-run lock: %v", err)
		return r.exec(sigs, nil)
	}
	if res.State != "holding" {
		line := fmt.Sprintf("waiting for the heavy-run lock: %d ahead", res.Ahead)
		if res.Reason != "" {
			line += " (" + res.Reason + ")"
		}
		r.say("%s", line)
	}
	waits := make(chan result, 1)
	for res.State != "holding" {
		go func(c *control.Client) {
			res, err := r.take(context.Background(), c, MaxWait)
			waits <- result{res, err}
		}(c)
		select {
		case s := <-sigs:
			if s == syscall.SIGINT || s == syscall.SIGQUIT || s == syscall.SIGTERM || s == syscall.SIGHUP {
				r.release(c)
				c.Close()
				return 128 + int(s.(syscall.Signal))
			}
		case w := <-waits:
			res, err = w.res, w.err
		}
		if err == nil {
			continue
		}
		var ce *control.Error
		if errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound {
			// The hab let the ticket go (its connection dropped); take it back in its place.
			res, err = r.take(context.Background(), c, 0)
			if err == nil {
				continue
			}
		}
		if isAnswer(err) {
			c.Close()
			r.say("%v", err)
			return 1
		}
		c.Close()
		c, res, err = r.reconnect(nil, sigs, r.env.Patience)
		var sg signalled
		if errors.As(err, &sg) {
			// A signal while waiting ends the wait; the hab, when it is back, holds no ticket.
			return 128 + int(sg.sig)
		}
		if err != nil {
			r.say("gff-hab did not come back (%v); running without the heavy-run lock", err)
			return r.exec(sigs, nil)
		}
	}
	r.mu.Lock()
	r.client = c
	r.mu.Unlock()
	return r.exec(sigs, c)
}

type result struct {
	res api.HeavyTaken
	err error
}

// release gives the ticket back on c, best effort.
func (r *runner) release(c *control.Client) {
	r.mu.Lock()
	t := r.last.Ticket
	r.mu.Unlock()
	if c == nil || t == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = api.Client{Caller: c}.HeavyRelease(ctx, api.HeavyReleaseParams{Ticket: t})
}

// exec runs the command with this process's stdio and returns its exit code. With c, the CLI
// holds the lock meanwhile: a dropped connection is dialed again with the ticket, and the lock
// goes back when the command exits. SIGINT and SIGQUIT reach the command from the terminal
// itself; SIGTERM and SIGHUP sent to the CLI are passed on.
func (r *runner) exec(sigs <-chan os.Signal, c *control.Client) int {
	cmd := exec.Command(r.argv[0], r.argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.env.Stdin, r.env.Stdout, r.env.Stderr
	if err := cmd.Start(); err != nil {
		if c != nil {
			r.release(c)
			c.Close()
		}
		r.say("%v", err)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127
		}
		return 126
	}
	exited := make(chan struct{})
	var wg sync.WaitGroup
	if c != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.hold(c, exited)
		}()
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var werr error
	for loop := true; loop; {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM || s == syscall.SIGHUP {
				_ = cmd.Process.Signal(s)
			}
		case werr = <-waited:
			loop = false
		}
	}
	close(exited)
	wg.Wait()
	r.mu.Lock()
	held := r.client
	r.mu.Unlock()
	if held != nil {
		r.release(held)
		held.Close()
	}
	if werr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return 128 + int(st.Signal())
		}
		return ee.ExitCode()
	}
	r.say("%v", werr)
	return 1
}

// hold keeps the ticket while the command runs: when the connection drops (gff-hab restarted),
// it dials again with the ticket until it is back or the command exited.
func (r *runner) hold(c *control.Client, exited <-chan struct{}) {
	for {
		select {
		case <-exited:
			return
		case <-c.Done():
		}
		c.Close()
		nc, _, err := r.reconnect(exited, nil, 0)
		if err != nil {
			r.mu.Lock()
			r.client = nil
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		r.client = nc
		r.mu.Unlock()
		c = nc
	}
}
