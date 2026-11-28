package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/diaglog"
	"github.com/noahkiss/flightdeck/internal/friction"
)

var frictionClient = api.ClientInfo{Name: "gff friction", Version: build, Kind: api.ClientKindCli}

const frictionUsage = `usage: gff-hab friction [-socket path | -via command] [-session name|id] [-pane id] [-blocker] <text...>
       gff-hab friction list [-state dir] [-days n]
       gff-hab friction show [-state dir] <trace>

A friction note records a moment where Flightdeck fell short: your own words, the session, and
the hab's merged log of the last 10 minutes. -blocker marks work you could not do in Flightdeck;
without it the note is a preference. gff friction is the same command.
`

// frictionCmd is gff-hab friction (diagnostics.md 9): make a note through diag.friction, or
// read the notes this host's hab stored. list and show read the files, so they work while
// gff-hab is down.
func frictionCmd(args []string, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return frictionList(args[1:], out, time.Now())
		case "show":
			return frictionShow(args[1:], out)
		}
	}
	fs := flag.NewFlagSet("friction", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), frictionUsage); fs.PrintDefaults() }
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	session := fs.String("session", "", "the session the note is about, by name or id")
	pane := fs.String("pane", "", "the pane the note is about")
	blocker := fs.Bool("blocker", false, "the work could not be done in Flightdeck")
	timeout := fs.Duration("timeout", 10*time.Second, "give up after this long")
	fs.Parse(args)
	text, err := friction.CheckText(strings.Join(fs.Args(), " "))
	if err != nil {
		return fmt.Errorf("%v; see gff-hab friction -h", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return fmt.Errorf("%v: the note is not kept; is gff-hab running? (gff-hab status)", err)
	}
	defer c.Close()
	if _, err := c.Hello(ctx, frictionClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	p := api.FrictionParams{Text: text, Pane: *pane, Blocker: *blocker}
	if *session != "" {
		st, err := cl.StructureGet(ctx, api.StructureGetParams{})
		if err != nil {
			return err
		}
		for _, s := range st.Sessions {
			if s.ID == *session || s.Name == *session {
				p.Session = s.ID
				break
			}
		}
		if p.Session == "" {
			return fmt.Errorf("no session %q", *session)
		}
	}
	res, err := cl.DiagFriction(ctx, p)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "friction note %s stored\n", res.Trace)
	return err
}

func frictionList(args []string, out io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("friction list", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	days := fs.Int("days", 30, "the notes of the last n days")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("friction list takes no arguments")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	notes, bad, err := friction.List(frictionDir(dir), now.Add(-time.Duration(*days)*24*time.Hour))
	if err != nil {
		return err
	}
	if len(notes) == 0 {
		fmt.Fprintf(out, "no friction notes in the last %d days\n", *days)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, n := range notes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.T.Local().Format("2006-01-02 15:04"), n.Trace, orDash(n.Session),
			kindWord(n.Blocker), oneLine(n.Text, 100))
	}
	tw.Flush()
	if bad > 0 {
		fmt.Fprintf(out, "%d note files did not parse\n", bad)
	}
	return nil
}

func frictionShow(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("friction show", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("friction show needs one trace; gff-hab friction list prints them")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	n, err := friction.Find(frictionDir(dir), fs.Arg(0))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "trace\t%s\n", n.Trace)
	fmt.Fprintf(tw, "time\t%s\n", n.T.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(tw, "kind\t%s\n", kindWord(n.Blocker))
	fmt.Fprintf(tw, "client\t%s\n", orDash(n.Client))
	fmt.Fprintf(tw, "session\t%s\n", orDash(n.Session))
	fmt.Fprintf(tw, "pane\t%s\n", orDash(n.Pane))
	fmt.Fprintf(tw, "hab\tversion %s, build %s\n", orDash(n.Version), orDash(n.Build))
	tw.Flush()
	fmt.Fprintf(out, "\n%s\n\n", n.Text)
	switch {
	case n.Dropped:
		fmt.Fprintln(out, "log tail: dropped after 90 days")
	case len(n.Log) == 0:
		fmt.Fprintln(out, "log tail: no lines")
	default:
		cut := ""
		if n.LogCut > 0 {
			cut = fmt.Sprintf(", %d older lines cut to fit", n.LogCut)
		}
		fmt.Fprintf(out, "log tail: %d lines%s\n", len(n.Log), cut)
		for _, l := range n.Log {
			text := l.Text
			if p, ok := diaglog.Parse(string(l.Line)); ok { // a structured line (diagnostics.md 4.2)
				text = p.Text()
				if l.Text != "" {
					text += "\n" + l.Text // its continuation lines
				}
			} else if p, ok := diaglog.Parse(text); ok { // a note written before Line held it
				text = p.Text()
			}
			fmt.Fprintf(out, "%s %-15s %s\n", l.T.Local().Format("2006-01-02 15:04:05.000000"), l.Proc, text)
		}
	}
	return nil
}

func kindWord(blocker bool) string {
	if blocker {
		return "blocker"
	}
	return "preference"
}

// oneLine is s on one line, cut to n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
