package groups

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Group states (groups.md 5.2).
const (
	StateConfirming api.GroupState = "confirming"
	StateRunning    api.GroupState = "running"
	StatePaused     api.GroupState = "paused"
	StateEnding     api.GroupState = "ending"
	StateEnded      api.GroupState = "ended"
	StateDropped    api.GroupState = "dropped"
)

// Item states (groups.md 5.3).
const (
	ItemQueued  api.GroupItemState = "queued"
	ItemStalled api.GroupItemState = "stalled"
	ItemWorking api.GroupItemState = "working"
	ItemReview  api.GroupItemState = "review"
	ItemFix     api.GroupItemState = "fix"
	ItemReady   api.GroupItemState = "ready"
	ItemLanding api.GroupItemState = "landing"
	ItemLanded  api.GroupItemState = "landed"
	ItemDropped api.GroupItemState = "dropped"
)

// Member states.
const (
	MemberPlanned  api.GroupMemberState = "planned"
	MemberStarting api.GroupMemberState = "starting"
	MemberRunning  api.GroupMemberState = "running"
	MemberExited   api.GroupMemberState = "exited"
	MemberFailed   api.GroupMemberState = "failed"
)

// Live reports whether a group still runs or waits: not ended or dropped.
func Live(g api.Group) bool {
	return g.State != StateEnded && g.State != StateDropped
}

// NewID returns a group id: 64 random bits as 16 hex digits.
func NewID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Branch is an item's branch (groups.md 7.1).
func Branch(group, item string) string { return "group/" + group + "/" + item }

// New returns a group in confirming for a normalized, checked plan.
func New(id string, plan api.GroupPlan, project, home string, coord api.GroupCoordinator, now time.Time) api.Group {
	g := api.Group{ID: id, Revision: 1, Title: plan.Title, Project: project, Home: home, Generation: 1,
		Coordinator: coord, Inbox: []api.GroupReport{}, State: StateConfirming, Waits: []string{}, Leftovers: []string{},
		Lane: api.GroupLaneState{Queue: []string{}}, Plan: plan, Created: stamp(now), Updated: stamp(now)}
	g.Items, g.Members = []api.GroupItem{}, []api.GroupMember{}
	AddPlan(&g, plan)
	return g
}

// AddPlan adds a plan's items and members to the record: queued and planned. An amend adds
// its change the same way.
func AddPlan(g *api.Group, p api.GroupPlan) {
	for _, it := range p.Items {
		g.Items = append(g.Items, api.GroupItem{ID: it.ID, Title: it.Title, Branch: Branch(g.ID, it.ID), After: it.After,
			Review: it.Review, State: ItemQueued})
	}
	for _, m := range p.Members {
		g.Members = append(g.Members, api.GroupMember{Name: m.Name, Host: m.Host, Harness: m.Harness, Model: m.Model,
			Role: m.Role, Item: m.Item, State: MemberPlanned})
		if i := ItemIndex(*g, m.Item); i >= 0 {
			switch m.Role {
			case RoleLead:
				g.Items[i].Lead = m.Name
			case RoleReviewer:
				// One reviewer reviews an item (groups.md 6.2): the first the plan names.
				if g.Items[i].Reviewer == "" {
					g.Items[i].Reviewer = m.Name
					g.Items[i].ReviewBranch = ReviewBranch(g.ID, m.Item)
				}
			}
		}
	}
	for i := range g.Items {
		it := &g.Items[i]
		if it.Review == "off" {
			it.Reviewer, it.ReviewBranch, it.SameHarness = "", "", false
			continue
		}
		var lead, rev string
		for _, m := range g.Members {
			switch m.Name {
			case it.Lead:
				lead = m.Harness
			case it.Reviewer:
				rev = m.Harness
			}
		}
		it.SameHarness = it.Reviewer != "" && lead != "" && lead == rev
	}
}

// ItemIndex returns the index of an item, or -1.
func ItemIndex(g api.Group, id string) int {
	return slices.IndexFunc(g.Items, func(it api.GroupItem) bool { return it.ID == id })
}

// MemberIndex returns the index of a member by name, or -1.
func MemberIndex(g api.Group, name string) int {
	return slices.IndexFunc(g.Members, func(m api.GroupMember) bool { return m.Name == name })
}

// holdsSlot reports whether an item's lead holds a lead slot: from its start until it lands or
// drops (groups.md 4.3, 7.3 step 7). A stalled item frees its slot.
func holdsSlot(s api.GroupItemState) bool {
	switch s {
	case ItemWorking, ItemReview, ItemFix, ItemReady, ItemLanding:
		return true
	}
	return false
}

// reachedReview reports whether a stacked item's base reached review (groups.md 7.2).
func reachedReview(s api.GroupItemState) bool {
	switch s {
	case ItemReview, ItemFix, ItemReady, ItemLanding, ItemLanded:
		return true
	}
	return false
}

// NextLeads returns the items whose leads start now, in plan order: queued items whose base
// reached review, up to maxLeads items holding a slot (groups.md 4.3). A running group only.
func NextLeads(g api.Group, maxLeads int) []string {
	if g.State != StateRunning {
		return nil
	}
	held := 0
	for _, it := range g.Items {
		if holdsSlot(it.State) {
			held++
		}
	}
	var out []string
	for _, it := range g.Items {
		if held >= maxLeads {
			break
		}
		if it.State != ItemQueued || it.Lead == "" {
			continue
		}
		if it.After != "" {
			if b := ItemIndex(g, it.After); b < 0 || !reachedReview(g.Items[b].State) {
				continue
			}
		}
		out = append(out, it.ID)
		held++
	}
	return out
}

// AddReport adds a report to the inbox: at most max reports for days, the oldest go first
// (groups.md 5.5).
func AddReport(g *api.Group, r api.GroupReport, max int, days int, now time.Time) {
	g.Inbox = append(g.Inbox, r)
	PruneInbox(g, max, days, now)
}

// PruneInbox drops reports older than days, then the oldest beyond max.
func PruneInbox(g *api.Group, max int, days int, now time.Time) {
	cut := now.Add(-time.Duration(days) * 24 * time.Hour)
	g.Inbox = slices.DeleteFunc(g.Inbox, func(r api.GroupReport) bool {
		t, err := time.Parse(time.RFC3339Nano, r.At)
		return err == nil && t.Before(cut)
	})
	if len(g.Inbox) > max {
		g.Inbox = slices.Delete(g.Inbox, 0, len(g.Inbox)-max)
	}
}

// InboxText is the one message a resumed coordinator gets: every waiting report, oldest first.
func InboxText(g api.Group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Group %s (%s): %d reports came while you were away, oldest first.\n", g.ID, g.Title, len(g.Inbox))
	for _, r := range g.Inbox {
		from := r.From
		if from == "" {
			from = "Flightdeck"
		}
		fmt.Fprintf(&b, "- %s, %s: %s\n", r.At, from, r.Text)
	}
	return b.String()
}

// SetPause sets or lifts one pause reason (groups.md 5.6) and moves the state between running
// and paused. A usage resume never lifts a user pause. It reports whether anything changed.
func SetPause(g *api.Group, reason string, on bool, until string, now time.Time) bool {
	if g.State != StateRunning && g.State != StatePaused {
		return false
	}
	var f *string
	switch reason {
	case "user":
		f = &g.Pause.User
	case "usage":
		f = &g.Pause.Usage
	default:
		return false
	}
	changed := false
	switch {
	case on && *f == "":
		*f, changed = stamp(now), true
	case !on && *f != "":
		*f, changed = "", true
	}
	if reason == "usage" && g.Pause.Until != until {
		g.Pause.Until, changed = until, true
	}
	if g.Pause.Usage == "" {
		g.Pause.Until = ""
	}
	want := StateRunning
	if g.Pause.User != "" || g.Pause.Usage != "" {
		want = StatePaused
	}
	if g.State != want {
		g.State, changed = want, true
	}
	return changed
}

// CapcomAddress is the coordinator address of a group the Capcom service coordinates: the
// channel name that follows the Capcom mark (groups.md 12.1).
const CapcomAddress = "capcom"

// ByCapcom reports whether the Capcom service coordinates a group.
func ByCapcom(g api.Group) bool { return g.Coordinator.Address == CapcomAddress }

// Short is a group's short form for the catalog and group.list (groups.md 5.1).
func Short(g api.Group) api.CatalogGroup {
	c := api.CatalogGroup{ID: g.ID, Title: g.Title, State: string(g.State), Generation: g.Generation, Home: g.Home,
		Items: len(g.Items), Coordinator: &api.CatalogMember{Device: g.Home, Session: g.Coordinator.Session, Name: g.Coordinator.Name, Capcom: ByCapcom(g)}}
	for _, it := range g.Items {
		if it.State == ItemLanded {
			c.Landed++
		}
	}
	for _, m := range g.Members {
		if m.Session != "" {
			c.Members++
		}
	}
	if g.Pause.User != "" || g.Pause.Usage != "" {
		p := g.Pause
		c.Pause = &p
	}
	return c
}

// --- The record files ---

// Store keeps one file per group in its folder, <state>/groups/<id>.json, each written through
// a synced temporary file before its rename.
type Store struct {
	dir string
}

var fileName = regexp.MustCompile(`^[0-9a-f]{16}\.json$`)

// NewStore makes the folder.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Load reads every record. A file that does not parse is set aside as .bad-<unix seconds> and
// named in the problems.
func (s *Store) Load(now time.Time) ([]api.Group, []string) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var out []api.Group
	var problems []string
	for _, e := range ents {
		if !fileName.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(s.dir, e.Name())
		b, err := os.ReadFile(p)
		var g api.Group
		if err == nil {
			err = json.Unmarshal(b, &g)
		}
		if err == nil && g.ID+".json" != e.Name() {
			err = fmt.Errorf("names group %q", g.ID)
		}
		if err != nil {
			bad := fmt.Sprintf("%s.bad-%d", p, now.Unix())
			os.Rename(p, bad)
			problems = append(problems, fmt.Sprintf("%s: %v; set aside as %s", e.Name(), err, filepath.Base(bad)))
			continue
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b api.Group) int { return strings.Compare(a.Created, b.Created) })
	return out, problems
}

// Save writes one record.
func (s *Store) Save(g api.Group) error {
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(s.dir, g.ID+".json"), append(b, '\n'))
}

// Remove deletes a record's file.
func (s *Store) Remove(id string) error {
	err := os.Remove(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func writeSynced(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
