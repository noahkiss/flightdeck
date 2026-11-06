package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// adoptCmd is `gff-hab adopt`: bring a conversation started outside Flightdeck into a new pane
// (session.adopt), or list the ones it could take (session.outside), gff-client.md 6.5.
func adoptCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	harness := fs.String("harness", "", "claude or codex (default: the one whose transcripts hold the id)")
	name := fs.String("name", "", "session name")
	cwd := fs.String("cwd", "", "folder to resume in (default: the one the transcript records)")
	list := fs.Bool("list", false, "list recent conversations no session holds")
	limit := fs.Int("limit", 0, "with -list: at most this many per harness (default 20)")
	asJSON := fs.Bool("json", false, "print JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "give up after this long")
	fs.Parse(args)
	switch {
	case *list && fs.NArg() != 0:
		return errors.New("adopt -list takes no id")
	case !*list && fs.NArg() != 1:
		return errors.New("adopt needs one id, or -list")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, adoptClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	if *list {
		res, err := cl.SessionOutside(ctx, api.SessionOutsideParams{Harness: *harness, Limit: *limit})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSONTo(out, res)
		}
		printOutside(out, res.Conversations)
		return nil
	}
	res, err := cl.SessionAdopt(ctx, api.SessionAdoptParams{ID: fs.Arg(0), Harness: *harness, Name: *name, Cwd: *cwd})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSONTo(out, res)
	}
	fmt.Fprintf(out, "session %s\npane %s\nharness %s\nfolder %s\n", res.Session, res.Pane, res.Harness, res.Cwd)
	return nil
}

// printOutside prints one conversation per line: harness, id, last write, live mark, folder
// and title.
func printOutside(out io.Writer, list []api.OutsideConversation) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no conversations outside Flightdeck")
		return
	}
	for _, c := range list {
		live := ""
		if c.Live {
			live = " (runs elsewhere)"
		}
		fmt.Fprintf(out, "%-6s %s  %s%s  %s", c.Harness, c.ID, c.Updated, live, c.Cwd)
		if c.Title != "" {
			fmt.Fprintf(out, "  %q", c.Title)
		}
		fmt.Fprintln(out)
	}
}

var adoptClient = api.ClientInfo{Name: "gff-hab adopt", Version: build, Kind: api.ClientKindScript}
