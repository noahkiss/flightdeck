package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/panetext"
)

// actionCmd is `gff-hab action` (hab-logic-api.md 3.13): a named action on a session. The door is
// picked as for type: -socket or -via, else GFF_HAB from a pane, else the default control socket.
// From an agent's pane the hab refuses it: only the owner and the Capcom service run it.
func actionCmd(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("action", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	asJSON := fs.Bool("json", false, "print the session.action answer as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: gff-hab action [-socket path | -via command] [-json] <session | label/name> <compact|clear|restart|close|recreate> [follow-up... | -]\n\n"+
			"Run a named action on a session; the hab translates it for the harness in its pane. compact and clear keep\n"+
			"the pane; restart starts the harness again with its resume id; close ends the session; recreate starts a\n"+
			"fresh session under the same name. The old conversation stays resumable. A follow-up (compact, clear,\n"+
			"recreate) is the first message after the action; - reads it from stdin. The answer comes at once, in state\n"+
			"queued: the action waits for the agent's turn to end.\n\n")
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), driveNote)
	}
	fs.Parse(args)
	if fs.NArg() < 2 {
		fs.Usage()
		os.Exit(2)
	}
	followUp := strings.Join(fs.Args()[2:], " ")
	if fs.NArg() == 3 && fs.Arg(2) == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, 4*maxFollowUp+1))
		if err != nil {
			return err
		}
		followUp = strings.TrimRight(string(b), "\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, done, err := paneCallerFor(ctx, *sock, *via)
	if err != nil {
		return err
	}
	defer done()
	var r api.SessionAction
	p := api.SessionActionParams{Session: fs.Arg(0), Action: api.SessionActionKind(fs.Arg(1)), FollowUp: followUp}
	if err := c.Call(ctx, "session.action", p, &r); err != nil {
		return cliErr(err)
	}
	clean := func(s string) string { return panetext.StripBidi(panetext.CleanControls(s)) }
	r.ID, r.Session, r.State = clean(r.ID), clean(r.Session), clean(r.State)
	if *asJSON {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", b)
		return nil
	}
	fmt.Fprintf(out, "%s %s on %s: %s\n", r.ID, r.Action, r.Session, r.State)
	return nil
}

// capcomTools are the tools only the Capcom engine's server lists (`gff-hab mcp -capcom`): a
// session action on any agent Capcom may drive (hab-logic-api.md 3.13).
var capcomTools = []kitTool{
	{Name: "session_action", Action: true, Drives: true, Shell: "gff-hab action <target> <compact|clear|restart|close|recreate> [follow-up]",
		Doc: "Run a named action on a session, to keep an agent fresh: compact (compact its conversation in place), clear (a fresh conversation in place), restart (start the harness again with its conversation), close (end the session), recreate (a fresh session under the same name, with follow_up as its first message). Use this, never the harness's own slash command, where an action exists. Answers at once with state queued; the action waits for the agent's turn to end. Refused where the owner's settings do not let you type into the session's pane; compact and clear answer unavailable for a harness without a translation: use recreate.",
		Args: []toolArg{argTarget, {Name: "action", Type: "string", Doc: "compact, clear, restart, close or recreate.", Required: true},
			{Name: "follow_up", Type: "string", Doc: "compact, clear and recreate: the first message after the action, at most 8000 characters."}},
		run: runSessionAction},
}

func runSessionAction(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	if err := fromPaneRoute(c); err != nil {
		return nil, "", err
	}
	var r api.SessionAction
	p := api.SessionActionParams{Session: a.str("target"), Action: api.SessionActionKind(a.str("action")), FollowUp: a.str("follow_up")}
	if err := c.Call(ctx, "session.action", p, &r); err != nil {
		return nil, "", err
	}
	return r, fmt.Sprintf("Session action %s (%s on %s) is %s.\n", panetext.CleanControls(r.ID), r.Action, panetext.CleanControls(r.Session), panetext.CleanControls(r.State)), nil
}
