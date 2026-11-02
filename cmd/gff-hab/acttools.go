package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/suggest"
)

// actionCallTime bounds one action tool call: the 120 s wait for the user, then a start's run.
const actionCallTime = confirm.MaxWait + confirm.RunTime + 10*time.Second

// runSuggest ranks the hosts from the merged catalog (capcom.md 3.3). It reads only.
func runSuggest(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	cl, err := readCatalog(ctx, c)
	if err != nil {
		return nil, "", err
	}
	var needs []string
	for _, n := range strings.Split(a.str("needs"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			needs = append(needs, n)
		}
	}
	r := suggest.Rank(cl, suggest.Request{Project: a.str("project"), Branch: a.str("branch"), Needs: needs,
		Harness: a.str("harness"), Host: a.str("host")})
	var b strings.Builder
	line := func(prefix string, c suggest.Candidate) {
		fmt.Fprintf(&b, "%s %s", prefix, c.Host)
		if c.Folder != "" {
			fmt.Fprintf(&b, ", folder %s", c.Folder)
		}
		fmt.Fprintf(&b, ": %s\n", strings.Join(c.Reasons, "; "))
	}
	if r.Top != nil {
		line("suggest", *r.Top)
	}
	for _, n := range r.Next {
		line("also", n)
	}
	for _, e := range r.Excluded {
		fmt.Fprintf(&b, "not %s: %s\n", e.Host, e.Reason)
	}
	if r.Why != "" {
		fmt.Fprintf(&b, "no host fits: %s\n", r.Why)
	}
	if r.SetupOn != "" {
		fmt.Fprintf(&b, "offer the user a set-up on %s (set_up_project)\n", r.SetupOn)
	}
	return r, b.String(), nil
}

func runConfirmationRead(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.ConfirmationResult
	if err := c.Call(ctx, "confirm.get", api.ConfirmGetParams{ID: a.str("id")}, &r); err != nil {
		return nil, "", err
	}
	return r, confirmationText(r.Confirmation), nil
}

// runStart asks the hab for a start and waits up to 120 s for the answer (capcom.md 6.1).
func runStart(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.ConfirmRequestParams{Kind: "start", WaitMS: int(confirm.MaxWait / time.Millisecond), Start: &api.ConfirmStart{
		Host: a.str("host"), Folder: a.str("folder"), Harness: a.str("harness"), Model: a.str("model"),
		Name: a.str("name"), Seed: a.str("seed")}}
	var r api.ConfirmRequested
	if err := c.Call(ctx, "confirm.request", p, &r); err != nil {
		return nil, "", err
	}
	return r, confirmationText(r.Confirmation), nil
}

func runSetup(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.ConfirmRequestParams{Kind: "setup", Setup: &api.ConfirmSetup{Host: a.str("host"), Repository: a.str("repository"), Branch: a.str("branch")}}
	var r api.ConfirmRequested
	if err := c.Call(ctx, "confirm.request", p, &r); err != nil {
		return nil, "", err
	}
	return r, confirmationText(r.Confirmation), nil
}

// confirmationText says where a confirmation stands, for an agent.
func confirmationText(c api.Confirmation) string {
	var b strings.Builder
	what := "the start"
	if s := c.Start; s != nil {
		what = fmt.Sprintf("the start of %s (%s) in %s on %s", s.Name, s.Harness, s.Folder, s.Host)
	}
	switch c.State {
	case "open":
		fmt.Fprintf(&b, "Still waiting for the owner to confirm %s. Confirmation %s. Do not ask again: the answer reaches you as a message from Flightdeck, and confirmation_read shows where it stands.\n", what, c.ID)
	case "running":
		fmt.Fprintf(&b, "The owner confirmed %s; it is running now. Confirmation %s.\n", what, c.ID)
	case "done":
		name := c.Start.Name
		if c.Result != nil && c.Result.Name != "" {
			name = c.Result.Name
		}
		host := c.Start.Host
		if c.Result != nil && c.Result.Host != "" {
			host = c.Result.Host
		}
		how := "after the owner confirmed it"
		if c.Record {
			how = "at once, with no confirmation asked"
		}
		if c.Edited {
			how += ", with the owner's edits"
		}
		fmt.Fprintf(&b, "Started session %s on %s, %s. Confirmation %s.\n", name, host, how, c.ID)
		if c.Start.Seed != "" {
			b.WriteString("Its first message is your seed; it arrives once the agent is ready.\n")
		}
	case "cancelled":
		fmt.Fprintf(&b, "The owner cancelled %s. Nothing ran. Confirmation %s.\n", what, c.ID)
	case "expired":
		fmt.Fprintf(&b, "No answer within 60 minutes: %s expired and nothing ran. Confirmation %s.\n", what, c.ID)
	case "unknown":
		fmt.Fprintf(&b, "The outcome of %s is unknown: %s. Check the agents tool for %s before you offer a retry. Confirmation %s.\n", what, c.Reason, c.Start.Name, c.ID)
	default:
		fmt.Fprintf(&b, "%s did not run (%s): %s. Confirmation %s.\n", strings.ToUpper(what[:1])+what[1:], c.State, c.Reason, c.ID)
	}
	return b.String()
}
