package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/worktree"
)

// The trust cover of a group member's start (groups.md 4.2, 12.6): the user confirmed the
// group's start, or turned confirm.group_plan off, and that is the trust answer for every
// worktree a hab makes for the group. The member's own hab answers its pane's folder trust
// prompt, and only for a folder the cover matches: a worktree this hab made (it carries
// gff-hab's mark), on a branch under group/<group>/, for the cover's harness. Anything else,
// and any failure, shows the trust card as before (app.md 7.10).

// confirmationNone is the cover's confirmation when the plan started with none.
const confirmationNone = "none"

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// groupCover is the cover a group start sends: the confirmation id when the user answered it,
// else none.
func groupCover(s api.ConfirmStart, id string, confirmed bool) *api.TrustCover {
	c := &api.TrustCover{Group: s.Group, Confirmation: confirmationNone, Harness: s.Harness,
		BranchPrefix: confirm.GroupBranchPrefix(s.Group)}
	if confirmed {
		c.Confirmation = id
	}
	return c
}

type ownCoverKey struct{}

// withOwnCover marks a session.create this hab's own confirmations run: it may carry a cover.
func withOwnCover(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownCoverKey{}, true)
}

func ownCover(ctx context.Context) bool {
	v, _ := ctx.Value(ownCoverKey{}).(bool)
	return v
}

// checkCoverParams checks a session.create's trust cover: it comes from this hab's own
// confirmations, or over the link of a peer this hab's peer list names; its fields are well
// formed and its harness is the one the create starts.
func checkCoverParams(ctx context.Context, p api.SessionCreateParams, listed func(device string) bool) error {
	c := p.TrustCover
	if c == nil {
		return nil
	}
	if !ownCover(ctx) {
		peer, ok := control.CallerPeer(ctx)
		switch {
		case !ok:
			return control.Errorf(api.ErrorKindInvalid, "trust_cover comes only from a hab: this hab's own confirmations, or a peer hab over its link")
		case listed == nil || !listed(peer.DeviceID):
			return control.Errorf(api.ErrorKindInvalid, "trust_cover from %s: this hab's peer list does not name it", peer.Host)
		}
	}
	switch {
	case p.Harness == "":
		return control.Errorf(api.ErrorKindInvalid, "trust_cover needs harness")
	case c.Harness != p.Harness:
		return control.Errorf(api.ErrorKindInvalid, "trust_cover names harness %q, the create starts %q", c.Harness, p.Harness)
	case !confirm.ValidGroupID(c.Group):
		return control.Errorf(api.ErrorKindInvalid, "trust_cover group %q: use 1 to 64 lowercase letters, digits and '-'", c.Group)
	case c.BranchPrefix != confirm.GroupBranchPrefix(c.Group):
		return control.Errorf(api.ErrorKindInvalid, "trust_cover branch_prefix %q is not %q", c.BranchPrefix, confirm.GroupBranchPrefix(c.Group))
	case c.Confirmation != confirmationNone && !hex32.MatchString(c.Confirmation):
		return control.Errorf(api.ErrorKindInvalid, "trust_cover confirmation %q is not a confirmation id or none", c.Confirmation)
	}
	return nil
}

// paneCover is a cover as the trust watch keeps it for one pane.
type paneCover struct {
	cover  api.TrustCover
	folder string // the folder the session started in
}

// uncoveredHarness lists the harnesses whose trust answer reaches past the worktree, with why:
// the cover never answers for them, and their prompt shows the card. Codex 0.167 records trust
// for a linked worktree on the repository's main checkout ("Trusting will apply to the
// repository root"), which would trust every worktree of the repository, someone else's code
// too (checked live 2026-11-06).
var uncoveredHarness = map[string]string{
	"codex": "Codex trusts the whole repository, not the worktree, so the owner answers it",
}

// coverCheck checks, when the dialog shows, that a pane's folder is one its cover answers: a
// linked worktree this hab made, holding the folder, whose branch starts with the cover's
// prefix, for the cover's harness. It returns why not, or "".
func coverCheck(g worktree.Git) func(ctx context.Context, harness string, pc paneCover) string {
	return func(ctx context.Context, harness string, pc paneCover) string {
		if harness != pc.cover.Harness {
			return fmt.Sprintf("the pane runs %s, the cover is for %s", harness, pc.cover.Harness)
		}
		if why, ok := uncoveredHarness[harness]; ok {
			return why
		}
		folder := realPath(pc.folder)
		top, err := g.Top(ctx, folder)
		if err != nil {
			return fmt.Sprintf("%s is not in a git worktree", pc.folder)
		}
		top = realPath(top)
		wts, err := g.List(ctx, top)
		if err != nil {
			return fmt.Sprintf("listing the worktrees of %s: %v", top, err)
		}
		i := slices.IndexFunc(wts, func(w worktree.Worktree) bool { return realPath(w.Path) == top })
		switch {
		case i < 0:
			return fmt.Sprintf("git does not list %s as a worktree", top)
		case wts[i].Main:
			return fmt.Sprintf("%s is the project's main checkout, not a group worktree", top)
		case !strings.HasPrefix(wts[i].Branch, pc.cover.BranchPrefix) || len(wts[i].Branch) == len(pc.cover.BranchPrefix):
			return fmt.Sprintf("%s is on branch %q, not under %s", top, wts[i].Branch, pc.cover.BranchPrefix)
		}
		m, ok, err := g.ReadMark(ctx, top)
		switch {
		case err != nil || !ok:
			return fmt.Sprintf("%s is not a worktree this hab made", top)
		case m.Foreign != "" && !m.Approved:
			// Code that may come from someone else, whose setup still waits for the owner's yes
			// (hab-logic-api.md 3.10): the owner has not seen it, so the cover does not trust it.
			return fmt.Sprintf("%s holds code that may come from someone else (%s), not yet approved", top, m.Foreign)
		}
		return ""
	}
}

// Defaults and bounds of [groups] trust_hold (groups.md 15.3).
const (
	defaultTrustHold = 10 * time.Second
	minTrustHold     = time.Second
	maxTrustHold     = time.Minute
)

// loadTrustHold reads [groups] trust_hold from the hab's config file: a duration such as "10s"
// from 1 s to 60 s. A value outside its range, or of the wrong type, is logged and the default
// runs; so does a file that does not parse. Other [groups] keys belong to later stages and are
// left alone.
func loadTrustHold(path string, logf func(string, ...any)) time.Duration {
	if path == "" {
		return defaultTrustHold
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultTrustHold
	}
	if err != nil {
		logf("groups: %v; [groups] trust_hold %s runs", err, defaultTrustHold)
		return defaultTrustHold
	}
	var fc struct {
		Groups map[string]any `toml:"groups"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		logf("groups: %s does not parse (%v); [groups] trust_hold %s runs", path, err, defaultTrustHold)
		return defaultTrustHold
	}
	v, ok := fc.Groups["trust_hold"]
	if !ok {
		return defaultTrustHold
	}
	s, isStr := v.(string)
	d, err := time.ParseDuration(s)
	if !isStr || err != nil || d < minTrustHold || d > maxTrustHold {
		logf("groups: %s: [groups] trust_hold %v is not a duration from \"1s\" to \"60s\"; %s runs", filepath.Base(path), v, defaultTrustHold)
		return defaultTrustHold
	}
	return d
}
