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
		if m.Role == RoleLead {
			if i := ItemIndex(*g, m.Item); i >= 0 {
				g.Items[i].Lead = m.Name
			}
		}
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

// Short is a group's short form for the catalog and group.list (groups.md 5.1).
func Short(g api.Group) api.CatalogGroup {
	c := api.CatalogGroup{ID: g.ID, Title: g.Title, State: string(g.State), Generation: g.Generation, Home: g.Home,
		Items: len(g.Items), Coordinator: &api.CatalogMember{Device: CoordinatorDevice(g), Session: g.Coordinator.Session, Name: g.Coordinator.Name},
		Moving: g.Moving, Moved: g.Moved, Tried: g.Tried}
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

// CoordinatorDevice is the device id of the hab the coordinator runs on: the home hab for a
// record from before the movable home (groups.md 5.7).
func CoordinatorDevice(g api.Group) string {
	if g.Coordinator.Device != "" {
		return g.Coordinator.Device
	}
	return g.Home
}

// Where is a group's home as the catalog copies show it (groups.md 5.7).
type Where struct {
	Device     string           // the home hab's device id
	Generation uint64           // the generation that won
	Entry      api.CatalogGroup // the winning short form: the live one, else the tombstone
	Publisher  string           // the device id of the hab whose catalog holds Entry
}

// Find finds a group's home in the catalog copies: the entry with the highest generation wins,
// a tombstone counting as its move's generation and pointing at its to; at a tie the live entry
// wins (groups.md 5.7, how members re-point).
func Find(cl api.CatalogList, id string) (Where, bool) {
	var w Where
	found, live := false, false
	for _, h := range cl.Hosts {
		for _, g := range h.Catalog.Groups {
			if g.ID != id {
				continue
			}
			gen, dev, isLive := g.Generation, h.Catalog.DeviceID, g.Moved == nil
			if !isLive {
				gen, dev = g.Moved.Generation, g.Moved.To
			}
			if !found || gen > w.Generation || (gen == w.Generation && isLive && !live) {
				w, found, live = Where{Device: dev, Generation: gen, Entry: g, Publisher: h.Catalog.DeviceID}, true, isLive
			}
		}
	}
	return w, found
}

// Movable reports why a group cannot move now, or "" (groups.md 5.7, the checks).
func Movable(g api.Group) string {
	switch {
	case g.State != StateRunning && g.State != StatePaused:
		return fmt.Sprintf("group %s is %s; only a running or paused group moves", g.ID, g.State)
	case g.Moving != nil:
		return fmt.Sprintf("group %s is moving to %s already", g.ID, nameOr(g.Moving.Label, g.Moving.To))
	}
	return ""
}

func nameOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Drain puts every item in landing back to ready at the front of the lane queue: a move's
// step 2 for a land before its push (groups.md 5.7). It reports whether anything changed.
func Drain(g *api.Group) bool {
	changed := false
	for i := len(g.Items) - 1; i >= 0; i-- {
		it := &g.Items[i]
		if it.State != ItemLanding {
			continue
		}
		it.State, changed = ItemReady, true
		g.Lane.Queue = append([]string{it.ID}, slices.DeleteFunc(g.Lane.Queue, func(q string) bool { return q == it.ID })...)
		if g.Lane.Landing == it.ID {
			g.Lane.Landing = ""
		}
	}
	return changed
}

// --- The record files ---

// Store keeps one file per group in its folder, <state>/groups/<id>.json, each written through
// a synced temporary file before its rename.
type Store struct {
	dir string
}

var fileName = regexp.MustCompile(`^[0-9a-f]{16}\.json$`)

var idForm = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidID reports whether id is a group id: 16 hex digits.
func ValidID(id string) bool { return idForm.MatchString(id) }

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

// Queued is a move that waits on its target hab for the group's home to be back (groups.md 5.7).
type Queued struct {
	Group string `json:"group"`
	Title string `json:"title,omitempty"`
	Home  string `json:"home"`
	Label string `json:"label,omitempty"`
	At    string `json:"at"`
}

const queuedFile = "queued-moves.json"

// LoadQueued reads the queued moves; a file that does not parse is set aside and none load.
func (s *Store) LoadQueued(now time.Time) ([]Queued, error) {
	p := filepath.Join(s.dir, queuedFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Queued
	if err := json.Unmarshal(b, &out); err != nil {
		os.Rename(p, fmt.Sprintf("%s.bad-%d", p, now.Unix()))
		return nil, fmt.Errorf("%s: %v; set aside", queuedFile, err)
	}
	return out, nil
}

// SaveQueued writes the queued moves.
func (s *Store) SaveQueued(q []Queued) error {
	if q == nil {
		q = []Queued{}
	}
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(s.dir, queuedFile), append(b, '\n'))
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
