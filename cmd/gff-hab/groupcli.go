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

// groupCmd is `gff-hab group ls|show|pause|resume|close [-socket path] [-json] [group]`: the
// user's side of the groups this hab is the home of (groups.md 15.1). Over the control socket,
// as the user, so pause and resume work here and not from a pane.
func groupCmd(args []string, out io.Writer) error {
	const use = "usage: gff-hab group ls [-state s] | show <group> | pause <group> | resume <group> | close <group>, each with [-socket path] [-json]"
	if len(args) == 0 {
		return errors.New(use)
	}
	verb := args[0]
	fs := flag.NewFlagSet("group "+verb, flag.ExitOnError)
	sock := socketFlag(fs)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	state := fs.String("state", "", "ls: only groups in this state")
	fs.Parse(args[1:])
	method := map[string]string{"ls": "group.list", "show": "group.get", "pause": "group.pause", "resume": "group.resume", "close": "group.close"}[verb]
	switch {
	case method == "":
		return errors.New(use)
	case verb == "ls" && fs.NArg() != 0, verb != "ls" && fs.NArg() != 1:
		return errors.New(use)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := open(*sock, "")
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return err
	}
	if verb == "ls" {
		var r api.GroupList
		if err := c.Call(ctx, method, api.GroupListParams{State: api.GroupState(*state)}, &r); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(r)
		}
		if len(r.Groups) == 0 {
			fmt.Fprintln(out, "no groups")
		}
		for _, g := range r.Groups {
			coord := ""
			if g.Coordinator != nil {
				coord = g.Coordinator.Name
			}
			paused := ""
			if g.Pause != nil {
				paused = " (paused)"
			}
			fmt.Fprintf(out, "%s  %-10s %d/%d landed  %d members  coordinator %s  %s%s\n", g.ID, g.State, g.Landed, g.Items, g.Members, coord, g.Title, paused)
		}
		return nil
	}
	var r api.GroupResult
	if err := c.Call(ctx, method, api.GroupRef{Group: fs.Arg(0)}, &r); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	fmt.Fprint(out, groupText(r.Group))
	return nil
}
