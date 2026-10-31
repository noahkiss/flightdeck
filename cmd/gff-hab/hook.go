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
	"github.com/noahkiss/flightdeck/internal/structure"
)

// hookCmd is what each harness's hook config runs: `gff-hab hook <harness> [event]` with the
// native payload on stdin. It always exits 0 and never writes to stdout, so it can neither fail
// nor slow a turn: the whole run is capped at hook.Budget. `-print <harness>` prints the hook
// config for that harness instead.
func hookCmd(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	print := fs.String("print", "", "print the hook config for this harness (claude, codex, opencode) and exit")
	command := fs.String("command", "gff-hab", "with -print: how the harness should run gff-hab")
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
	timer := time.AfterFunc(hook.Budget+50*time.Millisecond, func() { os.Exit(0) })
	defer timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), hook.Budget)
	defer cancel()
	err := hook.Run(ctx, hook.Invocation{Harness: fs.Arg(0), Event: fs.Arg(1), Stdin: stdin, Getenv: os.Getenv})
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
	store  *structure.Store
	agents *harness.Store
}

func (a agentSource) Snapshot(host api.HostInfo) api.Structure {
	st := a.store.Snapshot(host)
	st.Agents = a.agents.Snapshot()
	return st
}

var errHookServe = errors.New("hook endpoint stopped")
