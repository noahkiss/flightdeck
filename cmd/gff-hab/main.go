// Command gff-hab is the logic server on each host (docs/specs/hab-logic-api.md). This build
// serves the control connection on a local Unix socket, with no TCP port, carries the
// structure store, and follows every holder's panes through the holder link (internal/link).
//
//	gff-hab serve   [-socket path] [-state dir] [-host-id id] [-holders dir]
//	gff-hab connect [-socket path]
//	gff-hab api call [-socket path | -via command] <method> [params-json]
//	gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
//
// The socket path comes from -socket, else $GFF_HAB_SOCKET, else gff/hab.sock under the user
// config directory on macOS and under $XDG_RUNTIME_DIR elsewhere. The state directory comes from
// -state, else $GFF_HAB_STATE, else gff/hab under the user config directory on macOS and under
// $XDG_STATE_HOME (or ~/.local/state) elsewhere. The holder directory comes from -holders, else
// $GFF_HOLDER_DIR, else the holder's own default; screen checkpoints go to <state>/checkpoints.
//
// `gff-hab connect` copies stdin to the socket and the socket to stdout, so
// `ssh <host> gff-hab connect` reaches a remote hab with no new listener. -via runs a command
// such as that and speaks the protocol over its stdin and stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habkey"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// build is set at link time: -ldflags "-X main.build=<id>".
var build = "dev"

const usage = `usage:
  gff-hab serve   [-socket path] [-state dir] [-host-id id] [-holders dir]
  gff-hab connect [-socket path]
  gff-hab api call [-socket path | -via command] <method> [params-json]
  gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "connect":
		err = connect(os.Args[2:])
	case "api":
		if len(os.Args) < 3 || os.Args[2] != "call" {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = apiCall(os.Args[3:])
	case "events":
		err = events(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab: %v\n", err)
		var ce *control.Error
		if errors.As(err, &ce) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func socketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", "", "control socket path (default: see gff-hab -h)")
}

func socketPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	return control.DefaultSocket(runtime.GOOS, os.Getenv, os.UserConfigDir)
}

func stateDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if d := os.Getenv("GFF_HAB_STATE"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "darwin" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "gff", "hab"), nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gff", "hab"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "gff", "hab"), nil
}

// emptyHolders is the pane source when the holder link is off: no panes, no notices.
type emptyHolders struct{}

func (emptyHolders) Panes() ([]structure.HolderPane, error)           { return nil, nil }
func (emptyHolders) Subscribe(func(structure.Notice)) (cancel func()) { return func() {} }

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sock := socketFlag(fs)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	hostID := fs.String("host-id", os.Getenv("GFF_HAB_HOST_ID"), "host id sent in the welcome (default: $GFF_HAB_HOST_ID, else the system host name)")
	holders := fs.String("holders", "", "holder registry directory (default: $GFF_HOLDER_DIR, else the holder's default)")
	fs.Parse(args)
	logger := log.New(os.Stderr, "gff-hab: ", log.LstdFlags)

	path, err := socketPath(*sock)
	if err != nil {
		return err
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	if *hostID == "" {
		if *hostID, err = os.Hostname(); err != nil {
			return err
		}
	}
	key, err := habkey.LoadOrCreate(dir)
	if err != nil {
		return err
	}

	lk, err := openLink(*holders, dir, logger)
	if err != nil {
		return err
	}
	var hs structure.Holders = emptyHolders{}
	if lk != nil {
		hs = lk.Structure()
	}
	var srv atomic.Pointer[control.Server]
	store, err := structure.Open(structure.Config{
		Dir:     dir,
		Holders: hs,
		OnEvent: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		},
	})
	if err != nil {
		return err
	}
	defer store.Close()
	if h := store.Health(); h.Problem != "" {
		logger.Printf("state: %s", h.Problem)
	}
	s := control.New(control.Config{
		Build:    build,
		Host:     api.HostInfo{ID: *hostID},
		DeviceID: key.Fingerprint(),
		Source:   store,
		Logf:     logger.Printf,
	})
	srv.Store(s)
	l, err := control.Listen(path)
	if err != nil {
		return err
	}
	logger.Printf("epoch %s build %s device %s listening on %s", s.Epoch(), build, key.Fingerprint(), path)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	stopped := make(chan struct{})
	go func() {
		sig := <-sigs
		logger.Printf("%v: shutting down", sig)
		s.Close() // sends closing (shutdown) to every client and waits for it to flush
		if lk != nil {
			if err := lk.Close(); err != nil { // planned stop: checkpoints, then handover
				logger.Printf("holder link: %v", err)
			}
		}
		close(stopped)
	}()
	if err := s.Serve(l); err != nil {
		return err
	}
	<-stopped
	return nil
}

// connect bridges stdin and stdout to the local socket. It copies bytes and parses nothing
// (spec 4.2). It ends when the hab closes the connection.
func connect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	sock := socketFlag(fs)
	fs.Parse(args)
	path, err := socketPath(*sock)
	if err != nil {
		return err
	}
	nc, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer nc.Close()
	go func() {
		io.Copy(nc, os.Stdin)
		// Client finished sending: half-close so the hab sees EOF and closes its side.
		if uc, ok := nc.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, nc)
	return err
}

// cmdConn runs a command and speaks over its stdin and stdout. Close may be called more than
// once (the client closes at EOF, and the caller closes again); the command is waited for once.
type cmdConn struct {
	cmd *exec.Cmd
	io.Reader
	io.WriteCloser
	once sync.Once
	err  error
}

func (c *cmdConn) Close() error {
	c.once.Do(func() {
		c.WriteCloser.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case c.err = <-done:
		case <-time.After(5 * time.Second):
			c.cmd.Process.Kill()
			c.err = <-done
		}
	})
	return c.err
}

func open(sock, via string) (*control.Client, error) {
	if via != "" {
		cmd := exec.Command("/bin/sh", "-c", via)
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return control.NewClient(&cmdConn{cmd: cmd, Reader: out, WriteCloser: in}), nil
	}
	path, err := socketPath(sock)
	if err != nil {
		return nil, err
	}
	return control.Dial(path)
}

var cliClient = api.ClientInfo{Name: "gff-hab", Version: build, Kind: api.ClientKindCli}

func printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Printf("%s\n", b)
	return err
}

// apiCall says hello, sends one request and prints its result as one JSON line. Calling
// hab.hello prints the welcome.
func apiCall(args []string) error {
	fs := flag.NewFlagSet("api call", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	timeout := fs.Duration("timeout", 30*time.Second, "give up after this long")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("api call needs a method and optional JSON params")
	}
	method := fs.Arg(0)
	var params any = struct{}{}
	if fs.NArg() == 2 {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(fs.Arg(1)), &raw); err != nil {
			return fmt.Errorf("params are not JSON: %w", err)
		}
		params = raw
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	w, err := c.Hello(ctx, cliClient, "", "")
	if err != nil {
		return err
	}
	if method == "hab.hello" {
		return printJSON(w)
	}
	var result json.RawMessage
	if err := c.Call(ctx, method, params, &result); err != nil {
		return err
	}
	_, err = fmt.Printf("%s\n", result)
	return err
}

func parseScope(s string) (api.Scope, error) {
	kind, ids, _ := strings.Cut(s, ":")
	sc := api.Scope{Kind: api.ScopeKind(kind)}
	if ids != "" {
		sc.IDs = strings.Split(ids, ",")
	}
	if sc.Kind != api.ScopeKindHost && len(sc.IDs) == 0 {
		return sc, fmt.Errorf("scope %q needs ids, as in projects:id1,id2", s)
	}
	return sc, nil
}

// events subscribes and prints the snapshot, then every notification, one JSON line each:
// {"method":..., "params":...}. It ends when the hab closes the connection; a closing other
// than shutdown is an error.
func events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	scope := fs.String("scope", "host", "host, projects:id,... or sessions:id,...")
	fs.Parse(args)
	sc, err := parseScope(*scope)
	if err != nil {
		return err
	}
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, err := c.Hello(ctx, cliClient, "", "")
	if err != nil {
		return err
	}
	if err := printJSON(map[string]any{"method": "welcome", "params": w}); err != nil {
		return err
	}
	c.KeepAlive(control.DefaultHeartbeat)
	r, err := api.Client{Caller: c}.EventsSubscribe(ctx, api.SubscribeParams{Scope: sc})
	if err != nil {
		return err
	}
	if err := printJSON(map[string]any{"method": "snapshot", "params": r}); err != nil {
		return err
	}
	for n := range c.Notifications() {
		if err := printJSON(map[string]any{"method": n.Method, "params": n.Params}); err != nil {
			return err
		}
	}
	switch reason := c.ClosingReason(); reason {
	case api.ClosingReasonShutdown:
		return nil
	case "":
		return fmt.Errorf("connection ended with no closing message: %w", c.Err())
	default:
		return fmt.Errorf("hab closed the connection: %s", reason)
	}
}
