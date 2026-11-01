// Command gff-hab is the logic server on each host (docs/specs/hab-logic-api.md). This build
// serves the control connection on a local Unix socket, with no TCP port, carries the
// structure store, and follows every holder's panes through the holder link (internal/link).
//
//	gff-hab serve   [-socket path] [-hook-socket path] [-state dir] [-host-id id] [-holders dir]
//	                [-scrollback-mib n] [-scrollback-budget-mib n] [-retired-days n] [-peers file]
//	                [-git-interval d] [-init none|systemd|launchd] [-prefix p] [-unit-dir dir]
//	gff-hab connect [-socket path]
//	gff-hab api call [-socket path | -via command] <method> [params-json]
//	gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
//	gff-hab peers    [-socket path] [-json]
//	gff-hab peers accept [-socket path] <label>
//	gff-hab notify   [-socket path | -via command] [-level l] [-session id] <title> [body]
//	gff-hab manifests refresh [-state dir] [-from url|path] [-dry-run]
//	gff-hab manifests list    [-state dir]
//	gff-hab detect explain [-socket path | -via command] [-json] [-all] <pane|session>
//	gff-hab hook [-t ms] <harness> [event] < payload
//	gff-hab hook -print <harness> [-command name]
//	gff-hab probe -nonce n -checks grant,... [-request grant]
//	gff-hab install   [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
//	                  [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-dry-run]
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/gitstatus"
	"github.com/noahkiss/flightdeck/internal/habkey"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/panesize"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/service"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
)

// build is set at link time: -ldflags "-X main.build=<id>".
var build = "dev"

const usage = `usage:
  gff-hab serve   [-socket path] [-hook-socket path] [-state dir] [-host-id id] [-holders dir]
                  [-scrollback-mib n] [-scrollback-budget-mib n] [-retired-days n] [-peers file]
                  [-init none|systemd|launchd] [-prefix p] [-unit-dir dir]
  gff-hab connect [-socket path]
  gff-hab api call [-socket path | -via command] <method> [params-json]
  gff-hab events   [-socket path | -via command] [-scope host|projects:id,...|sessions:id,...]
  gff-hab peers    [-socket path] [-json]
  gff-hab peers accept [-socket path] <label>
  gff-hab notify   [-socket path | -via command] [-level passive|active|time-sensitive] [-session id] <title> [body]
  gff-hab manifests refresh [-state dir] [-from url|path] [-dry-run]
  gff-hab manifests list    [-state dir]
  gff-hab detect explain [-socket path | -via command] [-json] [-all] <pane|session>
  gff-hab hook [-t ms] <harness> [event] < payload
  gff-hab hook -print <harness> [-command name]
  gff-hab statusline claude [-timeout d] [-- command...] < status-line input
  gff-hab statusline -print claude [-command name] [-wrap command]
  gff-hab probe -nonce n -checks grant,... [-request grant]
  gff-hab install   [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
                    [-unit-dir dir] [-hab-bin path] [-holder-bin path] [-dry-run]
  gff-hab uninstall [-init systemd|launchd] [-prefix p] [-state dir] [-holders dir] [-socket path]
                    [-unit-dir dir] [-yes]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
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
	case "notify":
		err = notifyCmd(os.Args[2:])
	case "manifests":
		err = manifests(os.Args[2:], os.Stdout)
	case "detect":
		err = detectCmd(os.Args[2:], os.Stdout)
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
	pushConfig := fs.String("push-config", "", "push config file (default: <state>/push.json); $GFF_HAB_PUSH_* override its fields")
	permWait := fs.Duration("permission-wait", 0, "hold Claude's PermissionRequest hook up to this long for the app's answer; 0 is off (default: $GFF_HAB_PERMISSION_WAIT, else off)")
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
	pwait, err := permissionWait(*permWait, os.Getenv)
	if err != nil {
		return err
	}
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
	agents := harness.NewStore(harness.Config{
		Publish: func(e structure.Event) {
			if s := srv.Load(); s != nil {
				s.Publish(e)
			}
			if st, ok := e.Data.(api.AgentState); ok && pushes != nil && pushes.notifier != nil {
				pushes.notifier.State(st) // never blocks
			}
		},
		Logf: debugf,
	})
	// Push (app.md 8): the hab sends to APNs with the user's own key, when it is set up.
	pushes = newPushService(pushSetup{
		configPath: *pushConfig, state: dir, hab: *hostID, getenv: os.Getenv, logf: logger.Printf,
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
	hpath, err := hookSocketPath(*hookSock, path)
	if err != nil {
		return err
	}
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
	tokens := hook.NewTokens(secret, livePane(lk))
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
	var data control.Data
	var input control.Input
	var sizes control.Size
	var chat control.Chat
	var commands control.Commands
	var chats *chatService
	var agentCalls control.Agents
	var agentSvc *agentService
	var explain control.Detect
	var ds *datastream.Server
	var mgr *panesize.Manager
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
		acts = &actions{
			sp:      &spawner{lk: lk, tokens: tokens, hookSock: hpath},
			store:   store,
			getenv:  os.Getenv,
			environ: func() []string { return basePaneEnv(os.Environ(), runtime.GOOS) },
			home:    home,
			refresh: repos.Refresh,
		}
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
		chats = newChatService(chatConfig{
			session: store.Session,
			agent:   agents.Get,
			find:    transcriptFinder(claudeDir, codexHome),
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
		})
		defer chats.Close()
		chat = chats
		caps = append(caps, "transcripts")
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
		agentSvc = &agentService{store: store, agents: agents, gate: harness.NewGate(pwait), input: linkQueuer{lk}, logf: logger.Printf,
			answering: pushes.answering}
		agentCalls = agentSvc
		// Why a pane shows its state (harness-adapters.md, Detection): read only.
		mdir := filepath.Join(dir, "manifests")
		explain = &detectService{lk: linkPanes{lk}, store: store, agents: agents,
			manifests: func() *detect.Set { return detect.Load(mdir) }, group: detect.GroupProcesses}
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
		},
		Logf: logger.Printf,
	})
	if err != nil {
		return err
	}
	defer pm.Close()
	caps = append(caps, peers.Capability)
	if pushes.Enabled() {
		caps = append(caps, "push")
	}
	s := control.New(control.Config{
		Build:        build,
		Host:         api.HostInfo{ID: *hostID},
		DeviceID:     key.Fingerprint(),
		Source:       agentSource{store: store, agents: agents},
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
		Push:        pushes,
		Usage:       usageStore,
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
		stopWatch := lk.Watch(func(n link.Notice) {
			switch n.Kind {
			case link.PaneGone:
				agents.Forget(n.Pane.UUID.String())
				pushes.forget(n.Pane.UUID.String())
				mgr.Forget(n.Pane.UUID.String())
			case link.PaneExited:
				agents.Exited(n.Pane.UUID.String())
			case link.PaneReport: // OSC 7501 from the program; a replayed one applies as screen
				agents.Report(n.Pane.UUID.String(), n.Report, time.Now().UnixMilli(), n.Live)
			case link.PanePrompt: // a shell prompt is back: stale OSC working or blocked ends
				agents.Prompt(n.Pane.UUID.String())
			}
		})
		defer stopWatch()
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
	if agentSvc != nil {
		hooks.SetHolder(agentSvc.holdPermission(s.Approvers))
		if pwait > 0 {
			logger.Printf("blocking permission hook on: Claude's PermissionRequest waits up to %v for the app's answer", pwait)
		}
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
