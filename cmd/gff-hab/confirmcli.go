package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// confirmCmd is the CLI fallback for confirmations until the app shows them (capcom.md 6.1):
//
//	gff-hab confirm list   [-socket path] [-all] [-json]
//	gff-hab confirm show   [-socket path] [-json] <id>
//	gff-hab confirm answer [-socket path] [-host h -folder f] [-harness h] [-model m] [-seed text | -seed-file path] <id> confirm|cancel
func confirmCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: gff-hab confirm list|show|answer ...")
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("confirm "+verb, flag.ExitOnError)
	sock := socketFlag(fs)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	all := fs.Bool("all", false, "list: also the closed confirmations the hab keeps")
	var ed api.ConfirmEdits
	fs.StringVar(&ed.Host, "host", "", "answer: start on this host instead (give -folder too)")
	fs.StringVar(&ed.Folder, "folder", "", "answer: start in this folder instead")
	fs.StringVar(&ed.Harness, "harness", "", "answer: start this harness instead")
	fs.StringVar(&ed.Model, "model", "", "answer: start with this model instead")
	fs.StringVar(&ed.Seed, "seed", "", "answer: replace the seed with this text")
	seedFile := fs.String("seed-file", "", "answer: replace the seed with this file's text")
	fs.Parse(args)
	switch verb {
	case "list":
		if fs.NArg() != 0 {
			return errors.New("confirm list takes no arguments")
		}
	case "show":
		if fs.NArg() != 1 {
			return errors.New("confirm show needs one id")
		}
	case "answer":
		if fs.NArg() != 2 || (fs.Arg(1) != "confirm" && fs.Arg(1) != "cancel") {
			return errors.New("usage: gff-hab confirm answer [edits] <id> confirm|cancel")
		}
		// A cheap guard: an agent's pane carries GFF_HAB, and an agent never answers its own
		// confirmation (capcom.md 6.1).
		if os.Getenv(hook.Env) != "" {
			return errors.New("confirm answer is the owner's answer; it does not run in an agent's pane")
		}
		if *seedFile != "" {
			b, err := os.ReadFile(*seedFile)
			if err != nil {
				return err
			}
			ed.Seed = string(b)
		}
	default:
		return fmt.Errorf("no confirm command %q: list, show or answer", verb)
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionCallTime)
	defer cancel()
	c, err := open(*sock, "")
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return err
	}
	switch verb {
	case "list":
		var r api.ConfirmList
		if err := c.Call(ctx, "confirm.list", api.ConfirmListParams{All: *all}, &r); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(r)
		}
		for _, x := range r.Confirmations {
			io.WriteString(out, confirmationCard(x))
		}
		if len(r.Confirmations) == 0 {
			fmt.Fprintln(out, "no open confirmations")
		}
		return nil
	case "show":
		var r api.ConfirmationResult
		if err := c.Call(ctx, "confirm.get", api.ConfirmGetParams{ID: fs.Arg(0)}, &r); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(r)
		}
		_, err := io.WriteString(out, confirmationCard(r.Confirmation))
		return err
	}
	p := api.ConfirmAnswerParams{ID: fs.Arg(0), Choice: api.ConfirmChoice(fs.Arg(1))}
	if ed != (api.ConfirmEdits{}) {
		p.Edits = &ed
	}
	var r api.ConfirmationResult
	if err := c.Call(ctx, "confirm.answer", p, &r); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	_, err = io.WriteString(out, confirmationCard(r.Confirmation))
	return err
}

// confirmationCard prints a confirmation as the app's card shows it (capcom.md 6.3).
func confirmationCard(c api.Confirmation) string {
	var b strings.Builder
	state := string(c.State)
	if c.Record {
		state += ", a record: it ran at once"
	}
	fmt.Fprintf(&b, "%s %s: %s\n", c.ID, c.Kind, state)
	if c.From != "" {
		fmt.Fprintf(&b, "  asked by:  %s\n", c.From)
	}
	if s := c.Start; s != nil {
		fmt.Fprintf(&b, "  host:      %s\n  project:   %s\n  folder:    %s\n", s.Host, orNone(s.Project), s.Folder)
		if s.Branch != "" {
			fmt.Fprintf(&b, "  branch:    %s\n", s.Branch)
		}
		fmt.Fprintf(&b, "  harness:   %s\n", s.Harness)
		if s.Model != "" {
			fmt.Fprintf(&b, "  model:     %s\n", s.Model)
		}
		fmt.Fprintf(&b, "  name:      %s\n", s.Name)
		if s.Seed != "" {
			fmt.Fprintf(&b, "  seed:\n%s\n", indent(s.Seed))
		}
	}
	if c.ExpiresAt != "" && c.State == "open" {
		if t, err := time.Parse(time.RFC3339Nano, c.ExpiresAt); err == nil {
			fmt.Fprintf(&b, "  expires:   %s (in %s)\n", c.ExpiresAt, time.Until(t).Round(time.Minute))
		}
	}
	if c.Result != nil {
		fmt.Fprintf(&b, "  started:   %s on %s (session %s)\n", c.Result.Name, c.Result.Host, c.Result.Session)
	}
	if c.Reason != "" {
		fmt.Fprintf(&b, "  reason:    %s\n", c.Reason)
	}
	if c.AnsweredBy != "" {
		fmt.Fprintf(&b, "  answered:  by %s", c.AnsweredBy)
		if c.Edited {
			b.WriteString(", edited")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
