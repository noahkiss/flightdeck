// Package agentcli is `gff agent`, the commands an agent runs about itself from its own pane
// (harness-adapters.md, Self-compact). `gff-hab agent` is the same command, for panes that have
// only gff-hab. Each reaches gff-hab through the pane's GFF_HAB route: the pane's token names
// the agent, so an agent acts only on itself.
package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// Exit codes: 3 for an API error, 64 for a usage error, as the other CLIs.
const (
	exitAPI   = 3
	exitUsage = 64
)

// callTime bounds one call: the hab answers agent.compact at once.
const callTime = 15 * time.Second

// Run runs `<name> agent <verb> ...` and returns the exit code.
func Run(name string, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	usage := func() {
		fmt.Fprintf(stderr, "usage: %s agent compact [-json] [follow-up... | -]\n", name)
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "compact":
		return compact(name, args[1:], stdin, stdout, stderr, getenv)
	}
	fmt.Fprintf(stderr, "%s agent: unknown verb %q\n", name, args[0])
	usage()
	return exitUsage
}

func compact(name string, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("agent compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s agent compact [-json] [follow-up... | -]\n\nAsk gff-hab to compact this agent's conversation once its turn ends, then send the follow-up as\nits first message after the compaction. Runs only in an agent's pane. - reads the follow-up\nfrom stdin.\n", name)
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	followUp := strings.Join(fs.Args(), " ")
	if fs.NArg() == 1 && fs.Arg(0) == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			fmt.Fprintf(stderr, "%s agent compact: %v\n", name, err)
			return 1
		}
		followUp = strings.TrimSpace(string(b))
	}
	addr := getenv(hook.Env)
	if addr == "" {
		fmt.Fprintf(stderr, "%s agent compact runs in an agent's pane: %s is not set\n", name, hook.Env)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTime)
	defer cancel()
	var c api.Compaction
	if err := hook.Call(ctx, addr, "agent.compact", api.CompactParams{FollowUp: followUp}, &c); err != nil {
		fmt.Fprintf(stderr, "%s agent compact: %v\n", name, err)
		var ce *control.Error
		if errors.As(err, &ce) {
			return exitAPI
		}
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	fmt.Fprint(stdout, Text(c))
	return 0
}

// Text says what a queued compaction will do, for an agent.
func Text(c api.Compaction) string {
	then := "."
	if c.FollowUp {
		then = ", then your follow-up arrives as your first message after it."
	}
	return fmt.Sprintf("Compaction %s queued. End your turn now: /compact runs when the turn ends%s\n", c.ID, then)
}
