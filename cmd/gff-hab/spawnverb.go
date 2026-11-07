package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// The spawn verb (harness-adapters.md, Spawn): an agent starts a teammate or sub-agent for its
// own work, in any harness, on any host, with one call. It is a start as start_session's
// (confirm.request kind start, capcom.md 6.4), from the agent's pane, with three additions:
// the harness may be an alias from the owner's table, the folder defaults to the agent's own
// project, and a teammate reports each turn the agent began back to it over channels.
//
// Stage B2 (groups.md 4.1, 12.3) adds the group fields {group, role, item}: a group start asks
// while confirm.group_plan is on, and its session.create carries the trust cover (groups.md
// 4.2, groupcover.go). The argument named role is theirs, so the brief is seed here.

// Spawn modes.
const (
	spawnTeammate = "teammate" // the default: turn reports come back to the spawner
	spawnDetached = "detached" // nothing comes back on its own
)

// spawnResult is the spawn tool's structured answer.
type spawnResult struct {
	Confirmation api.Confirmation `json:"confirmation"`
	Mode         string           `json:"mode"`
	Address      string           `json:"address,omitempty"`
}

// runSpawn starts a teammate for the calling agent and waits up to 45 s for the answer, as
// start_session does.
func runSpawn(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	if p, ok := c.(paneDoor); !ok || !p.fromPane() {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "spawn runs from your own Flightdeck pane: the new session is your teammate, and its reports come to your session")
	}
	mode := a.str("mode")
	switch mode {
	case "":
		mode = spawnTeammate
	case spawnTeammate, spawnDetached:
	default:
		return nil, "", control.Errorf(api.ErrorKindInvalid, "mode %q is not %s or %s", mode, spawnTeammate, spawnDetached)
	}
	p := api.ConfirmRequestParams{Kind: "start", WaitMS: int(startWait / time.Millisecond), Start: &api.ConfirmStart{
		Host: a.str("host"), Folder: a.str("folder"), Harness: a.str("harness"), Model: a.str("model"),
		Name: a.str("name"), Seed: a.str("seed"), Teammate: mode == spawnTeammate,
		Group: a.str("group"), Role: a.str("role"), Item: a.str("item")}}
	var r api.ConfirmRequested
	if err := c.Call(ctx, "confirm.request", p, &r); err != nil {
		return nil, "", err
	}
	res := spawnResult{Confirmation: r.Confirmation, Mode: mode}
	if r.Confirmation.Result != nil {
		res.Address = r.Confirmation.Result.Address
	}
	return res, spawnText(r.Confirmation, mode), nil
}

// spawnText says where a spawn stands, and how its reports reach the spawner.
func spawnText(c api.Confirmation, mode string) string {
	var b strings.Builder
	b.WriteString(confirmationText(c))
	if s := c.Start; s != nil && s.Alias != "" {
		model := s.Model
		if model == "" {
			model = "its default model"
		}
		fmt.Fprintf(&b, "%s is the owner's alias for %s with %s.\n", s.Alias, s.Harness, model)
	}
	addr := ""
	if c.Result != nil {
		addr = c.Result.Address
	}
	switch {
	case c.State != "done" && c.State != "open" && c.State != "running":
		return b.String()
	case addr != "":
		fmt.Fprintf(&b, "Its channel address: %s. channel_send sends it more work.\n", addr)
	}
	if mode == spawnTeammate {
		b.WriteString("It is your teammate: when a turn that your seed or your message began ends, Flightdeck sends you its final message as a channel message from it. channel_wait waits for it.\n")
	} else {
		b.WriteString("It runs detached: nothing comes back on its own. Its seed names your session, so it may reply with gff-hab send.\n")
	}
	return b.String()
}

// spawnFolders fills the folder of a start a pane asked for with none (harness-adapters.md,
// Spawn): on this hab, the requesting session's own working folder; on a peer, a folder of the
// same project there, from the catalog.
type spawnFolders struct {
	folderOf func(ctx context.Context, session string) string
	catalog  func() api.CatalogList
	host     string // this hab's host id
}

func (d spawnFolders) fill(ctx context.Context, ss api.Session, host string) (string, error) {
	own := ""
	if d.folderOf != nil {
		own = d.folderOf(ctx, ss.ID)
	}
	if own == "" {
		return "", control.Errorf(api.ErrorKindInvalid, "this session's folder is not known: give folder")
	}
	if host == "" || host == d.host {
		return own, nil
	}
	if d.catalog == nil {
		return "", control.Errorf(api.ErrorKindInvalid, "no catalog to find this project on %s: give folder", host)
	}
	cl := d.catalog()
	var selfDev, peerDev string
	for _, h := range cl.Hosts {
		switch {
		case h.Own:
			selfDev = h.Catalog.DeviceID
		case h.Label == host:
			peerDev = h.Catalog.DeviceID
		}
	}
	if peerDev == "" {
		return "", control.Errorf(api.ErrorKindNotFound, "no peer labelled %q in the catalog; the hosts tool lists them", host)
	}
	want := realPath(own)
	for _, p := range cl.Projects {
		branch, mine := "", false
		for _, l := range p.Locations {
			if l.DeviceID != selfDev {
				continue
			}
			for _, f := range l.Folders {
				if realPath(f.Path) == want {
					branch, mine = f.Branch, true
				}
			}
		}
		if !mine {
			continue
		}
		var first string
		for _, l := range p.Locations {
			if l.DeviceID != peerDev {
				continue
			}
			for _, f := range l.Folders {
				if f.Branch == branch && branch != "" {
					return f.Path, nil
				}
				if first == "" {
					first = f.Path
				}
			}
		}
		if first != "" {
			return first, nil
		}
		return "", control.Errorf(api.ErrorKindNotFound, "project %s has no folder on %s: give folder, or set the project up there", p.Name, host)
	}
	return "", control.Errorf(api.ErrorKindNotFound, "the catalog lists no project for this session's folder %s: give folder", own)
}

// realPath resolves links, for comparing folders; a path that does not resolve stays clean.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
