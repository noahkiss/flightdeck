package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

var restartClient = api.ClientInfo{Name: "gff-hab restart", Version: build, Kind: api.ClientKindScript}

// restartCmd is `gff-hab restart`: restart one session through its harness's resume
// (session.restart, instructions-mcp-flight-rules.md 13.1), by name or id. With -list it prints
// the running sessions that work from an older render.
func restartCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: gff-hab restart [-socket path | -via command] [-json] <session>\n       gff-hab restart -list [-socket path | -via command] [-json]\n\nExit an idle or done session's harness and start it again with its resume id, so it loads\nits instructions anew. -list prints the sessions flagged stale.")
		fs.PrintDefaults()
	}
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	list := fs.Bool("list", false, "list the sessions that work from an older render")
	asJSON := fs.Bool("json", false, "print JSON")
	timeout := fs.Duration("timeout", control.RestartTimeout+10*time.Second, "give up after this long")
	fs.Parse(args)
	switch {
	case *list && fs.NArg() != 0:
		return errors.New("restart -list takes no session")
	case !*list && fs.NArg() != 1:
		return errors.New("restart needs one session, by name or id, or -list")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, restartClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	st, err := cl.StructureGet(ctx, api.StructureGetParams{})
	if err != nil {
		return err
	}
	if *list {
		var stale []api.Session
		for _, s := range st.Sessions {
			if len(s.Stale) > 0 {
				stale = append(stale, s)
			}
		}
		if *asJSON {
			return printJSONTo(out, stale)
		}
		if len(stale) == 0 {
			fmt.Fprintln(out, "no session works from an older render")
		}
		for _, s := range stale {
			fmt.Fprintf(out, "%s %s %v\n", s.ID, s.Name, s.Stale)
		}
		return nil
	}
	id := ""
	for _, s := range st.Sessions {
		if s.ID == fs.Arg(0) || s.Name == fs.Arg(0) {
			id = s.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("no session %q", fs.Arg(0))
	}
	res, err := cl.SessionRestart(ctx, api.SessionRef{Session: id})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSONTo(out, res)
	}
	fmt.Fprintf(out, "session %s\npane %s\n", res.Session, res.Pane)
	return nil
}
