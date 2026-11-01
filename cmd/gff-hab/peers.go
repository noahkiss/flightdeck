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
	"github.com/noahkiss/flightdeck/internal/peers"
)

// peersPath is the peer file: -peers, else $GFF_HAB_PEERS, else gff/peers.toml in the user
// config directory (mesh.md 3.1).
func peersPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	return peers.DefaultPath()
}

// peersCmd is `gff-hab peers` and `gff-hab peers accept <label>`. Both ask the running hab:
// only it knows each peer's link, and the id a refused peer presented.
func peersCmd(args []string, out io.Writer) error {
	accept := len(args) > 0 && args[0] == "accept"
	if accept {
		args = args[1:]
	}
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	sock := socketFlag(fs)
	asJSON := fs.Bool("json", false, "print the peers.list result as JSON")
	fs.Parse(args)
	switch {
	case accept && fs.NArg() != 1:
		return errors.New("peers accept needs one label")
	case !accept && fs.NArg() != 0:
		return errors.New("usage: gff-hab peers [-socket path] [-json], or gff-hab peers accept [-socket path] <label>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := open(*sock, "")
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	if accept {
		r, err := cl.PeersAccept(ctx, api.PeersAcceptParams{Label: fs.Arg(0)})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "accepted %s\n  was: %s\n  now: %s\n", r.Label, orNone(r.Old), r.New)
		return nil
	}
	r, err := cl.PeersList(ctx, api.PeersListParams{})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	printPeers(out, r)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// printPeers writes the peer table: label, state, endpoint, short device id, last seen, and
// why a peer is not online (mesh.md 4.3).
func printPeers(out io.Writer, r api.PeerList) {
	if len(r.Peers) == 0 && len(r.Unlisted) == 0 && len(r.Problems) == 0 {
		fmt.Fprintln(out, "no peers")
		return
	}
	if len(r.Peers) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "LABEL\tSTATE\tENDPOINT\tDEVICE\tLAST SEEN\tNOTE")
		for _, p := range r.Peers {
			state := strings.ReplaceAll(string(p.State), "_", " ")
			if p.Inbound && p.State == api.PeerStateOnline {
				state += " (in)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Label, state, dash(p.Endpoint), dash(shortID(p.DeviceID)), dash(lastSeen(p.LastSeen)), p.Reason)
		}
		tw.Flush()
	}
	for _, u := range r.Unlisted {
		fmt.Fprintf(out, "linked from %s (%s), not in your peer list\n", u.Host, shortID(u.DeviceID))
	}
	for _, p := range r.Problems {
		fmt.Fprintf(out, "problem: %s\n", p)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortID is a device id cut to its first 8 fingerprint characters, as lists show it
// (transport.md 5).
func shortID(id string) string {
	const prefix = "SHA256:"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	return id
}

// lastSeen shows an RFC 3339 time in local time, to the second.
func lastSeen(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
