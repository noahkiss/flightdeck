package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbcli"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// hookCmd is what each harness's hook config runs: `gff-hab hook <harness> [event]` with the
// native payload on stdin. It always exits 0, so it can never fail a turn, and the whole run is
// capped at hook.Budget. `-print <harness>` prints the hook config for that harness instead.
//
// At a context event (hook.ContextEvent: a session's start, a prompt, a stop, and Claude Code's
// PostToolUse) it prints what gff-hab adds for the agent in the harness's hook output shape: the
// Logbook brief, the reminder or the closeout (logbook.md 11.3), and on Claude Code's prompt and
// tool hooks the context nudge (harness-adapters.md), joined when both are due. When gff-hab does not answer at the brief's event, it builds
// the brief from the Logbook file itself; the reminder and the closeout are then skipped. At
// every other event it prints nothing.
//
// `-wait` is the blocking permission hook, for the PermissionRequest of Claude Code and Codex. gff-hab
// answers at once unless the owner turned it on (`serve -permission-wait`); then the command
// waits for the app's answer and prints it as the harness's hook output. The answer's header must
// still come within hook.Budget, so a hung gff-hab costs no more than that; the whole run is
// capped at hook.WaitBudget.
func hookCmd(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stamp := fs.Int64("t", 0, "the event's time in ms since the Unix epoch, when the caller stamps it (default: now)")
	print := fs.String("print", "", "print the hook config for this harness (claude, codex, opencode) and exit")
	command := fs.String("command", "gff-hab", "with -print: how the harness should run gff-hab")
	wait := fs.Bool("wait", false, "for claude and codex PermissionRequest: wait for the app's answer when gff-hab holds the hook, and print it")
	// -context was a flag of the context nudge before it asked at every context event; a config
	// written then still parses.
	fs.Bool("context", false, "ignored: the context events ask for context with no flag")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	if *print != "" {
		cfg, err := hook.Config(*print, *command)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gff-hab: %v\n", err)
			return 2
		}
		fmt.Fprint(stdout, cfg)
		return 0
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return 0
	}
	// The hard cap: whatever happens, exit 0 within the budget.
	budget := hook.Budget
	if *wait {
		budget = hook.WaitBudget
	}
	timer := time.AfterFunc(budget+50*time.Millisecond, func() { os.Exit(0) })
	defer timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	inv := hook.Invocation{Harness: fs.Arg(0), Event: fs.Arg(1), T: *stamp, Stdin: stdin, Getenv: os.Getenv, Stdout: stdout,
		Wait: *wait, FileBrief: fileBrief}
	err := hook.Run(ctx, inv)
	if err != nil && os.Getenv("GFF_HOOK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "gff-hab hook: %v\n", err)
	}
	return 0
}

// hookSocketPath picks the hook socket: -hook-socket, else $GFF_HAB_HOOK_SOCKET, else hook.sock
// beside the control socket.
func hookSocketPath(flagValue, controlSocket string) (string, error) {
	p := flagValue
	if p == "" {
		p = os.Getenv("GFF_HAB_HOOK_SOCKET")
	}
	if p == "" {
		p = filepath.Join(filepath.Dir(controlSocket), "hook.sock")
	}
	return filepath.Abs(p)
}

// agentSource adds the agent state of every pane to the structure snapshot.
type agentSource struct {
	store    *structure.Store
	agents   *harness.Store
	settings *settings.Service // nil: no settings in the snapshot
	confirms confirmService    // nil: no confirmations in the snapshot
	rounds   *roundsService    // nil: no rounds in the snapshot
	groups   *groupsService    // nil: no groups in the snapshot
}

func (a agentSource) Snapshot(host api.HostInfo) api.Structure {
	st := a.store.Snapshot(host)
	st.Agents = a.agents.Snapshot()
	if a.settings != nil {
		st.Settings = a.settings.List()
	}
	if a.confirms != nil {
		if open := openOnly(a.confirms.List(false)); len(open) > 0 {
			st.Confirmations = open
		}
	}
	if a.rounds != nil {
		if rs := a.rounds.List(); len(rs) > 0 {
			st.Rounds = rs
		}
	}
	if a.groups != nil {
		st.Groups = a.groups.Snapshot()
	}
	return st
}

// openOnly keeps the open confirmations: a running one has its answer already.
func openOnly(cs []api.Confirmation) []api.Confirmation {
	var out []api.Confirmation
	for _, c := range cs {
		if c.State == "open" {
			out = append(out, c)
		}
	}
	return out
}

var errHookServe = errors.New("hook endpoint stopped")

// fileBrief is the brief from the Logbook file, for a hook that finds gff-hab down (logbook.md
// 11.3): the state directory from $GFF_HAB_STATE, which gff-hab sets in its panes, and the
// habit switches from the hab's config file, $GFF_HAB_CONFIG or the default.
func fileBrief(ctx context.Context, cwd string) (string, error) {
	cpath, _ := habConfigPath("") // unknown: no file, so the defaults
	b, err := lbcli.FileBrief(ctx, lbcli.Env{Getenv: os.Getenv, Dir: cwd}, cpath)
	return b.Text, err
}
