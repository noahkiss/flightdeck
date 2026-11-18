// Package agentcli is `gff agent`, the commands an agent runs about itself from its own pane
// (harness-adapters.md, Self-compact and Agent kit): compact its conversation, and name its
// session. `gff-hab agent` is the same command, for panes that have
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
		fmt.Fprintf(stderr, "usage: %s agent compact [-json] [follow-up... | -]\n       %s agent name [-json] <name>\n", name, name)
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "compact":
		return compact(name, args[1:], stdin, stdout, stderr, getenv)
	case "name":
		return rename(name, args[1:], stdout, stderr, getenv)
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

// rename runs `agent name <name>`: the pane's session takes the name, which is its channel
// address on this host (mesh.md 6.1).
func rename(name string, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("agent name", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the renamed session as JSON")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s agent name [-json] <name>\n\nName this agent's session: other agents send to it by this name, or label/name from another\nhost. 1 to 64 of A-Z a-z 0-9 . _ -, unique on this host and its peers. Runs only in an\nagent's pane.\n", name)
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	addr := getenv(hook.Env)
	if addr == "" {
		fmt.Fprintf(stderr, "%s agent name runs in an agent's pane: %s is not set\n", name, hook.Env)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTime)
	defer cancel()
	var s api.Session
	if err := hook.Call(ctx, addr, "agent.name", api.AgentNameParams{Name: fs.Arg(0)}, &s); err != nil {
		fmt.Fprintf(stderr, "%s agent name: %v\n", name, err)
		var ce *control.Error
		if errors.As(err, &ce) {
			return exitAPI
		}
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	fmt.Fprint(stdout, NameText(s))
	return 0
}

// NameText says what a rename did, for an agent.
func NameText(s api.Session) string {
	return fmt.Sprintf("Your session is now named %s. Other agents send to you as %s on this host, or <label>/%s from another.\n", s.Name, s.Name, s.Name)
}

// Text says what a queued compaction will do, for an agent.
func Text(c api.Compaction) string {
	then := "."
	if c.FollowUp {
		then = ", then your follow-up arrives as your first message after it."
	}
	return fmt.Sprintf("Compaction %s queued. End your turn now: /compact runs when the turn ends%s\n", c.ID, then)
}
