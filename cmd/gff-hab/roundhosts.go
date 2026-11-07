package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

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
	if p.DeviceID == "" {
		// Online with no device id: the link could not pin the peer (gff-hab peers says why).
		// Any hab at that endpoint would pass, so nothing is dialed until the peer is pinned.
		return nil, nil, fmt.Errorf("peer %s: host not pinned: this hab has no device id for it yet; see gff-hab peers, and try again once it is pinned", label)
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
	if w.DeviceID != p.DeviceID {
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

// moveCallTimeout bounds each step of a move after the user answered; moveSettleTimeout bounds
// the rollback and the question to the peer after a failed create, each with its own context
// from context.Background, so they run whatever happened to the move's own. Tests shorten them.
var (
	moveCallTimeout   = 60 * time.Second
	moveSettleTimeout = 15 * time.Second
)

// moveRound moves a fresh, capcom, group or event round from the hab behind from to the peer
// behind to (rounds.md 9),
// after the user confirms: it pauses the round here, so no slot runs on both hosts, creates it
// on the peer with the same definition and pause mark, its project named by remote, then
// deletes it here. A create the peer refused unpauses it here again. A create whose answer was
// lost asks the peer: a round there finishes the move; no round unpauses it here; no answer
// leaves it paused here and says what to check. Resume and fork rounds stay: their target
// session lives on this host, and so does an event round whose filter names a session. An event
// filter's project goes by remote, as the round's does; an event round's pending batch goes in
// the create, read after the pause so no event joins it here after. A completed round stays: its
// counts stay behind, so it would run again on the peer. The run history stays behind, and goes
// with the delete.
func moveRound(ctx context.Context, from, to caller, name, label string, yes, asJSON bool, stdin io.Reader, out io.Writer) error {
	var got api.RoundResult
	if err := from.Call(ctx, "round.get", api.RoundRef{Name: name}, &got); err != nil {
		return err
	}
	r := got.Round
	if r.Mode == api.RoundModeResume || r.Mode == api.RoundModeFork {
		return fmt.Errorf("round %s is a %s round: its target session lives on this host, so it cannot move", name, r.Mode)
	}
	if r.Completed {
		why := ""
		if r.CompletedReason != "" {
			why = " (" + r.CompletedReason + ")"
		}
		return fmt.Errorf("round %s is completed%s: on %s it would run again, as its counts stay here. Edit its trigger, -max-runs or -expires with gff-hab round edit, or delete it with gff-hab round rm %s", name, why, label, name)
	}
	if r.Cwd != "" {
		return fmt.Errorf("round %s starts in %s, a folder on this host: clear -cwd with round edit first", name, r.Cwd)
	}
	if w := r.Trigger.Watch; w != nil && w.Cwd != "" {
		return fmt.Errorf("round %s's watcher starts in %s, a folder on this host: clear -watch-cwd with round edit first", name, w.Cwd)
	}
	spec := r.RoundSpec
	if r.Project != "" {
		if r.ProjectRemote == "" {
			return fmt.Errorf("round %s: its project has no remote, so the other host cannot name it", name)
		}
		spec.Project, spec.ProjectDir = "", ""
	}
	if f := r.Trigger.Filter; r.Trigger.Kind == api.RoundTriggerKindEvent && f != nil {
		if f.Session != "" {
			return fmt.Errorf("round %s: its event filter names session %s, which lives on this host, so it cannot move", name, f.Session)
		}
		ff := *f
		switch {
		case ff.ProjectRemote != "":
			ff.Project = ""
		case ff.Project != "" && r.Trigger.Event != "logbook.updated":
			return fmt.Errorf("round %s: its event filter's project has no remote, so the other host cannot name it", name)
		}
		// A logbook.updated filter with no remote names a Logbook project, the same on every hab.
		spec.Trigger.Filter = &ff
	}
	if !yes && !askYes(stdin, out, fmt.Sprintf("Move round %s to %s? It is created there and deleted here, with its run history. [y/N] ", name, label)) {
		return errors.New("not moved")
	}
	// The answer may have taken any time: the move gets a context of its own.
	ctx, cancel := context.WithTimeout(context.Background(), moveCallTimeout)
	defer cancel()
	if !r.Paused {
		if err := from.Call(ctx, "round.pause", api.RoundRef{ID: r.ID}, nil); err != nil {
			return fmt.Errorf("pause round %s here: %w", name, err)
		}
	}
	create := api.RoundCreateParams{Round: spec}
	if r.Trigger.Kind == api.RoundTriggerKindEvent || r.Trigger.Kind == api.RoundTriggerKindWatch {
		// Read after the pause: a paused round takes no more events here. A watch round's cursor
		// goes with its batch; its dedup keys stay behind (rounds.md 9).
		var b api.RoundResult
		if err := from.Call(ctx, "round.get", api.RoundGetParams{RoundRef: api.RoundRef{ID: r.ID}, Batch: true}, &b); err != nil {
			if !r.Paused {
				from.Call(ctx, "round.unpause", api.RoundRef{ID: r.ID}, nil)
			}
			return fmt.Errorf("read round %s's pending events here: %w", name, err)
		}
		create.Batch = b.Batch
		create.Cursor = b.Cursor
	}
	var made api.RoundCreated
	if err := to.Call(ctx, "round.create", create, &made); err != nil {
		var ce *control.Error
		refused := errors.As(err, &ce)
		sctx, scancel := context.WithTimeout(context.Background(), moveSettleTimeout)
		defer scancel()
		var there api.RoundResult
		gerr := to.Call(sctx, "round.get", api.RoundRef{Name: name}, &there)
		var ge *control.Error
		absent := errors.As(gerr, &ge) && ge.Data.Kind == api.ErrorKindNotFound
		switch {
		case !refused && gerr == nil && sameRound(there.Round, spec):
			// The create took effect; only its answer was lost. Finish the move.
			made = api.RoundCreated{Round: there.Round}
			return finishMove(from, r, made, name, label, asJSON, out)
		case !refused && !absent:
			// Neither the create's answer nor the peer's: the round may run on both hosts once
			// unpaused here, so it stays paused.
			why := "has a different round by that name"
			if gerr != nil {
				why = "did not say whether it has the round: " + gerr.Error()
			}
			return fmt.Errorf("create round %s on %s: %w; the answer was lost, and %s %s. Round %s stays paused here. Check with gff-hab round show %s -host %s: if %s has it, remove it here with gff-hab round rm %s; if not, run it here again with gff-hab round unpause %s",
				name, label, err, label, why, name, name, label, label, name, name)
		case refused && gerr == nil:
			// The peer refused, and has a round by that name: an earlier move whose delete
			// failed, or another round.
			if !r.Paused {
				if uerr := from.Call(sctx, "round.unpause", api.RoundRef{ID: r.ID}, nil); uerr != nil {
					return fmt.Errorf("create round %s on %s: %w; a round %s already exists on %s, and round %s stays paused here: %v", name, label, err, name, label, name, uerr)
				}
			}
			return fmt.Errorf("create round %s on %s: %w; a round %s already exists on %s. If it is this round from an earlier move, remove it here with gff-hab round rm %s; else rename one of them with gff-hab round edit",
				name, label, err, name, label, name)
		}
		if !r.Paused {
			if uerr := from.Call(sctx, "round.unpause", api.RoundRef{ID: r.ID}, nil); uerr != nil {
				return fmt.Errorf("create round %s on %s: %w; and it stays paused here: %v", name, label, err, uerr)
			}
		}
		return fmt.Errorf("create round %s on %s: %w; it stays here", name, label, err)
	}
	return finishMove(from, r, made, name, label, asJSON, out)
}

// finishMove deletes round r here once the peer has it as made.
func finishMove(from caller, r api.Round, made api.RoundCreated, name, label string, asJSON bool, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), moveSettleTimeout)
	defer cancel()
	if err := from.Call(ctx, "round.delete", api.RoundRef{ID: r.ID}, nil); err != nil {
		return fmt.Errorf("round %s now runs on %s, but deleting it here failed: %w; it is paused here, remove it with gff-hab round rm %s", name, label, err, name)
	}
	if asJSON {
		return printJSONTo(out, made)
	}
	fmt.Fprintf(out, "moved %s to %s: next %s (was %s)\n", name, label, orDash(made.Round.NextSlot), orDash(r.NextSlot))
	return nil
}

// sameRound says whether have, a round read from the peer, is the one a lost create sent:
// the same mode, trigger, command, prompt and harness.
func sameRound(have api.Round, sent api.RoundSpec) bool {
	return have.Mode == sent.Mode && have.Trigger.Kind == sent.Trigger.Kind && have.Trigger.Cron == sent.Trigger.Cron &&
		have.Trigger.Zone == sent.Trigger.Zone && have.Trigger.At == sent.Trigger.At && have.Trigger.Event == sent.Trigger.Event &&
		have.Prompt == sent.Prompt && have.Harness == sent.Harness && have.Group == sent.Group && slices.Equal(have.Command, sent.Command)
}
