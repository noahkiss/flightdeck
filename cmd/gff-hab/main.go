// Command gff-hab is the logic server on each host (docs/specs/hab-logic-api.md). This build
// serves the control connection on a local Unix socket, with no TCP port, carries the
// structure store, and follows every holder's panes through the holder link (internal/link).
//
//	gff-hab serve   [-socket path] [-hook-socket path] [-state dir] [-host-id id] [-holders dir]
//	                [-scrollback-mib n] [-scrollback-budget-mib n] [-retired-days n] [-peers file]
//	                [-git-interval d] [-init none|systemd|launchd] [-prefix p] [-unit-dir dir] [-config file]
//	gff-hab connect [-socket path]
//	gff-hab api call [-socket path | -via command] <method> [params-json]
//	gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
//	gff-hab peers    [-socket path] [-json]
//	gff-hab peers accept [-socket path] <label>
//	gff-hab pair     [-state dir] [-label name] [-listen addr]... [-port n] [-timeout d] [-authorized-keys path] [-command cmd] [-link] [-no-tty]
//	gff-hab devices list|revoke [-state dir] [-json] [<id|label>]
//	gff-hab notify   [-socket path | -via command] [-level l] [-session id] <title> [body]
//	gff-hab send     [-socket path | -via command] [-from session] [-reply-to id] [-wait d] <address> <text... | ->
//	gff-hab wait     [-socket path | -via command] [-session name] [-agent name] [-for states] [-timeout d] [-json]
//	gff-hab worktree list|create|setup|remove [-socket path | -via command] [-project id | -dir path] ...
//	gff-hab capcom assign [-socket path | -via command] <peer label|self>
//	gff-hab capcom open   [-socket path | -via command] [-new]
//	gff-hab capcom attach|detach [-socket path | -via command] [-host h] <session> <kind> <ref>
//	gff-hab manifests refresh [-state dir] [-from url|path] [-dry-run]
//	gff-hab manifests list    [-state dir]
//	gff-hab detect explain [-socket path | -via command] [-json] [-all] <pane|session>
//	gff-hab read [-socket path] [-json] <tool> [-arg value]... [words]
//	gff-hab lb [--json] [--socket path] [--state dir] <verb> [args]
//	gff-hab mcp [serve] [-socket path] [--skill]
//	gff-hab instructions render -config file [-state dir] [-host-id id] [-dry-run | -write target]
//	gff-hab hook [-t ms] <harness> [event] < payload
//	gff-hab hook -print <harness> [-command name]
//	gff-hab probe -nonce n -checks grant,... [-request grant]
//	gff-hab opencode-pane -report path [--] [opencode argv]
//	gff-hab install   [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
//	                  [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-gff-bin path] [-dry-run]
//	gff-hab uninstall [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
//	                  [-unit-dir dir] [-yes]
//
// The socket path comes from -socket, else $GFF_HAB_SOCKET, else gff/hab.sock under the user
// config directory on macOS and under $XDG_RUNTIME_DIR elsewhere. The state directory comes from
// -state, else $GFF_HAB_STATE, else gff/hab under the user config directory on macOS and under
// $XDG_STATE_HOME (or ~/.local/state) elsewhere. The holder directory comes from -holders, else
// $GFF_HOLDER_DIR, else the holder's own default; screen checkpoints go to <state>/checkpoints.
// The hook socket comes from -hook-socket, else $GFF_HAB_HOOK_SOCKET, else hook.sock beside the
// control socket. Each pane parser's scrollback cap comes from -scrollback-mib, else
// $GFF_HAB_SCROLLBACK_MIB, else 10 MiB. The cap for all parsers together comes from
// -scrollback-budget-mib, else $GFF_HAB_SCROLLBACK_BUDGET_MIB, else 256 MiB; past it each parser
// gets an equal share, at least 1 MiB. The uuid of a pane that is gone stays refused to new
// spawns, across restarts, for -retired-days, else $GFF_HAB_RETIRED_DAYS, else 30 days. Each
// project's repository state (branch, head, ahead, behind) is read again every -git-interval,
// else $GFF_HAB_GIT_INTERVAL, else 60s, besides the reads that hooks and ref changes trigger.
// A pane's screen checkpoint is written once it has printed nothing for $GFF_HAB_CHECKPOINT_IDLE,
// else 1s, and at least every $GFF_HAB_CHECKPOINT_EVERY, else 10s, while it prints.
// Push config comes from -push-config, else <state>/push.json, with $GFF_HAB_PUSH_* over it
// (docs/push.md); with none, the hab runs without push. `gff-hab notify` raises a notice.
// New worktrees go to <root>/<repository folder>/<branch slug>, where root comes from
// -worktrees, else $GFF_HAB_WORKTREES, else ~/.gff/worktrees (hab-logic-api.md 3.10).
//
// With -init systemd or launchd (as the jobs that `gff-hab install` writes run it), gff-hab asks
// the init system for each holder as its own job (internal/service, holder spec 5.4) and removes
// a holder's job once it exits cleanly. With -init none, the default, it starts no holders.
// `gff-hab install` writes the jobs from the same flags and copies the binaries to <state>/bin;
// `gff-hab uninstall` asks before it ends programs (-yes skips that) and removes every file
// install wrote.
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/attach"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/content"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/gitstatus"
	"github.com/noahkiss/flightdeck/internal/habkey"
	"github.com/noahkiss/flightdeck/internal/harness"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbcli"
	"github.com/noahkiss/flightdeck/internal/lbsync"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/opencode"
	"github.com/noahkiss/flightdeck/internal/panesize"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/readtools"
	"github.com/noahkiss/flightdeck/internal/service"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/speech"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
	gffversion "github.com/noahkiss/flightdeck/internal/version"
)

// build is set at link time: -ldflags "-X main.build=<id>".
var build = "dev"

// version is the release version, set at link time: -ldflags "-X main.version=<version>".
var version = "dev"

const usage = `usage:
  gff-hab serve   [-socket path] [-hook-socket path] [-state dir] [-host-id id] [-holders dir]
                  [-scrollback-mib n] [-scrollback-budget-mib n] [-retired-days n] [-peers file] [-config file]
                  [-init none|systemd|launchd] [-prefix p] [-unit-dir dir]
  gff-hab connect [-socket path]
  gff-hab api call [-socket path | -via command] <method> [params-json]
  gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
  gff-hab peers    [-socket path] [-json]
  gff-hab peers accept [-socket path] <label>
  gff-hab pair     [-state dir] [-label name] [-listen addr]... [-port n] [-timeout d] [-authorized-keys path] [-command cmd] [-ssh-port n] [-sshd-keys dir] [-link] [-no-tty]
  gff-hab devices list [-state dir] [-json]
  gff-hab devices revoke [-state dir] <id|label>
  gff-hab notify   [-socket path | -via command] [-level passive|active|time-sensitive] [-session id] <title> [body]
  gff-hab show     [-socket path | -via command] [-session id] [-kind k] [-title t] <path>
  gff-hab send     [-socket path | -via command] [-from session] [-reply-to id] [-wait d] <label/name | name> <text... | ->
  gff-hab wait     [-socket path | -via command] [-session name] [-agent name] [-for state,...] [-timeout d] [-json]
  gff-hab worktree list   [-socket path | -via command] [-project id | -dir path]
  gff-hab worktree create [-socket path | -via command] [-project id | -dir path] [-branch b] [-base ref] [-path dir] [-setup auto|run|skip]
  gff-hab worktree setup  [-socket path | -via command] [-project id | -dir path] <worktree>
  gff-hab worktree remove [-socket path | -via command] [-project id | -dir path] [-force] <worktree>
  gff-hab capcom assign [-socket path | -via command] <peer label|self>
  gff-hab capcom open   [-socket path | -via command] [-new]
  gff-hab capcom attach|detach [-socket path | -via command] [-host h] <session> <kind> <ref>
  gff-hab manifests refresh [-state dir] [-from url|path] [-dry-run]
  gff-hab manifests list    [-state dir]
  gff-hab detect explain [-socket path | -via command] [-json] [-all] <pane|session>
  gff-hab read [-socket path] [-json] <tool> [-arg value]... [words]
  gff-hab lb [--json] [--socket path] [--state dir] <verb> [args]   (the same as gff lb)
  gff-hab act  [-socket path] [-json] <tool> [-arg value]... [words]
  gff-hab confirm list   [-socket path] [-all] [-json]
  gff-hab confirm show   [-socket path] [-json] <id>
  gff-hab confirm answer [-socket path] [-host h -folder f] [-harness h] [-model m] [-seed text | -seed-file path] <id> confirm|cancel
  gff-hab round ls|show|add|edit|rm|pause|unpause|run|runs|export|apply [-socket path | -via command] ...
  gff-hab mcp [serve] [-socket path] [--skill]
  gff-hab instructions render -config file [-state dir] [-host-id id] [-dry-run | -write target]
  gff-hab hook [-t ms] <harness> [event] < payload
  gff-hab hook -print <harness> [-command name]
  gff-hab statusline claude [-timeout d] [-- command...] < status-line input
  gff-hab statusline -print claude [-command name] [-wrap command]
  gff-hab probe -nonce n -checks grant,... [-request grant]
  gff-hab opencode-pane -report path [--] [opencode argv]
  gff-hab install   [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
                    [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-gff-bin path] [-dry-run]
  gff-hab uninstall [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
                    [-unit-dir dir] [-yes]
  gff-hab version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if gffversion.Requested(os.Args[1:]) {
		gffversion.Print(os.Stdout, "gff-hab", version)
		return
	}
	if os.Args[1] == "hook" {
		os.Exit(hookCmd(os.Args[2:], os.Stdin, os.Stdout))
	}
	if os.Args[1] == "statusline" {
		os.Exit(statuslineCmd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Args[1] == "probe" {
		os.Exit(probeCmd(os.Args[2:]))
	}
	if os.Args[1] == "opencode-pane" {
		os.Exit(opencodePaneCmd(os.Args[2:]))
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
	case "peers":
		err = peersCmd(os.Args[2:], os.Stdout)
	case "pair":
		err = pairCmd(os.Args[2:], os.Stdin, os.Stdout)
	case "devices":
		err = devicesCmd(os.Args[2:], os.Stdout)
	case "notify":
		err = notifyCmd(os.Args[2:])
	case "show":
		err = showCmd(os.Args[2:])
	case "send":
		err = sendCmd(os.Args[2:], os.Stdin, os.Stdout)
	case "wait":
		err = waitCmd(os.Args[2:], os.Stdout)
	case "worktree":
		err = worktreeCmd(os.Args[2:])
	case "capcom":
		err = capcomCmd(os.Args[2:], os.Stdout)
	case "manifests":
		err = manifests(os.Args[2:], os.Stdout)
	case "detect":
		err = detectCmd(os.Args[2:], os.Stdout)
	case "read":
		err = readCmd(os.Args[2:], os.Stdout)
	case "act":
		err = actCmd(os.Args[2:], os.Stdout)
	case "confirm":
		err = confirmCmd(os.Args[2:], os.Stdout)
	case "round":
		err = roundCmd(os.Args[2:], os.Stdin, os.Stdout)
	case "mcp":
		err = mcpCmd(os.Args[2:], os.Stdin, os.Stdout)
	case "lb", "logbook":
		os.Exit(lbcli.Main(lbcli.Env{Args: os.Args[2:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
			Getenv: os.Getenv, Client: cliClient, Name: "gff-hab lb"}))
	case "instructions":
		err = instructionsCmd(os.Args[2:], os.Stdout)
	case "install":
		err = install(os.Args[2:], os.Stdout)
	case "uninstall":
		err = uninstall(os.Args[2:], os.Stdin, os.Stdout)
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

// socketDirPoll is how often gff-hab looks again at a socket folder that is open to others.
const socketDirPoll = 2 * time.Second

// waitPrivateSocketDirs waits while the folder of any socket in paths is open to other users,
// which Listen refuses. It logs one line per folder, naming the fix, and none while it waits, so
// a supervisor that restarts gff-hab on exit sees no exit and no hot loop. It returns true when
// SIGTERM or SIGINT came first, false once every folder is private or not there yet. Any other
// fault in a folder is left to Listen.
func waitPrivateSocketDirs(paths []string, sigs <-chan os.Signal, logf func(string, ...any), every time.Duration) (stopped bool) {
	logged := map[string]bool{}
	for {
		var open *control.OpenDirError
		for _, p := range paths {
			if errors.As(control.CheckSocketDir(p), &open) {
				break
			}
		}
		if open == nil {
			for d := range logged {
				logf("socket folder %s is private now; starting", d)
			}
			return false
		}
		if !logged[open.Dir] {
			logged[open.Dir] = true
			logf("socket folder %s is open to other users (mode %v), and gff-hab will not listen there; waiting until it is private: run chmod 700 %q", open.Dir, open.Mode, open.Dir)
		}
		select {
		case <-sigs:
			return true
		case <-time.After(every):
		}
	}
}

func stateDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	return control.DefaultState()
}

// emptyHolders is the pane source when the holder link is off: no panes, no notices.
type emptyHolders struct{}

func (emptyHolders) Panes() ([]structure.HolderPane, error)           { return nil, nil }
func (emptyHolders) Subscribe(func(structure.Notice)) (cancel func()) { return func() {} }

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sock := socketFlag(fs)
	hookSock := fs.String("hook-socket", "", "hook endpoint socket path (default: see gff-hab -h)")
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	hostID := fs.String("host-id", os.Getenv("GFF_HAB_HOST_ID"), "host id sent in the welcome (default: $GFF_HAB_HOST_ID, else the system host name)")
	holders := fs.String("holders", "", "holder registry directory (default: $GFF_HOLDER_DIR, else the holder's default)")
	scrollMiB := fs.Int("scrollback-mib", 0, "scrollback cap per pane in MiB (default: $GFF_HAB_SCROLLBACK_MIB, else 10)")
	retiredDays := fs.Int("retired-days", 0, "days a gone pane's uuid stays refused (default: $GFF_HAB_RETIRED_DAYS, else 30)")
	budgetMiB := fs.Int("scrollback-budget-mib", 0, "scrollback cap for all panes together in MiB (default: $GFF_HAB_SCROLLBACK_BUDGET_MIB, else 256)")
	gitEvery := fs.Duration("git-interval", 0, "read each project's repository state this often (default: $GFF_HAB_GIT_INTERVAL, else 60s)")
	sf := &serviceFlags{
		init:    fs.String("init", os.Getenv("GFF_HAB_INIT"), "init system that starts holders: none, systemd or launchd (default: $GFF_HAB_INIT, else none: holders are started by hand)"),
		prefix:  fs.String("prefix", "", "job name prefix (default: $GFF_SERVICE_PREFIX, else "+service.DefaultPrefix+")"),
		unitDir: fs.String("unit-dir", "", "systemd user unit or LaunchAgents directory (default: the user's)"),
		state:   state, holders: holders, socket: sock,
		build: fs.String("build", "", "launchd: the build directory under <state>/builds this gff-hab runs from (gff-hab install sets it)"),
	}
	peersFile := fs.String("peers", "", "peer list file (default: $GFF_HAB_PEERS, else gff/peers.toml in the user config directory)")
	habConfig := fs.String("config", "", "the hab's config file, with its [catalog] and [speech] sections (default: $GFF_HAB_CONFIG, else gff/hab.toml in the user config folder)")
	pushConfig := fs.String("push-config", "", "push config file (default: <state>/push.json); $GFF_HAB_PUSH_* override its fields")
	permWait := fs.Duration("permission-wait", 0, "hold Claude's PermissionRequest hook up to this long for the app's answer; 0 is off (default: $GFF_HAB_PERMISSION_WAIT, else off)")
	wtRoot := fs.String("worktrees", "", "folder for new worktrees, as <folder>/<repository>/<branch> (default: $GFF_HAB_WORKTREES, else ~/.gff/worktrees)")
	fs.Parse(args)
	logger := log.New(os.Stderr, "gff-hab: ", log.LstdFlags)
	// Catch SIGTERM and SIGINT before any socket is up: a stop that arrives while gff-hab starts
	// waits for the shutdown below instead of killing it with no checkpoint.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	path, err := socketPath(*sock)
	if err != nil {
		return err
	}
	hpath, err := hookSocketPath(*hookSock, path)
	if err != nil {
		return err
	}
	// A socket folder other users can open would fail Listen, and a supervisor would restart
	// gff-hab every second. Wait for the fix instead, before anything starts.
	if stopped := waitPrivateSocketDirs([]string{path, hpath}, sigs, logger.Printf, socketDirPoll); stopped {
		return nil
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

	scrollback, err := scrollbackBytes(*scrollMiB, os.Getenv)
	if err != nil {
		return err
	}
	budget, err := scrollbackBudget(*budgetMiB, os.Getenv)
	if err != nil {
		return err
	}
	ttl, err := retiredTTL(*retiredDays, os.Getenv)
	if err != nil {
		return err
	}
	gitInterval, err := gitInterval(*gitEvery, os.Getenv)
	if err != nil {
		return err
	}
	ckEvery, ckIdle, err := checkpointTimes(os.Getenv)
	if err != nil {
		return err
	}
	permWaitGiven := false
	fs.Visit(func(f *flag.Flag) { permWaitGiven = permWaitGiven || f.Name == "permission-wait" })
	pwaitLock, err := permissionWaitLock(*permWait, permWaitGiven, os.Getenv)
	if err != nil {
		return err
	}
	// The blocking permission hook's gate: its wait is the hab-side setting hab.permission_wait.
	gate := harness.NewGate(0)
	starter, err := serveStarter(sf, logger.Printf)
	if err != nil {
		return err
	}
	lk, err := openLink(*holders, dir, linkOptions{scrollback: scrollback, budget: budget, retiredTTL: ttl, starter: starter,
		checkpointEvery: ckEvery, checkpointIdle: ckIdle}, logger)
	if err != nil {
		return err
	}
	var hs structure.Holders = emptyHolders{}
	hidden := newHiddenPanes(filepath.Join(dir, "macos", probePanesFile), logger.Printf)
	if lk != nil {
		hidden.close = linkCloser(lk)
		hs = hidingHolders{inner: lk.Structure(), hide: hidden}
	}
	var srv atomic.Pointer[control.Server]
	var cat atomic.Pointer[catalog.Service]  // set once the catalog runs; a change pokes it
	var chs atomic.Pointer[channels.Service] // set once channels run; a state change pokes them
	var lbs atomic.Pointer[lbsync.Service]   // set once Logbook sync runs
	pokeCatalog := func() {
		if c := cat.Load(); c != nil {
			c.Poke()
		}
		if c := chs.Load(); c != nil {
			c.Poke() // a session may have exited or been renamed
		}
	}
	var contentRef atomic.Pointer[content.Service] // set once the content service exists
	var attachRef atomic.Pointer[attach.Service]   // set once the attachment store exists
	store, err := structure.Open(structure.Config{
		Dir:     dir,
		Holders: hs,
		OnEvent: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
			if e.Type == structure.SessionDeleted {
				// Shown references go with the session record (app.md 4.7).
				if c := contentRef.Load(); c != nil {
					c.Forget(e.ID)
				}
			}
			pokeCatalog()
		},
	})
	if err != nil {
		return err
	}
	defer store.Close()
	if h := store.Health(); h.Problem != "" {
		logger.Printf("state: %s", h.Problem)
	}
	logManifestDrift(dir, logger.Printf)
	// Repository state per project (hab-logic-api.md 3.9): reads run off the event loop, with a
	// timeout, and change a project only when a value changed.
	repos := gitstatus.New(gitstatus.Config{Store: store, Interval: gitInterval, Logf: logger.Printf})
	defer repos.Close()
	debugf := func(string, ...any) {}
	if os.Getenv("GFF_HAB_DEBUG") != "" {
		debugf = logger.Printf
	}
	var pushes *pushService // set before anything publishes an agent state
	// Harness runs of rounds follow their agents' states (roundharness.go); set once rounds run.
	var roundsRef atomic.Pointer[roundsService]
	agents := harness.NewStore(harness.Config{
		Publish: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
			if r := roundsRef.Load(); r != nil {
				if st, ok := e.Data.(api.AgentState); ok {
					r.agentState(st) // never blocks
				}
			}
			if st, ok := e.Data.(api.AgentState); ok {
				if n := pushes.live(); n != nil {
					n.State(st) // never blocks
				}
			}
			if e.Type == api.EventAgentState {
				pokeCatalog() // pokes channels too
			}
		},
		Logf: debugf,
	})
	// Push (app.md 8): the hab sends to APNs with the user's own key, when it is set up.
	// The Capcom home host, once the session host below exists: banners carry the Ask action
	// while some host runs Capcom (capcom.md 8.5).
	var capcomRef atomic.Pointer[capcomService]
	var readsRef atomic.Pointer[readtools.Service]
	pushes = newPushService(pushSetup{
		// Recent notices, saved in the state directory, for notice.get and Capcom (8.5).
		notices: openNoticeLog(dir, nil, logger.Printf),
		denied: func(folder string) bool {
			r := readsRef.Load()
			return r == nil || r.FolderDenied(folder) // before the read tools exist: hidden
		},
		configPath: *pushConfig, state: dir, hab: *hostID, getenv: os.Getenv, logf: logger.Printf,
		self: key.Fingerprint(),
		capcom: func() string {
			if c := capcomRef.Load(); c != nil {
				return c.Home()
			}
			return ""
		},
		connected: func(c string) bool { s := srv.Load(); return s != nil && s.Connected(c) },
		label: func(c string) string {
			if s := srv.Load(); s != nil {
				return s.ClientLabel(c)
			}
			return ""
		},
		publish: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		},
	}, store, agents)
	defer pushes.Close()
	// A pane's hook token is a keyed hash of its uuid under the secret in the state directory, so
	// it outlives a gff-hab restart. A token resolves only while the link follows the pane: a
	// pane the link reports gone loses its token and its agent state.
	secret, created, err := hook.LoadOrCreateKey(dir)
	if err != nil {
		return err
	}
	if msg := newHookKeyWarning(created, livePanes(lk)); msg != "" {
		logger.Print(msg)
	}
	// A spawn in flight counts as live: its program may call back before the link has the
	// holder's answer (spawner.spawn).
	spawning := newSpawning()
	tokens := hook.NewTokens(secret, livePane(lk, spawning))
	// opencode panes gff-hab starts run a private server; gff-hab follows each one
	// (harness-adapters.md, opencode).
	ocs := newOpencodeServers(dir, secret, agents, logger.Printf)
	defer ocs.Close()
	// Usage readings per harness account (app.md 14): served always; fed by the adapters.
	usageStore := habusage.NewStore(func(e structure.Event) {
		if s := srv.Load(); s != nil {
			s.Publish(e)
		}
	}, nil)
	usages := newUsageService(&usageService{store: usageStore, host: *hostID, logf: logger.Printf})
	defer usages.Close()
	caps := []string{"usage"}
	var acts control.Actions
	var wts control.Worktrees
	var data control.Data
	var input control.Input
	var sizes control.Size
	var chat control.Chat
	var commands control.Commands
	var chats *chatService
	var agentCalls control.Agents
	var agentSvc *agentService
	var explain control.Detect
	var contents control.Content
	var attachments control.Attachments
	var ds *datastream.Server
	var mgr *panesize.Manager
	var sessionActs *actions
	var findTranscript func(harness, native string) (string, error)
	if lk != nil {
		caps = append(caps, "agent-state") // panes gff-hab spawns post hooks
		caps = append(caps, "data-streams", "snapshot-vt")
		ds = datastream.New(datastream.Config{Source: linkSource{lk}, Logf: debugf})
		data, input = dataService{ds}, inputService{lk, agents}
		mgr = newSizes(lk, store, ds, logger.Printf)
		defer mgr.Close()
		sizes = sizeService{mgr}
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/"
		}
		absState, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		sessionActs = &actions{
			sp:       &spawner{lk: lk, tokens: tokens, hookSock: hpath, state: absState, spawning: spawning, logf: logger.Printf},
			store:    store,
			getenv:   os.Getenv,
			environ:  func() []string { return basePaneEnv(os.Environ(), runtime.GOOS) },
			home:     home,
			refresh:  repos.Refresh,
			opencode: ocs,
			peerName: func(name string) string {
				if c := cat.Load(); c != nil {
					return channels.ClashLabel(c.List(), key.Fingerprint(), name)
				}
				return ""
			},
		}
		if exe, err := os.Executable(); err == nil {
			sessionActs.exe = exe
		}
		sessionActs.seed = func(session, from, text string) (string, error) {
			if c := chs.Load(); c != nil {
				return c.Seed(session, from, text)
			}
			return "", errors.New("channels are not running yet")
		}
		acts = sessionActs
		root, err := worktreeRoot(*wtRoot, os.Getenv, home)
		if err != nil {
			return err
		}
		wts = newWorktrees(sessionActs, root, logger.Printf)
		// The chat view reads each session's harness transcript where the harness writes it.
		claudeDir, err := transcript.ClaudeConfigDir(os.Getenv)
		if err != nil {
			return err
		}
		codexHome, err := transcript.CodexHome(os.Getenv)
		if err != nil {
			return err
		}
		// Codex reports usage in its rollout: read the end of it after the session's hooks.
		usages.codexHome = codexHome
		usages.find = func(native string) (string, error) { return transcript.FindCodex(codexHome, native) }
		findTranscript = transcriptFinder(claudeDir, codexHome)
		chats = newChatService(chatConfig{
			session: store.Session,
			agent:   agents.Get,
			find:    findTranscript,
			publish: func(e structure.Event) {
				if s := srv.Load(); s != nil {
					s.Publish(e)
				}
			},
			watched: func(session string) bool {
				s := srv.Load()
				return s != nil && s.Watching(session)
			},
			logf: logger.Printf,
			opencode: func(pane string) (*opencode.Client, string, bool) {
				c, ok := ocs.Client(pane)
				return c, ocs.version(pane), ok
			},
			attachment: func(id string) bool {
				a := attachRef.Load()
				return a != nil && a.Known(id)
			},
			matchSend: func(session, text string, at time.Time) (string, []string, bool) {
				if a := attachRef.Load(); a != nil {
					return a.Match(session, text, at)
				}
				return text, nil, false
			},
		})
		defer chats.Close()
		ocs.dirty = chats.dirty
		chat = chats
		caps = append(caps, "transcripts")
		// Rich content (app.md 4.7): references to files a session showed or touched.
		contentSvc, err := newContent(dir, content.Config{
			Session:      store.Session,
			PaneSession:  paneSessionOf(tokens, store),
			Conversation: chats.conversation,
			ProjectDir:   projectDirOf(store),
			Publish: func(it api.ContentItem) {
				if s := srv.Load(); s != nil {
					s.Publish(structure.Event{Type: control.EventContentUpdated, ID: it.ID, Data: it})
				}
			},
			Logf: logger.Printf,
		})
		if err != nil {
			return err
		}
		defer func() {
			if err := contentSvc.Close(); err != nil {
				logger.Printf("content: %v", err)
			}
		}()
		contents = contentSvc
		contentRef.Store(contentSvc)
		caps = append(caps, "content")
		// Files the owner sends to a session (app.md 4.4), stored in the session's folder.
		attachSvc := attach.New(attach.Config{
			StatePath: filepath.Join(dir, attachmentsFile),
			Session:   func(id string) bool { _, ok := store.Session(id); return ok },
			Folder:    contentSvc.Folder,
			Fallback:  filepath.Join(dir, "attachments"),
			Logf:      logger.Printf,
		})
		defer attachSvc.Close()
		attachments = attachSvc
		attachRef.Store(attachSvc)
		sessionActs.startedIn = contentSvc.NoteFolder
		caps = append(caps, "attachments")
		// Commands from OSC 133 marks (hab-logic-api.md 2.5): command.updated events are read
		// from the link and sent at a bounded rate.
		cmdEvents := newCommandEvents(lk.CommandChanges, func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		})
		defer cmdEvents.Close()
		stopCmds := lk.Watch(cmdEvents.notice)
		defer stopCmds()
		commands = commandService{lk}
		caps = append(caps, "commands")
		agentSvc = &agentService{store: store, agents: agents, gate: gate, input: linkQueuer{lk}, logf: logger.Printf,
			answering: pushes.answering, opencode: ocs, attachments: attachSvc, screen: linkScreen(lk)}
		agentCalls = agentSvc
		// Why a pane shows its state (harness-adapters.md, Detection): read only.
		// The manifests are parsed now, off the start path, and again only when the data
		// directory changes.
		mcache := detect.NewCache(filepath.Join(dir, "manifests"))
		go mcache.Set()
		explain = &detectService{lk: linkPanes{lk}, store: store, agents: agents,
			manifests: mcache.Set, group: detect.GroupProcesses}
		caps = append(caps, "detect-explain")
		// Activity marks for hidden tabs (gff-client.md 15.7): pane.output and pane.bell, at
		// most once a second per pane.
		activity := newActivityEvents(lk, func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		}, activityEvery)
		defer activity.Close()
	}
	// The permission check (macos-identity.md 7.2): the holder's checks run in a hidden probe
	// pane on the current holder; gff-hab's own checks run here.
	pc := permissionsConfig{sf: sf, store: store, state: dir}
	if lk != nil {
		if exe, err := os.Executable(); err == nil {
			pr := &prober{lk: lk, hide: hidden, argv: []string{exe},
				env: func() []string { return basePaneEnv(os.Environ(), runtime.GOOS) }, logf: logger.Printf}
			pc.probe = pr.run
		}
		pc.holderDir = holderDirOf(*holders)
	}
	perms, err := newPermissions(pc)
	if err != nil {
		return err
	}
	caps = append(caps, "permissions")
	// The peer list and one link per peer (mesh.md 3, 4).
	ppath, err := peersPath(*peersFile)
	if err != nil {
		return err
	}
	pm, err := peers.Open(peers.Config{
		Path: ppath, Self: key.Fingerprint(), Host: *hostID, Build: build,
		Publish: func(p api.Peer) {
			if s := srv.Load(); s != nil {
				s.Publish(structure.Event{Type: api.EventPeerUpdated, ID: p.Label, Data: p})
			}
			if c := cat.Load(); c != nil {
				c.Poke() // the peer ids in the catalog
				c.Kick() // a link that came up pulls and puts now
			}
			if c := chs.Load(); c != nil {
				c.Kick() // a link that came up hands over mail now
			}
			if l := lbs.Load(); l != nil {
				l.Kick() // a link that came up syncs Logbook now
			}
		},
		Event: func(label string, e api.Event) {
			if e.Type != api.EventMeshChanged {
				return
			}
			v, err := e.DecodeData()
			mc, _ := v.(api.MeshChanged)
			if c := chs.Load(); c != nil && err == nil && slices.Contains(mc.Reasons, api.MeshReasonMail) {
				c.Woken(label) // the peer holds mail for this hab: pull it now (mesh.md 6.5)
			}
			if l := lbs.Load(); l != nil && err == nil && slices.Contains(mc.Reasons, api.MeshReasonLogbook) {
				l.Woken(label) // the peer holds Logbook ops this hab lacks: sync now (logbook.md 7.1)
			}
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer pm.Close()
	caps = append(caps, peers.Capability)
	// Logbook (logbook.md 4, 7): the replica in <state>/logbook.db, synced over the peer links.
	lbStore, err := openLogbook(context.Background(), dir, key.Fingerprint(), logger.Printf)
	if err != nil {
		return fmt.Errorf("logbook: %w", err)
	}
	defer lbStore.Close()
	lbSvc, err := lbsync.New(lbsync.Config{
		Self: key.Fingerprint(), Store: lbStore, Path: filepath.Join(dir, logbookFile), Peers: logbookPeers{pm},
		Wake: func(device string) bool {
			s := srv.Load()
			return s != nil && s.NotifyLink(device, structure.Event{Type: api.EventMeshChanged, ID: key.Fingerprint(), Data: api.MeshChanged{Reasons: []api.MeshReason{api.MeshReasonLogbook}}}) > 0
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer lbSvc.Close()
	lbs.Store(lbSvc)
	caps = append(caps, lbsync.Capability)
	// The Logbook verbs (logbook.md 10) over the same store: every write tells sync at once, and
	// a watch turns other writers' ops into events.
	lbVerbs, closeVerbs, err := startLogbookVerbs(context.Background(), lbStore, lbSvc.Changed, func(e structure.Event) {
		if s := srv.Load(); s != nil {
			s.Publish(e)
		}
	}, logger.Printf)
	if err != nil {
		return fmt.Errorf("logbook: %w", err)
	}
	defer closeVerbs()
	// The catalog (mesh.md 5): this host's facts, projects and agents, and the peers' copies.
	cpath, err := habConfigPath(*habConfig)
	if err != nil {
		return err
	}
	// The Capcom session host (capcom.md 7): with the holder link, this hab can run Capcom.
	var capcomSvc *capcomService
	var capcomCtl control.Capcom
	if sessionActs != nil {
		exe, err := osExecutable()
		if err != nil {
			return fmt.Errorf("capcom: gff-hab's own path: %w", err)
		}
		capcomSvc = newCapcomService(&capcomService{
			self: key.Fingerprint(), state: dir, configPath: cpath, exe: exe,
			acts: sessionActs, store: store, links: catalogPeers{pm}.Links,
			complete: func() bool { return structure.Complete(hs) },
			claudeTranscript: func(id string) bool {
				d, err := transcript.ClaudeConfigDir(os.Getenv)
				if err != nil {
					return true
				}
				_, err = transcript.FindClaude(d, id)
				return !errors.Is(err, transcript.ErrNoTranscript)
			},
			native: func(pane string) string {
				if st, ok := agents.Get(pane); ok {
					return st.Session
				}
				return ""
			},
			poke: func() {
				if c := cat.Load(); c != nil {
					c.Poke()
				}
			},
			logf: logger.Printf,
		})
		sessionActs.resume = capcomSvc.Resume
		capcomSvc.host = *hostID
		capcomSvc.res.agent = agents.Get
		if commands != nil {
			capcomSvc.res.command = commandByID(commands)
		}
		if agentSvc != nil && agentSvc.attachments != nil {
			store := agentSvc.attachments
			capcomSvc.res.attachment = func(session, id string) (attach.Record, error) {
				recs, err := store.Files(session, []string{id})
				if err != nil {
					return attach.Record{}, err
				}
				return recs[0], nil
			}
		}
		if agentCalls != nil {
			// The owner's next message carries the context attached to it (capcom.md 8.1).
			agentCalls = capcomSender{Agents: agentCalls, cc: capcomSvc}
		}
		capcomCtl = capcomSvc
		capcomRef.Store(capcomSvc)
		caps = append(caps, capcom.Capability)
	}
	// Hab-side settings (app.md 20.5): client writes in <state>/settings.json, locks from the
	// config file's [settings] table and from -permission-wait or $GFF_HAB_PERMISSION_WAIT.
	overrides := map[string]json.RawMessage{}
	if pwaitLock != nil {
		overrides[settings.PermissionWait] = pwaitLock
	}
	setSvc, err := settings.Open(settings.Config{
		State: dir, ConfigPath: cpath, Overrides: overrides,
		Publish: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		},
		Apply: func(id string, v json.RawMessage) {
			applySetting(gate, id, v)
			if id == settings.PermissionWait {
				logger.Printf("settings: %s = %s", id, v)
			}
		},
		Notify: func(title, body string) {
			if _, err := pushes.Raise("", api.NoticeRaiseParams{Title: title, Body: body, Level: api.NoticeLevelActive}); err != nil {
				logger.Printf("settings: notice: %v", err)
			}
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer setSvc.Close()
	caps = append(caps, settings.Capability)
	detectFacts, err := catalog.DetectFor(os.Getenv("GFF_HAB_DETECT"))
	if err != nil {
		return err
	}
	host := api.HostInfo{ID: *hostID}
	cs, err := catalog.New(catalog.Config{
		Self: key.Fingerprint(), Host: *hostID, State: dir, ConfigPath: cpath, Detect: detectFacts,
		Structure:   func() api.Structure { return agentSource{store: store, agents: agents}.Snapshot(host) },
		ProjectDirs: store.ProjectDirs,
		Peers:       catalogPeers{pm},
		Notify: func(p api.NoticeRaiseParams) {
			if _, err := pushes.Raise("", p); err != nil {
				logger.Printf("catalog: notice: %v", err)
			}
		},
		Publish: func(u api.CatalogUpdated) {
			if s := srv.Load(); s != nil {
				s.Publish(structure.Event{Type: control.EventCatalogUpdated, ID: u.DeviceID, Data: u})
			}
		},
		Capcom: func() *api.CatalogCapcom {
			if capcomSvc == nil {
				return nil
			}
			return capcomSvc.Section()
		},
		Rounds: func() []api.CatalogRound {
			if r := roundsRef.Load(); r != nil {
				return r.CatalogSection()
			}
			return nil
		},
		OnCopy: func(c api.Catalog) {
			if capcomSvc != nil {
				capcomSvc.Observe(c)
			}
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer cs.Close()
	cat.Store(cs)
	if capcomSvc != nil {
		capcomSvc.copies = cs.Copies
		// The copies saved by the last run may carry a later assignment than this hab's own.
		for _, c := range cs.Copies() {
			capcomSvc.Observe(c)
		}
	}
	caps = append(caps, catalog.Capability)
	// Channels (mesh.md 6): agent messages by name, the outboxes and the inbox.
	cl := &channelLocal{store: store, agents: agents, send: agentSvc}
	chSvc, err := channels.New(channels.Config{
		Self: key.Fingerprint(), Host: *hostID, State: dir,
		Peers: channelPeers{pm}, Catalog: cs.List, Local: cl,
		Publish: func(r api.ChannelRecord) {
			if s := srv.Load(); s != nil {
				s.Publish(structure.Event{Type: control.EventChannelMessage, ID: r.ID, Data: r})
			}
		},
		Wake: func(device string) bool {
			s := srv.Load()
			return s != nil && s.NotifyLink(device, structure.Event{Type: api.EventMeshChanged, ID: key.Fingerprint(), Data: api.MeshChanged{Reasons: []api.MeshReason{api.MeshReasonMail}}}) > 0
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer chSvc.Close()
	chs.Store(chSvc)
	caps = append(caps, channels.Capability)
	// The push capability is added at each hello while push is on (control.PushCapability):
	// the key command may print its key after start.
	// The read tools (capcom.md 4): sessions and search over the native transcripts, transcript,
	// notice and file reads, each under this hab's read scope, and across the peer links.
	reads := newReads(readsSetup{self: key.Fingerprint(), host: *hostID, state: dir, configPath: cpath,
		store: store, agents: agents, catalog: cs, peers: pm, notices: pushes.notices, getenv: os.Getenv, logf: logger.Printf})
	caps = append(caps, readtools.Capability)
	readsRef.Store(reads)
	if capcomSvc != nil {
		capcomSvc.res.notice = func(ctx context.Context, id, host string) (api.NoticeRecord, error) {
			return reads.Notice(ctx, false, api.NoticeGetParams{ID: id, Host: host})
		}
	}
	// Confirmations (capcom.md 6.1): starts an agent asks for, confirmed by the user while
	// confirm.start_elsewhere is on, run by this hab with the confirmation id as the key.
	cfSvc, err := confirm.New(confirm.Config{
		Runner: &confirmRunner{host: *hostID, catalog: cs.List, links: confirmLinks(pm),
			local: func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
				if s := srv.Load(); s != nil {
					return s.CreateSession(ctx, p)
				}
				return api.SessionCreated{}, control.Errorf(api.ErrorKindUnavailable, "the control server is not running yet")
			},
			name: func(id string) string {
				if ss, ok := store.Session(id); ok {
					return ss.Name
				}
				return ""
			}},
		Enabled: func() bool { return string(setSvc.Value(settings.StartElsewhere)) == "true" },
		Publish: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
		},
		SessionID: func(name string) string {
			if ss, ok := store.SessionByName(name); ok {
				return ss.ID
			}
			return ""
		},
		Notify: func(id, text string) error {
			ss, ok := store.Session(id)
			if !ok {
				return fmt.Errorf("session %s is gone", id)
			}
			_, err := chSvc.Note(ss.Name, text)
			return err
		},
		Targets: confirmTargets(*hostID, confirmLinks(pm)),
		Logf:    logger.Printf,
	})
	if err != nil {
		return err
	}
	defer cfSvc.Close()
	caps = append(caps, confirm.Capability)
	// The speech engine (voice.md): on when its helper is installed and the host setting allows.
	var speechSvc control.Speech
	sp, err := speechSetup(cpath, dir, store, speechEnv{getenv: os.Getenv, executable: os.Executable, memory: speech.HostMemory()}, logger.Printf)
	if err != nil {
		return err
	}
	if sp != nil {
		defer sp.Close()
		speechSvc = sp
		caps = append(caps, "speech")
	}
	// Rounds (rounds.md): scheduled runs in holder panes, from <state>/rounds. They need the
	// holder link: a run is a session.
	var roundsSvc *roundsService
	var roundsCtl control.Rounds
	var pushCtl control.Push = pushes
	if lk != nil {
		rset, err := loadRoundsSettings(cpath)
		if err != nil {
			return err
		}
		sa := acts.(*actions)
		roundsSvc, err = newRounds(roundsConfig{
			state: dir, settings: rset, store: store,
			create: func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error) {
				if s := srv.Load(); s != nil {
					return s.CreateSession(ctx, p)
				}
				return api.SessionCreated{}, control.Errorf(api.ErrorKindUnavailable, "the control server is not running yet")
			},
			pane: func(id string) (link.PaneStatus, bool) {
				u, ok := parseUUID(id)
				if !ok {
					return link.PaneStatus{}, false
				}
				return lk.Pane(u)
			},
			complete:  func() bool { return structure.Complete(lk.Structure()) },
			closePane: sa.ClosePane,
			projectFor: func(ctx context.Context, dir string) (string, error) {
				id, top, err := sa.projectFor(ctx, dir)
				if err == nil {
					// The folder becomes the project's most recent one, where runs start
					// (rounds.md 3), also for a project this call made.
					sa.noteDir(ctx, id, top, false, "")
				}
				return id, err
			},
			publish: func(e structure.Event) {
				if s := srv.Load(); s != nil {
					s.Publish(e)
				}
			},
			raise: func(ev push.RoundEvent, detail, full, folder string) {
				pushes.raiseRound(ev, detail, full, folder)
			},
			changed: cs.Poke,
			remoteDir: func(remote string) string {
				for _, h := range cs.List().Hosts {
					if !h.Own {
						continue
					}
					for _, p := range h.Catalog.Projects {
						if p.Remote == remote && len(p.Folders) > 0 {
							return p.Folders[0].Path
						}
					}
				}
				return ""
			},
			tick: roundTick(),
			logf: logger.Printf,

			agent: agents.Get,
			send: func(ctx context.Context, session, text string, guard func(string) error) error {
				return agentSvc.sendGuarded(ctx, "round", api.SessionSendParams{Session: session, Text: text}, guard)
			},
			sendHeld: agentSvc.sendHeld,
			agents:   agents.Snapshot,
			folder: func(ctx context.Context, session string) string {
				if c := contentRef.Load(); c != nil {
					return c.Folder(ctx, session)
				}
				return ""
			},
			resume: func(ctx context.Context, session string, p api.SessionCreateParams, cwd string) (string, error) {
				return resumeExited(ctx, sa, session, p, cwd)
			},
			interrupt: agentSvc.interruptTurn,
			result: func(ctx context.Context, session, kind, native string) (string, error) {
				return readResult(ctx, func(ctx context.Context) ([]api.ChatMessage, error) {
					return roundMessages(ctx, chats, findTranscript, session, kind, native)
				})
			},
			messages: func(ctx context.Context, session, kind, native string) ([]api.ChatMessage, error) {
				return roundMessages(ctx, chats, findTranscript, session, kind, native)
			},
			argv: func(ctx context.Context, pid int) ([]string, error) {
				fg, err := detect.GroupProcesses(ctx, pid)
				if err != nil {
					return nil, err
				}
				if fg.Leader.PID != pid {
					return nil, fmt.Errorf("process %d is not its group's leader", pid)
				}
				return fg.Leader.Args, nil
			},
			screen: func(pane string) (string, bool) {
				u, ok := parseUUID(pane)
				if !ok {
					return "", false
				}
				rows, _, err := lk.VisibleRows(u)
				if err != nil {
					return "", false
				}
				return detect.ScreenText(rows), true
			},
			ready: roundReadySeam,
			quiet: roundQuietSeam,
		})
		if err != nil {
			return err
		}
		defer roundsSvc.Close()
		roundsRef.Store(roundsSvc)
		cs.Poke() // the catalog's rounds section
		roundsCtl = roundsSvc
		acts = roundActions{actions: sa, rounds: roundsSvc}
		pushCtl = roundPush{Push: pushes, rounds: roundsSvc}
		input = roundInput{Input: input, rounds: roundsSvc}
		agentCalls = roundAgents{Agents: agentCalls, rounds: roundsSvc}
		caps = append(caps, "rounds")
	}
	s := control.New(control.Config{
		Build:        build,
		Host:         api.HostInfo{ID: *hostID},
		DeviceID:     key.Fingerprint(),
		Source:       agentSource{store: store, agents: agents, settings: setSvc, confirms: cfSvc, rounds: roundsSvc},
		Capabilities: caps,
		Actions:      acts,
		Data:         data,
		Input:        input,
		Size:         sizes,
		Chat:         chat,
		Commands:     commands,
		Agents:       agentCalls,
		ApproversChanged: func() {
			if s := srv.Load(); s != nil && agentSvc != nil {
				agentSvc.releaseUnwatched(s.Approvers)
			}
		},
		Logf:        logger.Printf,
		Permissions: permissionService{perms},
		Detect:      explain,
		Peers:       pm,
		Catalog:     cs,
		Push:        pushCtl,
		Usage:       usageStore,
		Content:     contents,
		Channels:    chSvc,
		LogbookSync: lbSvc,
		Worktrees:   wts,
		Speech:      speechSvc,
		Attachments: attachments,
		Settings:    setSvc,
		Reads:       reads,
		Confirm:     cfSvc,
		Capcom:      capcomCtl,
		Rounds:      roundsCtl,
		Logbook:     lbVerbs,
	})
	srv.Store(s)
	l, err := control.Listen(path)
	if err != nil {
		return err
	}
	hl, err := control.Listen(hpath)
	if err != nil {
		l.Close()
		return err
	}
	if lk != nil {
		if roundsSvc != nil {
			stopRounds := lk.Watch(roundsSvc.notice)
			defer stopRounds()
		}
		stopWatch := lk.Watch(func(n link.Notice) {
			switch n.Kind {
			case link.PaneAdded: // a holder reached late: its opencode panes are followed now
				ocs.added(n.Pane.UUID.String())
			case link.PaneGone:
				ocs.forget(n.Pane.UUID.String())
				agents.Forget(n.Pane.UUID.String())
				pushes.forget(n.Pane.UUID.String())
				mgr.Forget(n.Pane.UUID.String())
			case link.PaneExited:
				ocs.forget(n.Pane.UUID.String()) // the wrapper is gone, and its server with it
				agents.Exited(n.Pane.UUID.String())
			case link.PaneReport: // OSC 7501 from the program; a replayed one applies as screen
				agents.Report(n.Pane.UUID.String(), n.Report, time.Now().UnixMilli(), n.Live)
			case link.PanePrompt: // a shell prompt is back: stale OSC working or blocked ends
				agents.Prompt(n.Pane.UUID.String())
			}
		})
		defer stopWatch()
		// The opencode servers of panes that outlived the last gff-hab. A pane of a holder the
		// link has not reached yet is followed on its PaneAdded.
		ocs.load(func(pane string) (hp.UUID, bool) {
			u, ok := parseUUID(pane)
			if !ok {
				return u, false
			}
			_, live := livePane(lk, nil)(u)
			return u, live
		})
		// Replays that ended before the watcher was added: the last report of each.
		for _, n := range lk.ReplayedReports() {
			agents.Report(n.Pane.UUID.String(), n.Report, time.Now().UnixMilli(), false)
		}
	}
	hooks := hook.NewServer(tokens, func(pane string, env harness.Envelope) string {
		if usages.Hook(env) {
			return "" // a status-line input: usage only
		}
		_, opened := agents.Hook(pane, env)
		// A hook from a pane in a project may follow a commit or a checkout: read the project's
		// repository again, debounced.
		repos.Refresh(store.ProjectOfPane(pane))
		return opened
	}, logger.Printf)
	hooks.SetChannels(channelHook(store, chSvc))
	hooks.SetReads(readHook(reads, cs, pm))
	hooks.SetConfirm(confirmHook(store, cfSvc))
	hooks.SetLogbook(logbookHook(lbVerbs, store, agents, *hostID))
	if roundsSvc != nil {
		hooks.SetRounds(roundHook(store, roundsSvc))
	}
	if agentSvc != nil {
		hooks.SetHolder(agentSvc.holdPermission(s.Approvers))
	}
	if roundsSvc != nil {
		// After the link watch, so no exit of an adopted run is missed, and before either
		// server accepts a call, so a client's input to an adopted run takes it over.
		roundsSvc.start()
	}
	go func() {
		if err := hooks.Serve(hl); err != nil {
			logger.Printf("%v: %v", errHookServe, err)
		}
	}()
	defer hooks.Close()
	logger.Printf("epoch %s build %s device %s listening on %s, hooks on %s", s.Epoch(), build, key.Fingerprint(), path, hpath)

	stopped := make(chan struct{})
	go func() {
		sig := <-sigs
		logger.Printf("%v: shutting down", sig)
		s.Close() // sends closing (shutdown) to every client and waits for it to flush
		if ds != nil {
			ds.Close() // clients resume each stream from its offset (spec 7.4)
		}
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
	if method == "confirm.answer" && os.Getenv(hook.Env) != "" {
		// The same cheap guard as gff-hab confirm answer (capcom.md 6.4).
		return errors.New("confirm.answer is the owner's answer; it does not run in an agent's pane")
	}
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
