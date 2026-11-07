package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/aliases"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// confirmLink is one peer link as the runner uses it.
type confirmLink struct {
	Label  string
	Online bool
	Caps   []string
	Caller api.Caller // nil while the link is down
}

// confirmRunner checks and runs the starts confirmations carry (capcom.md 3.3, 6.1): on this hab
// through its own control server, on a peer over the link this hab dialed.
type confirmRunner struct {
	host    string // this hab's host id
	catalog func() api.CatalogList
	links   func() []confirmLink
	// local runs session.create on this hab under its idempotency key.
	local func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error)
	// name returns a session's name on this hab.
	name func(id string) string
	// aliases returns the [aliases] table of this hab's config file now (harness-adapters.md,
	// Spawn). Nil: none.
	aliases func() aliases.Table
}

// resolveAlias turns an alias in s.Harness into the harness and model it stands for, once: a
// start that already names its alias is left as it is. An alias may shadow a harness word, so
// the owner can give codex a model. A model the request names wins over the alias's.
func (r *confirmRunner) resolveAlias(s api.ConfirmStart) api.ConfirmStart {
	if s.Alias != "" || r.aliases == nil {
		return s
	}
	if a, word, ok := r.aliases().Lookup(s.Harness); ok {
		s.Alias, s.Harness = word, a.Harness
		if s.Model == "" {
			s.Model = a.Model
		}
	}
	return s
}

// harnessWords lists the harnesses and the aliases, for a refusal.
func (r *confirmRunner) harnessWords() string {
	out := strings.Join(harnesses, ", ")
	if r.aliases != nil {
		if names := r.aliases().Names(); len(names) > 0 {
			out += "; aliases from this hab's [aliases] table: " + strings.Join(names, ", ")
		}
	}
	return out
}

var _ confirm.Runner = (*confirmRunner)(nil)

// harnesses are the harnesses a start may name: the ones session.create starts.
var harnesses = []string{detect.KindClaude, detect.KindCodex, detect.KindOpencode}

func (r *confirmRunner) self(host string) bool {
	return host == "" || host == r.host
}

func (r *confirmRunner) link(label string) (confirmLink, bool) {
	for _, l := range r.links() {
		if l.Label == label {
			return l, true
		}
	}
	return confirmLink{}, false
}

// CheckStart checks a start and fills in its host label, project and branch.
func (r *confirmRunner) CheckStart(s api.ConfirmStart) (api.ConfirmStart, bool, error) {
	s = r.resolveAlias(s)
	switch {
	case !slices.Contains(harnesses, s.Harness):
		return s, false, control.Errorf(api.ErrorKindInvalid, "harness %q is not one of %s", s.Harness, r.harnessWords())
	case !filepath.IsAbs(s.Folder):
		return s, false, control.Errorf(api.ErrorKindInvalid, "folder %q is not an absolute path on the target host", s.Folder)
	case strings.HasPrefix(s.Model, "-"):
		return s, false, control.Errorf(api.ErrorKindInvalid, "model %q is not a model name", s.Model)
	case s.Name != "" && !channels.ValidName(s.Name):
		return s, false, control.Errorf(api.ErrorKindInvalid, "session name %q: use %s", s.Name, channels.NameRule)
	}
	if err := confirm.CheckSeed(s.Seed); err != nil {
		return s, false, err
	}
	self := r.self(s.Host)
	if self {
		s.Host = r.host
	} else {
		l, ok := r.link(s.Host)
		switch {
		case !ok:
			return s, false, control.Errorf(api.ErrorKindNotFound, "no peer labelled %q; the hosts tool lists them", s.Host)
		case !l.Online || l.Caller == nil:
			return s, false, control.Errorf(api.ErrorKindUnavailable, "%s is offline, or only its own link to this hab is up; start elsewhere, or once it is back", s.Host)
		case !slices.Contains(l.Caps, confirm.Capability):
			return s, false, control.Errorf(api.ErrorKindUnavailable, "%s runs a gff-hab without the confirm capability; update it first", s.Host)
		case s.Teammate && !slices.Contains(l.Caps, teammatesCapability):
			return s, false, control.Errorf(api.ErrorKindUnavailable, "%s runs a gff-hab that cannot report a teammate's turns; update it, or spawn with mode detached", s.Host)
		}
	}
	s.Project, s.Branch = r.folderFacts(s.Host, self, s.Folder)
	return s, self, nil
}

// folderFacts returns the project name and branch the catalog lists for a folder on a host.
func (r *confirmRunner) folderFacts(host string, self bool, folder string) (project, branch string) {
	if r.catalog == nil {
		return "", ""
	}
	cl := r.catalog()
	device := ""
	for _, h := range cl.Hosts {
		if (self && h.Own) || (!self && h.Label == host) {
			device = h.Catalog.DeviceID
		}
	}
	for _, p := range cl.Projects {
		for _, l := range p.Locations {
			if l.DeviceID != device {
				continue
			}
			for _, f := range l.Folders {
				if filepath.Clean(f.Path) == filepath.Clean(folder) {
					return p.Name, f.Branch
				}
			}
		}
	}
	return "", ""
}

// RunStart runs session.create on the target with the confirmation id as its key.
func (r *confirmRunner) RunStart(ctx context.Context, id, from string, s api.ConfirmStart) (api.ConfirmResult, error) {
	p := api.SessionCreateParams{Name: s.Name, ProjectDir: s.Folder, Harness: s.Harness, Model: s.Model,
		Seed: s.Seed, IdempotencyKey: id}
	if sf := r.host + "/" + from; s.Seed != "" && from != "" && validSeedFrom(sf) {
		// A host id the envelope cannot carry leaves the writer out ("an agent") rather than
		// failing the start after the owner confirmed it.
		p.SeedFrom = sf
	}
	if s.Teammate && from != "" {
		// The target files the session as reporting to from, a session on the hab at the other
		// end of the call: this one, or over a peer link this hab, by the link's own hello.
		p.ReportTo = from
	}
	if s.Group != "" {
		// The target hab answers the member's folder trust prompt for the group's worktrees
		// (groups.md 4.2, 12.6): the user answered this confirmation, or confirm.group_plan is
		// off and nobody was asked.
		p.TrustCover = groupCover(s, id, confirm.Confirmed(ctx))
	}
	if r.self(s.Host) {
		made, err := r.local(withOwnCover(ctx), p)
		if err != nil {
			return api.ConfirmResult{}, err // the hab's own refusal: a failure
		}
		name := s.Name
		if r.name != nil {
			if n := r.name(made.Session); n != "" {
				name = n
			}
		}
		return api.ConfirmResult{Host: r.host, Session: made.Session, Name: name, Pane: made.Pane, Address: name}, nil
	}
	l, ok := r.link(s.Host)
	if !ok || !l.Online || l.Caller == nil {
		// Nothing was sent: a definite failure.
		return api.ConfirmResult{}, control.Errorf(api.ErrorKindUnavailable, "%s went offline before the start was sent; nothing ran", s.Host)
	}
	var made api.SessionCreated
	if err := l.Caller.Call(ctx, "session.create", p, &made); err != nil {
		var ce *control.Error
		if errors.As(err, &ce) {
			return api.ConfirmResult{}, control.Errorf(ce.Data.Kind, "%s refused the start: %s", s.Host, strings.TrimPrefix(ce.Message, "gff-hab: "))
		}
		return api.ConfirmResult{}, fmt.Errorf("%w: the answer from %s was lost (%v); a session named %s may run there", confirm.ErrUnknown, s.Host, err, s.Name)
	}
	return api.ConfirmResult{Host: s.Host, Session: made.Session, Name: s.Name, Pane: made.Pane, Address: s.Host + "/" + s.Name}, nil
}

// confirmService is what the pane route and the snapshot call; *confirm.Service is the one
// gff-hab runs.
type confirmService interface {
	Request(ctx context.Context, p api.ConfirmRequestParams) (api.Confirmation, error)
	List(all bool) []api.Confirmation
	Get(id string) (api.Confirmation, error)
	Targets() []api.ConfirmTarget
}

// confirmHook answers the confirmation methods from a pane (capcom.md 6.1): the pane's session
// is the requester, whatever the params say. confirm.answer is not on the route.
// folder, when not nil, fills a start's empty folder from the requesting session (spawn.go).
func confirmHook(store *structure.Store, svc confirmService, folder func(context.Context, api.Session, string) (string, error)) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		switch method {
		case "confirm.request":
			ss, ok := store.SessionByPane(pane)
			if !ok {
				return nil, nil, control.Errorf(api.ErrorKindInvalid, "this pane is not a session; a start is asked for by a session")
			}
			var p api.ConfirmRequestParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			p.From = ss.Name
			if p.Kind == "start" && p.Start != nil {
				if p.Start.Folder == "" && folder != nil {
					f, err := folder(ctx, ss, p.Start.Host)
					if err != nil {
						return nil, nil, err
					}
					p.Start.Folder = f
				}
			}
			c, err := svc.Request(ctx, p)
			return api.ConfirmRequested{Confirmation: c}, nil, err
		case "confirm.get":
			var p api.ConfirmGetParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			c, err := svc.Get(p.ID)
			return api.ConfirmationResult{Confirmation: c}, nil, err
		default: // confirm.list
			var p api.ConfirmListParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			return api.ConfirmList{Confirmations: svc.List(p.All), Targets: svc.Targets()}, nil, nil
		}
	}
}

// confirmTargets lists the hosts a start can go to now (confirm.list): this hab, and each peer
// with its reason when a start there would be refused.
func confirmTargets(host string, links func() []confirmLink) func() []api.ConfirmTarget {
	return func() []api.ConfirmTarget {
		out := []api.ConfirmTarget{{Host: host, Ready: true}}
		for _, l := range links() {
			t := api.ConfirmTarget{Host: l.Label}
			switch {
			case !l.Online || l.Caller == nil:
				t.Reason = "offline, or only its own link to this hab is up"
			case !slices.Contains(l.Caps, confirm.Capability):
				t.Reason = "its gff-hab lacks the confirm capability; update it"
			default:
				t.Ready = true
			}
			out = append(out, t)
		}
		return out
	}
}

// confirmLinks lists the peer links for the runner.
func confirmLinks(pm *peers.Manager) func() []confirmLink {
	return func() []confirmLink {
		var out []confirmLink
		for _, l := range pm.Links() {
			cl := confirmLink{Label: l.Label, Online: l.Online, Caps: l.Caps}
			if l.Conn != nil {
				cl.Caller = l.Conn
			}
			out = append(out, cl)
		}
		return out
	}
}
