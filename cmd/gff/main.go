// Command gff is the terminal client (docs/specs/gff-client.md).
//
//	gff [-config path] [-host label=endpoint[,endpoint...]]... [-socket path | -via command | -ssh host]
//	    [-split columns|rows] [-prefix ctrl-<key>] [-workspace name]
//	gff [-config path] [-socket path | -via command | -ssh host] [-new command]...
//	    [-split columns|rows] [-observe] [-prefix ctrl-<key>] [pane...]
//	gff lb [--json] [--socket path] [--state dir] <verb> [args]
//	gff agent compact [-json] [follow-up... | -]
//
// `gff agent compact` runs in an agent's pane: it asks gff-hab to compact the agent's own
// conversation once its turn ends, then send the follow-up (internal/agentcli,
// docs/specs/harness-adapters.md, Self-compact). `gff-hab agent` is the same command.
//
// `gff lb` (long form `gff logbook`) is the Logbook CLI (docs/specs/logbook.md 10.2,
// internal/lbcli). `gff-hab lb` is the same command, for panes that have only gff-hab.
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
// Keys go to the focused pane. The keymap has modes (docs/specs/gff-client.md 8); the bottom
// row shows the keys of the current one. The default keymap, as zellij's:
//
//	Ctrl+G  lock: every key goes to the pane until Ctrl+G again
//	Ctrl+P  pane mode: n new pane stacked onto the focused one, l/r j/d h k new pane on the
//	        right, below, left or above (in the focused pane's folder), p or Tab focus the next
//	        pane, f zoom, s turn the split, x close the focused view (the program keeps running)
//	Ctrl+T  tab mode: n new tab, r rename the tab, x close the tab (its programs keep running),
//	        Tab the last tab, h/l or the arrows previous and next, 1-9 go to a tab
//	Ctrl+O  session mode, one key: w the sidebar, g the status board, b hide or show the bars,
//	        ! the warnings, r redraw (a fresh snapshot from the hab), n nudge (the program
//	        redraws after a one-column resize), f fit (pane.take), d detach (quit gff; the
//	        programs keep running)
//	Alt+h/Alt+l  focus left or right, else the previous or next tab
//	Alt+j/Alt+k  focus down or up      Alt+i/Alt+o  move the tab left or right
//
// The Alt keys work in every mode but locked. A focus or tab move keeps pane or tab mode; another command, Esc or Enter ends it. Each
// leader switches modes from the others, and the current mode's leader pressed again sends it
// to the pane. -prefix moves the session-mode leader. The sidebar (Ctrl+O w): j/k move, Enter
// open, t open in a new tab, n new session, r rename, x end or remove, / filter, p grouping,
// Esc back. docs/examples/gff-keys-prefix.toml is a one-prefix keymap instead (Ctrl+\).
//
// The config file's [keys.<mode>] tables change the keymap, [bar] the bars and [workspace] the
// workspace's name and its startup tabs (LoadSettings in internal/gffclient); -workspace names
// it for one run. The name shows beside the clock. gff saves the workspace's tabs, layout and
// focus to $GFF_WORKSPACE_DIR/<name>.json, else gff/workspaces/<name>.json under the user
// config directory on macOS and under $XDG_STATE_HOME (or ~/.local/state) elsewhere, and
// restores them at the next start; a new workspace starts with the startup tabs.
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
	"github.com/noahkiss/flightdeck/internal/agentcli"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/gffclient"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/lbcli"
	gffversion "github.com/noahkiss/flightdeck/internal/version"
)

// build is the build id, set by the release build.
var build = "dev"

// version is the release version, set at link time: -ldflags "-X main.version=<version>".
var version = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ", ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if os.Getenv(guardEnv) == "1" {
		os.Exit(runGuard())
	}
	if gffversion.Requested(os.Args[1:]) {
		gffversion.Print(os.Stdout, "gff", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(setupMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(agentcli.Run("gff", os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && (os.Args[1] == "lb" || os.Args[1] == "logbook") {
		os.Exit(lbcli.Main(lbcli.Env{Args: os.Args[2:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
			Client: api.ClientInfo{Name: "gff", Version: version, Kind: api.ClientKindCli}, Name: "gff lb"}))
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
	prefixFlag := fs.String("prefix", "", "the key that enters session mode, as ctrl-<key> (default the config's, else ctrl-o)")
	configPath := fs.String("config", "", "gff's config file (default $GFF_CONFIG, else gff/config.toml in the user config directory)")
	workspace := fs.String("workspace", "", "the workspace to open (default the config's [workspace] name, else default)")
	var hostFlags multi
	fs.Var(&hostFlags, "host", "a host for the workspace, as label=endpoint[,endpoint...] (repeatable)")
	fs.Parse(args)

	path := *configPath
	if path == "" {
		var err error
		if path, err = hosts.DefaultConfigPath(); err != nil {
			return err
		}
	}
	settings, problems, err := gffclient.LoadSettings(path)
	if err != nil {
		return err
	}
	if *prefixFlag != "" {
		k, err := gffclient.ParseKey(*prefixFlag)
		if err != nil || !strings.HasPrefix(k, "ctrl-") {
			return fmt.Errorf("-prefix %q: want ctrl-<key>, such as ctrl-\\ or ctrl-a", *prefixFlag)
		}
		problems = append(problems, settings.Keymap.SetPrefix(k)...)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "gff:", p)
	}
	if *workspace != "" {
		if !gffclient.ValidWorkspaceName(*workspace) {
			return fmt.Errorf("-workspace %q: use 1 to 64 letters, digits, dots, dashes and underscores", *workspace)
		}
		settings.Workspace.Name = *workspace
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
		specs, err := workspaceHosts(path, hostFlags, tr, *sshHost)
		if err != nil {
			return err
		}
		set, err := hosts.Open(hosts.Config{Source: hosts.Static(specs), Client: client})
		if err != nil {
			return err
		}
		defer set.Close()
		state, err := gffclient.WorkspacePath(settings.Workspace.Name)
		if err != nil {
			return err
		}
		return runTerminal(ctx, in, out, gffclient.Config{Hosts: set, Split: sp, Keymap: settings.Keymap, Bar: &settings.Bar,
			TitleTemplate: &settings.Workspace.Title, Workspace: settings.Workspace.Name, StatePath: state, StartupTabs: settings.Workspace.Tabs})
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

	err = runTerminal(ctx, in, out, gffclient.Config{TitleTemplate: &settings.Workspace.Title, Conn: conn, Panes: panes, Split: sp, Keymap: settings.Keymap, Bar: &settings.Bar})
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
	specs, problems, err := hosts.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	for _, p := range hostProblems(path, problems) {
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

// hostProblems drops the host reader's "unknown key" problems for the tables the keymap and
// bar reader owns (gffclient.SettingsTables); LoadSettings reports its own.
func hostProblems(path string, problems []string) []string {
	var out []string
	for _, p := range problems {
		owned := false
		for _, t := range gffclient.SettingsTables {
			k := path + ": unknown key " + t
			if p == k || strings.HasPrefix(p, k+".") {
				owned = true
			}
		}
		if !owned {
			out = append(out, p)
		}
	}
	return out
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
