package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// Rounds on other hosts (rounds.md 9): `round <verb> -host <label>` acts on a peer's rounds
// through the peer's endpoint, `round ls -all` reads every host's rounds from the catalog, and
// `round move` moves a fresh round to another runner.

// errHostOffline starts the error of a call to a runner that is not online (rounds.md 11):
// edits never queue for a sleeping host.
const errHostOffline = "host offline"

// peerCaller opens a control connection to the peer label names in the peer list of the hab
// behind c: through the endpoint that peer's link uses, from this machine. The peer must be
// online, and the hab that answers must present the device id the peer list pinned.
func peerCaller(ctx context.Context, c caller, label string) (caller, func(), error) {
	var l api.PeerList
	if err := c.Call(ctx, "peers.list", api.PeersListParams{}, &l); err != nil {
		return nil, nil, err
	}
	var p *api.Peer
	for i := range l.Peers {
		if l.Peers[i].Label == label {
			p = &l.Peers[i]
		}
	}
	if p == nil {
		return nil, nil, fmt.Errorf("no peer %q in this hab's peer list (gff-hab peers)", label)
	}
	if p.State != api.PeerStateOnline {
		msg := fmt.Sprintf("%s: %s is %s", errHostOffline, label, p.State)
		if p.LastSeen != "" {
			msg += ", last seen " + p.LastSeen
		}
		if p.Reason != "" {
			msg += " (" + p.Reason + ")"
		}
		return nil, nil, errors.New(msg)
	}
	ep, err := hosts.ParseEndpoint(p.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("peer %s: %w", label, err)
	}
	tr, err := ep.Transport()
	if err != nil {
		return nil, nil, fmt.Errorf("peer %s: %w", label, err)
	}
	rw, err := tr.Dial(ctx, habconn.KindControl)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %s: %w", errHostOffline, label, err)
	}
	pc := control.NewClient(rw)
	w, err := pc.Hello(ctx, cliClient, "", "")
	if err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("%s: %s: %w", errHostOffline, label, err)
	}
	if p.DeviceID != "" && w.DeviceID != p.DeviceID {
		pc.Close()
		return nil, nil, fmt.Errorf("peer %s: the hab at %s presents %s, not the pinned %s", label, p.Endpoint, shortID(w.DeviceID), shortID(p.DeviceID))
	}
	return pc, func() { pc.Close() }, nil
}

// localRemote is the git remote of the project this folder belongs to, from the own catalog of
// the hab behind c: the project a round on another host names.
func localRemote(ctx context.Context, c caller) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wds := []string{wd}
	if r, err := filepath.EvalSymlinks(wd); err == nil && r != wd {
		wds = append(wds, r)
	}
	var cl api.CatalogList
	if err := c.Call(ctx, "catalog.list", api.CatalogListParams{}, &cl); err != nil {
		return "", err
	}
	best, remote := -1, ""
	for _, h := range cl.Hosts {
		if !h.Own {
			continue
		}
		for _, p := range h.Catalog.Projects {
			for _, f := range p.Folders {
				paths := []string{f.Path}
				if r, err := filepath.EvalSymlinks(f.Path); err == nil && r != f.Path {
					paths = append(paths, r)
				}
				for _, fp := range paths {
					for _, w := range wds {
						if (w == fp || strings.HasPrefix(w, fp+string(filepath.Separator))) && len(fp) > best {
							best, remote = len(fp), p.Remote
						}
					}
				}
			}
		}
	}
	switch {
	case best < 0:
		return "", fmt.Errorf("%s is not a project folder in this hab's catalog: give -remote, or -project with the folder on the other host", wd)
	case remote == "":
		return "", fmt.Errorf("the project at %s has no remote: give -project with the folder on the other host", wd)
	}
	return remote, nil
}

// hostRound is one row of `round ls -all`: a round and the host whose catalog lists it.
type hostRound struct {
	Host   string           `json:"host"`
	Label  string           `json:"label,omitempty"`
	Own    bool             `json:"own,omitempty"`
	Stale  bool             `json:"stale,omitempty"`
	SeenAt string           `json:"seen_at"`
	Round  api.CatalogRound `json:"round"`
}

// hostRounds flattens the rounds sections of catalog.list, in host order.
func hostRounds(cl api.CatalogList) []hostRound {
	out := []hostRound{}
	for _, h := range cl.Hosts {
		for _, r := range h.Catalog.Rounds {
			out = append(out, hostRound{Host: h.Catalog.Host, Label: h.Label, Own: h.Own, Stale: h.Stale, SeenAt: h.SeenAt, Round: r})
		}
	}
	return out
}

func printHostRounds(out io.Writer, rows []hostRound) {
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tNAME\tMODE\tTRIGGER\tNEXT\tLAST RUN\tSTATUS")
	for _, r := range rows {
		host := r.Label
		if host == "" {
			host = r.Host
		}
		if r.Own {
			host += " (here)"
		}
		status := "active"
		switch {
		case r.Round.Completed:
			status = "completed"
		case r.Round.Paused:
			status = "paused"
		}
		if r.Stale {
			status += "; host asleep since " + r.SeenAt
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", host, r.Round.Name, r.Round.Mode, r.Round.Trigger,
			orDash(r.Round.NextSlot), orDash(string(r.Round.LastRun)), status)
	}
	tw.Flush()
}

// moveRound moves a fresh round from the hab behind from to the peer behind to (rounds.md 9),
// after the user confirms: it pauses the round here, so no slot runs on both hosts, creates it
// on the peer with the same definition and pause mark, its project named by remote, then
// deletes it here. A failed create unpauses it here again. Resume and fork rounds stay: their
// target session lives on this host. The run history stays behind, and goes with the delete.
func moveRound(ctx context.Context, from, to caller, name, label string, yes, asJSON bool, stdin io.Reader, out io.Writer) error {
	var got api.RoundResult
	if err := from.Call(ctx, "round.get", api.RoundRef{Name: name}, &got); err != nil {
		return err
	}
	r := got.Round
	if r.Mode != api.RoundModeFresh {
		return fmt.Errorf("round %s is a %s round: its target session lives on this host, so it cannot move", name, r.Mode)
	}
	if r.Cwd != "" {
		return fmt.Errorf("round %s starts in %s, a folder on this host: clear -cwd with round edit first", name, r.Cwd)
	}
	spec := r.RoundSpec
	if r.Project != "" {
		if r.ProjectRemote == "" {
			return fmt.Errorf("round %s: its project has no remote, so the other host cannot name it", name)
		}
		spec.Project, spec.ProjectDir = "", ""
	}
	if !yes && !askYes(stdin, out, fmt.Sprintf("Move round %s to %s? It is created there and deleted here, with its run history. [y/N] ", name, label)) {
		return errors.New("not moved")
	}
	if !r.Paused {
		if err := from.Call(ctx, "round.pause", api.RoundRef{ID: r.ID}, nil); err != nil {
			return fmt.Errorf("pause round %s here: %w", name, err)
		}
	}
	var made api.RoundCreated
	if err := to.Call(ctx, "round.create", api.RoundCreateParams{Round: spec}, &made); err != nil {
		if !r.Paused {
			if uerr := from.Call(ctx, "round.unpause", api.RoundRef{ID: r.ID}, nil); uerr != nil {
				return fmt.Errorf("create round %s on %s: %w; and it stays paused here: %v", name, label, err, uerr)
			}
		}
		return fmt.Errorf("create round %s on %s: %w; it stays here", name, label, err)
	}
	if err := from.Call(ctx, "round.delete", api.RoundRef{ID: r.ID}, nil); err != nil {
		return fmt.Errorf("round %s now runs on %s, but deleting it here failed: %w; it is paused here, remove it with gff-hab round rm %s", name, label, err, name)
	}
	if asJSON {
		return printJSONTo(out, made)
	}
	fmt.Fprintf(out, "moved %s to %s: next %s (was %s)\n", name, label, orDash(made.Round.NextSlot), orDash(r.NextSlot))
	return nil
}
