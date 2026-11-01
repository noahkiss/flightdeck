// Command gff is the terminal client (docs/specs/gff-client.md). This build is the client core:
// it connects to one hab and shows one or more of its panes in the terminal it runs in.
//
//	gff [-socket path | -via command | -ssh host] [-new command]... [-split columns|rows]
//	    [-observe] [-prefix ctrl-<key>] [pane...]
//
// It shows the panes named on the command line, then one new pane per -new (the command runs
// under /bin/sh -c on the hab). With neither, it starts the hab's shell in a new pane. One pane
// that fits the terminal passes its bytes straight through; several share the screen side by
// side (-split columns) or stacked (-split rows). -observe opens every pane as an observer: it
// shows the pane and never sends input or a size.
//
// The hab is reached on its local socket (-socket, else $GFF_HAB_SOCKET, else gff/hab.sock under
// the user config directory on macOS and under $XDG_RUNTIME_DIR elsewhere), through a command
// that speaks over its stdio (-via), or through the system ssh running `gff-hab connect` on the
// host (-ssh).
//
// Keys go to the focused pane. The prefix key, Ctrl+\ unless -prefix names another, then:
//
//	o or Tab  focus the next pane        z  zoom the focused pane, and back
//	s         side by side or stacked    r  redraw: a fresh snapshot from the hab
//	n         nudge: snapshot, and the program redraws after a one-column resize
//	f         fit: make the pane this window's size (pane.take)
//	d         detach: quit gff; the programs keep running
//	the prefix again sends it to the pane; Esc cancels
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/gffclient"
	"github.com/noahkiss/flightdeck/internal/habconn"
)

// build is the version string, set by the release build.
var build = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ", ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil:
	case errors.Is(err, errEnded):
		fmt.Fprintln(os.Stderr, "gff: every pane has ended")
	case errors.Is(err, gffclient.ErrDetached):
		fmt.Fprintln(os.Stderr, "gff: detached; the programs keep running")
	default:
		fmt.Fprintln(os.Stderr, "gff:", err)
		os.Exit(1)
	}
}

var errEnded = errors.New("every pane has ended")

func run(args []string) error {
	fs := flag.NewFlagSet("gff", flag.ExitOnError)
	sock := fs.String("socket", "", "the hab's control socket")
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	sshHost := fs.String("ssh", "", "reach the hab through the system ssh to this host, running gff-hab connect")
	var news multi
	fs.Var(&news, "new", "start this command in a new pane (repeatable)")
	split := fs.String("split", "columns", "how panes share the screen: columns (side by side) or rows (stacked)")
	observe := fs.Bool("observe", false, "open every pane as an observer: no input, no size")
	prefixFlag := fs.String("prefix", "ctrl-\\", "the prefix key, as ctrl-<key>")
	fs.Parse(args)

	prefix, err := gffclient.ParsePrefix(*prefixFlag)
	if err != nil {
		return err
	}
	var sp gffclient.Split
	switch *split {
	case "columns":
		sp = gffclient.SplitColumns
	case "rows":
		sp = gffclient.SplitRows
	default:
		return fmt.Errorf("-split %q: want columns or rows", *split)
	}
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !isTerminal(in) || !isTerminal(out) {
		return errors.New("standard input and output must be a terminal")
	}

	tr, local, err := transport(*sock, *via, *sshHost)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer stop()
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := habconn.Connect(octx, habconn.Config{
		Transport: tr,
		Client:    api.ClientInfo{Name: "gff", Version: build, Kind: api.ClientKindTerminal},
	})
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	cols, rows := termSize(out)
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	var panes []gffclient.PaneSpec
	for _, id := range fs.Args() {
		panes = append(panes, gffclient.PaneSpec{ID: id, Observe: *observe})
	}
	if len(panes) == 0 && len(news) == 0 {
		news = append(news, "") // the hab's shell
	}
	// Each new pane starts at its view's size, so its program gets no resize as it starts.
	sizes := gffclient.ViewSizes(len(panes)+len(news), sp, cols, rows)
	for _, cmd := range news {
		size := sizes[len(panes)]
		p := api.SessionCreateParams{Cols: size[0], Rows: size[1]}
		if cmd != "" {
			p.Argv = []string{"/bin/sh", "-c", cmd}
		}
		if local {
			if wd, err := os.Getwd(); err == nil {
				p.Cwd = wd
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := conn.API().SessionCreate(cctx, p)
		cancel()
		if err != nil {
			return fmt.Errorf("start %q: %w", cmd, err)
		}
		panes = append(panes, gffclient.PaneSpec{ID: res.Pane, Label: label(cmd, res.Pane), Observe: *observe})
	}

	p, name := parser()
	if !strings.HasPrefix(name, "libghostty") {
		fmt.Fprintln(os.Stderr, "gff: parser", name)
	}
	restore, err := rawMode(in)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer restore()
	resized := make(chan struct{}, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			select {
			case resized <- struct{}{}:
			default:
			}
		}
	}()
	err = gffclient.Run(ctx, gffclient.Config{
		Conn:    conn,
		Parser:  p,
		Panes:   panes,
		Split:   sp,
		Prefix:  prefix,
		In:      os.Stdin,
		Out:     os.Stdout,
		Size:    func() (int, int) { return termSize(out) },
		Resized: resized,
	})
	restore()
	switch {
	case errors.Is(err, context.Canceled):
		return nil // a signal ended gff
	case err == nil:
		return errEnded
	}
	return err
}

// transport picks how to reach the hab. local is true for the local socket.
func transport(sock, via, host string) (habconn.Transport, bool, error) {
	n := 0
	for _, s := range []string{sock, via, host} {
		if s != "" {
			n++
		}
	}
	if n > 1 {
		return nil, false, errors.New("give one of -socket, -via and -ssh")
	}
	switch {
	case via != "":
		return habconn.CommandTransport{Argv: []string{"/bin/sh", "-c", via}}, false, nil
	case host != "":
		return habconn.SSHTransport{Host: host}, false, nil
	}
	if sock == "" {
		var err error
		if sock, err = control.DefaultSocket(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
			return nil, false, err
		}
	}
	return habconn.UnixTransport{Path: sock}, true, nil
}

// label names a new pane by its command's first word.
func label(cmd, pane string) string {
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return "shell " + short(pane)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
