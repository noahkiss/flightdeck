package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/panetext"
)

// `gff-hab panes`, `gff-hab type` and `gff-hab new` (hab-logic-api.md 3.12). The door is picked
// as for screen and wait -pane: -socket or -via, else GFF_HAB from a pane, else the default
// control socket.

const driveNote = `
Exit codes: 0 done; 1 local error; 2 usage; 3 the hab refused the call.
`

// panesCmd is `gff-hab panes`: the panes the caller may read.
func panesCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("panes", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	host := fs.String("host", "", "list this host only: a peer label, or this hab's host id")
	asJSON := fs.Bool("json", false, "print the pane.list answer as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: gff-hab panes [-socket path | -via command] [-host label] [-json]\n\n"+
			"List the panes you may read, here and on connected hosts. From a pane, agent.read_panes decides which.\n\n")
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), driveNote)
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, done, err := paneCallerFor(ctx, *sock, *via)
	if err != nil {
		return err
	}
	defer done()
	var r api.PaneList
	if err := c.Call(ctx, "pane.list", api.PaneListParams{Host: *host}, &r); err != nil {
		return cliErr(err)
	}
	cleanList(&r)
	if *asJSON {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", b)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSESSION\tPROJECT\tSTATE\tAGENT\tPANE")
	for _, p := range r.Panes {
		name := or(p.Session, "-")
		if p.Own {
			name += " (you)"
		}
		agent := "-"
		if p.Agent != "" {
			agent = p.Agent + " " + string(p.AgentState)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Host, name, or(p.ProjectName, "-"), or(string(p.State), "-"), agent, p.Pane)
	}
	tw.Flush()
	for _, u := range r.Unreached {
		fmt.Fprintf(out, "%s: not reached (%s)\n", u.Label, u.Reason)
	}
	return nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// cleanList drops control and bidi characters from every string of a pane.list answer: rows
// may come from another hab.
func cleanList(r *api.PaneList) {
	c := func(s string) string { return panetext.StripBidi(panetext.CleanControls(s)) }
	for i, p := range r.Panes {
		r.Panes[i] = api.PaneListEntry{Pane: c(p.Pane), Session: c(p.Session), SessionID: c(p.SessionID), Project: c(p.Project),
			ProjectName: c(p.ProjectName), Host: c(p.Host), Own: p.Own, State: api.SessionState(c(string(p.State))),
			Agent: c(p.Agent), AgentState: api.AgentStatus(c(string(p.AgentState)))}
	}
	for i, u := range r.Unreached {
		r.Unreached[i] = api.PaneUnreached{Label: c(u.Label), Reason: c(u.Reason)}
	}
}

// typeCmd is `gff-hab type`: type text into a pane, under the send gate.
func typeCmd(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("type", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	enter := fs.Bool("enter", false, "press Enter after the text")
	asJSON := fs.Bool("json", false, "print the pane.type answer as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: gff-hab type [-socket path | -via command] [-enter] [-json] <pane | session | label/name> <text... | ->\n\n"+
			"Type text into a pane; - reads it from stdin. The hab refuses, and types nothing, while the pane's agent\n"+
			"waits for a permission answer, while another message or type to it is in flight, or within 2 s of a\n"+
			"person typing there: try again later. From a pane, agent.drive_panes decides which panes you may type into.\n\n")
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), driveNote)
	}
	fs.Parse(args)
	if fs.NArg() < 1 || (fs.NArg() == 1 && !*enter) {
		fs.Usage()
		os.Exit(2)
	}
	text := strings.Join(fs.Args()[1:], " ")
	if fs.NArg() == 2 && fs.Arg(1) == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, paneTypeMax+1))
		if err != nil {
			return err
		}
		text = string(b)
	}
	if len(text) > paneTypeMax {
		return fmt.Errorf("the text is over %d bytes", paneTypeMax)
	}
	pane, session := paneTargetArg(fs.Arg(0))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, done, err := paneCallerFor(ctx, *sock, *via)
	if err != nil {
		return err
	}
	defer done()
	var r api.PaneTyped
	if err := c.Call(ctx, "pane.type", api.PaneTypeParams{Pane: pane, Session: session, Text: text, Enter: *enter}, &r); err != nil {
		return cliErr(err)
	}
	if *asJSON {
		r.Pane, r.Session = panetext.CleanControls(r.Pane), panetext.CleanControls(r.Session)
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", b)
	}
	return nil
}

// newCmd is `gff-hab new`: start a shell or a program in a new pane on this hab.
func newCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	cwd := fs.String("cwd", "", "the absolute folder to start in (default: your pane's folder, else the hab's home)")
	project := fs.Bool("project", false, "file the new session in your own pane's project")
	name := fs.String("name", "", "the new session's name (default: the program's name)")
	asJSON := fs.Bool("json", false, "print the pane.new answer as JSON")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: gff-hab new [-socket path | -via command] [-cwd dir] [-project] [-name n] [-json] [-- argv...]\n\n"+
			"Start a shell, or the program argv, in a new pane on this hab. It prints the session name, the session id\n"+
			"and the pane id. From a pane, agent.drive_panes decides whether you may.\n\n")
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), driveNote)
	}
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, done, err := paneCallerFor(ctx, *sock, *via)
	if err != nil {
		return err
	}
	defer done()
	var r api.PaneStarted
	if err := c.Call(ctx, "pane.new", api.PaneNewParams{Argv: fs.Args(), Cwd: *cwd, InProject: *project, Name: *name}, &r); err != nil {
		return cliErr(err)
	}
	if *asJSON {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", b)
		return nil
	}
	fmt.Fprintf(out, "%s %s %s\n", r.Name, r.Session, r.Pane)
	return nil
}
