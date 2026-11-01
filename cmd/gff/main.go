// Command gff is the terminal client (docs/specs/gff-client.md).
//
//	gff [-config path] [-host label=endpoint[,endpoint...]]... [-socket path | -via command | -ssh host]
//	    [-split columns|rows] [-prefix ctrl-<key>]
//	gff [-socket path | -via command | -ssh host] [-new command]... [-split columns|rows]
//	    [-observe] [-prefix ctrl-<key>] [pane...]
//
// With no pane and no -new, gff opens the workspace: every host at once, with tabs, a sidebar of
// every session on every host and a status board. Hosts come from gff's config file (-config,
// else $GFF_CONFIG, else gff/config.toml under the user config directory on macOS and under
// $XDG_CONFIG_HOME or ~/.config elsewhere) and from -host flags. An endpoint is a socket path,
// cmd:<command> for a command that speaks over its stdio, or an SSH destination. With no host
// configured, the one hab of -socket, -via or -ssh (else the local socket) is the only host.
// A host that is down shows offline and comes online when its hab does.
//
// With panes named on the command line or -new, gff shows just those panes of one hab, as the
// client core did: one new pane per -new (the command runs under /bin/sh -c on the hab). One
// pane that fits the terminal passes its bytes straight through; several share the screen side
// by side (-split columns) or stacked (-split rows). -observe opens every pane as an observer:
// it shows the pane and never sends input or a size.
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
//
// In the workspace, also:
//
//	w  the sidebar (j/k move, Enter open, t open in a new tab, n new session, r rename,
//	   x end or remove, / filter, p grouping, Esc back)
//	g  the status board          c  a new tab        ] [ 1-9  tabs
//	x  close the focused view; the program keeps running
//	b  hide or show the bars
//
// `gff setup` checks and walks the permission grants of the hab's processes (setup.go,
// docs/specs/macos-identity.md 7).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/gffclient"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// build is the version string, set by the release build.
var build = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ", ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if os.Getenv(guardEnv) == "1" {
		os.Exit(runGuard())
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(setupMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	err := run(os.Args[1:])
	switch {
	case err == nil, errors.Is(err, context.Canceled): // a signal ended gff
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
	configPath := fs.String("config", "", "gff's config file (default $GFF_CONFIG, else gff/config.toml in the user config directory)")
	var hostFlags multi
	fs.Var(&hostFlags, "host", "a host for the workspace, as label=endpoint[,endpoint...] (repeatable)")
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
	client := api.ClientInfo{Name: "gff", Version: build, Kind: api.ClientKindTerminal}
	if len(fs.Args()) == 0 && len(news) == 0 {
		specs, err := workspaceHosts(*configPath, hostFlags, tr, *sshHost)
		if err != nil {
			return err
		}
		set, err := hosts.Open(hosts.Config{Source: hosts.Static(specs), Client: client})
		if err != nil {
			return err
		}
		defer set.Close()
		return runTerminal(ctx, in, out, gffclient.Config{Hosts: set, Split: sp, Prefix: prefix})
	}
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := habconn.Connect(octx, habconn.Config{Transport: tr, Client: client})
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

	err = runTerminal(ctx, in, out, gffclient.Config{Conn: conn, Panes: panes, Split: sp, Prefix: prefix})
	if err == nil {
		return errEnded
	}
	return err
}

// runTerminal puts the terminal in raw mode and runs the client until it ends.
func runTerminal(ctx context.Context, in, out int, cfg gffclient.Config) error {
	p, name := parser()
	if !strings.HasPrefix(name, "libghostty") {
		fmt.Fprintln(os.Stderr, "gff: parser", name)
	}
	// The guard sets the terminal back if gff dies with it raw (a panic on any goroutine).
	g, err := startGuard()
	if err != nil {
		return err
	}
	defer g.stop() // runs after restore
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
	cfg.Parser, cfg.In, cfg.Out, cfg.Resized = p, os.Stdin, os.Stdout, resized
	cfg.Size = func() (int, int) { return termSize(out) }
	err = gffclient.Run(ctx, cfg)
	restore()
	if errors.Is(err, context.Canceled) {
		return context.Canceled // a signal ended gff
	}
	return err
}

// workspaceHosts lists the workspace's hosts: the config file's, then each -host flag's. With
// none, the one hab of -socket, -via or -ssh, else the local socket. Config problems are
// printed and the good entries kept.
func workspaceHosts(path string, flags []string, single habconn.Transport, sshHost string) ([]hosts.Spec, error) {
	if path == "" {
		var err error
		if path, err = hosts.DefaultConfigPath(); err != nil {
			return nil, err
		}
	}
	specs, problems, err := hosts.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "gff:", p)
	}
	for _, f := range flags {
		label, eps, ok := strings.Cut(f, "=")
		if !ok || label == "" {
			return nil, fmt.Errorf("-host %q: want label=endpoint[,endpoint...]", f)
		}
		sp := hosts.Spec{Label: label}
		for _, e := range strings.Split(eps, ",") {
			ep, err := hosts.ParseEndpoint(e)
			if err != nil {
				return nil, fmt.Errorf("-host %q: %w", f, err)
			}
			sp.Endpoints = append(sp.Endpoints, ep)
		}
		specs = slices.DeleteFunc(specs, func(s hosts.Spec) bool { return s.Label == label }) // the flag wins
		specs = append(specs, sp)
	}
	if len(specs) == 0 {
		label := "local"
		if sshHost != "" {
			label = sshHost
		}
		specs = []hosts.Spec{{Label: label, Transport: single}}
	}
	return specs, nil
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
