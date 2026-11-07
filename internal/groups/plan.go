// Package groups keeps agent groups' records (groups.md 3, 5): the plan check, the record and
// its file under <state>/groups, the item queue's lead slots, the coordinator's inbox and the
// two pause reasons. gff-hab runs the starts, the confirmation and Logbook around it. Pure Go.
package groups

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Capability is the capability word of a hab that keeps groups.
const Capability = "groups"

// Bounds of a plan's text and size. Not caps the user tunes: they bound one message.
const (
	MaxTitle   = 200
	MaxBrief   = 6000 // a member's seed carries the group's and the item's briefs, under the 8000 of a seed
	MaxItems   = 50
	MaxMembers = 200
	MaxText    = 4000 // a report or a note
)

// The roles (groups.md 2.1).
const (
	RoleLead     api.GroupRole = "lead"
	RoleWorker   api.GroupRole = "worker"
	RoleReviewer api.GroupRole = "reviewer"
)

func invalid(format string, args ...any) error {
	return control.Errorf(api.ErrorKindInvalid, format, args...)
}

// Normalize fills a plan's defaults: review on, automatic names (groups.md 3.1). It returns a
// copy.
func Normalize(p api.GroupPlan, group string) api.GroupPlan {
	p.Items = slices.Clone(p.Items)
	p.Members = slices.Clone(p.Members)
	for i := range p.Items {
		if p.Items[i].Review == "" {
			p.Items[i].Review = "on"
		}
	}
	used := map[string]bool{}
	for _, m := range p.Members {
		if m.Name != "" {
			used[m.Name] = true
		}
	}
	for i := range p.Members {
		m := &p.Members[i]
		if m.Name != "" {
			continue
		}
		// Two members of one role on one item (two workers, or a lead an amend adds where
		// one ran) would get one name: the later ones get -2, -3 and on.
		base := AutoName(m.Harness, group, m.Role, itemPos(p, m.Item))
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		m.Name = name
		used[name] = true
	}
	return p
}

func itemPos(p api.GroupPlan, item string) int {
	return slices.IndexFunc(p.Items, func(it api.GroupPlanItem) bool { return it.ID == item }) + 1
}

// AutoName is a member's automatic name (groups.md 3.1, capcom.md 6.4): the harness, six hex
// digits of the group id, then the role and the item's position, such as codex-3fa9c2-review-2.
func AutoName(harness, group string, role api.GroupRole, pos int) string {
	h := group
	if len(h) > 6 {
		h = h[:6]
	}
	return fmt.Sprintf("%s-%s-%s-%d", harness, h, role, pos)
}

// Check checks a normalized plan's own shape (groups.md 3.1): text bounds, ids unique and valid,
// no after cycle, one lead per item, members on known items and roles, at most maxWorkers
// workers per lead, names in the channel charset and unique. It does not look at hosts,
// harnesses or the catalog. Each refusal names the item or member.
func Check(p api.GroupPlan, maxWorkers int) error {
	switch {
	case strings.TrimSpace(p.Title) == "":
		return invalid("the plan needs a title")
	case utf8.RuneCountInString(p.Title) > MaxTitle || strings.ContainsAny(p.Title, "\n\r"):
		return invalid("the title is one line of at most %d characters", MaxTitle)
	case utf8.RuneCountInString(p.Brief) > MaxBrief:
		return invalid("the brief is %d characters; at most %d", utf8.RuneCountInString(p.Brief), MaxBrief)
	case p.Project == "":
		return invalid("the plan needs a project: its name or remote from the projects tool")
	case len(p.Items) == 0:
		return invalid("the plan needs at least one item")
	case len(p.Items) > MaxItems:
		return invalid("the plan has %d items; at most %d", len(p.Items), MaxItems)
	case len(p.Members) > MaxMembers:
		return invalid("the plan has %d members; at most %d", len(p.Members), MaxMembers)
	}
	ids := map[string]bool{}
	for _, it := range p.Items {
		switch {
		case !confirm.ValidGroupID(it.ID):
			return invalid("item %q: use 1 to 64 lowercase letters, digits and '-'", it.ID)
		case ids[it.ID]:
			return invalid("item %s is in the plan twice", it.ID)
		case strings.TrimSpace(it.Title) == "" || utf8.RuneCountInString(it.Title) > MaxTitle:
			return invalid("item %s needs a title of at most %d characters", it.ID, MaxTitle)
		case utf8.RuneCountInString(it.Brief) > MaxBrief:
			return invalid("item %s: the brief is over %d characters", it.ID, MaxBrief)
		case it.Review != "on" && it.Review != "off":
			return invalid("item %s: review %q is not on or off", it.ID, it.Review)
		}
		ids[it.ID] = true
	}
	for _, it := range p.Items {
		if it.After == "" {
			continue
		}
		if !ids[it.After] {
			return invalid("item %s stacks after %s, which is not in the plan", it.ID, it.After)
		}
		if cyc := afterCycle(p, it.ID); cyc != "" {
			return invalid("items stack in a cycle: %s", cyc)
		}
	}
	leads := map[string]int{}
	workers := map[string]int{}
	reviewers := map[string]int{}
	names := map[string]bool{}
	for i, m := range p.Members {
		who := fmt.Sprintf("member %d (%s)", i+1, m.Name)
		switch {
		case m.Role != RoleLead && m.Role != RoleWorker && m.Role != RoleReviewer:
			return invalid("%s: role %q is not lead, worker or reviewer", who, m.Role)
		case !ids[m.Item]:
			return invalid("%s works on item %q, which is not in the plan", who, m.Item)
		case m.Harness == "":
			return invalid("%s needs a harness", who)
		case strings.HasPrefix(m.Model, "-"):
			return invalid("%s: model %q is not a model name", who, m.Model)
		case !channels.ValidName(m.Name):
			return invalid("%s: name %q: use %s", who, m.Name, channels.NameRule)
		case names[m.Name]:
			return invalid("the name %s is in the plan twice", m.Name)
		}
		names[m.Name] = true
		switch m.Role {
		case RoleLead:
			leads[m.Item]++
		case RoleWorker:
			workers[m.Item]++
		case RoleReviewer:
			reviewers[m.Item]++
		}
	}
	for _, it := range p.Items {
		switch {
		case leads[it.ID] == 0:
			return invalid("item %s has no lead: give it one member with role lead", it.ID)
		case leads[it.ID] > 1:
			return invalid("item %s has %d leads; one lead owns an item", it.ID, leads[it.ID])
		case reviewers[it.ID] > 1:
			return invalid("item %s has %d reviewers; one reviewer reviews an item", it.ID, reviewers[it.ID])
		case workers[it.ID] > maxWorkers:
			return invalid("item %s has %d workers; group.max_workers allows %d per lead", it.ID, workers[it.ID], maxWorkers)
		}
	}
	return nil
}

// afterCycle returns the cycle through id's after chain, or "".
func afterCycle(p api.GroupPlan, id string) string {
	after := map[string]string{}
	for _, it := range p.Items {
		after[it.ID] = it.After
	}
	seen := []string{id}
	for cur := after[id]; cur != ""; cur = after[cur] {
		seen = append(seen, cur)
		if cur == id {
			return strings.Join(seen, " after ")
		}
		if len(seen) > len(p.Items)+1 {
			return ""
		}
	}
	return ""
}

// Warnings are the lines the card shows above its buttons (groups.md 11).
func Warnings(p api.GroupPlan) []string {
	var out []string
	if len(p.Items) == 1 {
		out = append(out, "One item: a single session costs less.")
	}
	if len(p.Items) > 1 {
		same := true
		first := strings.Join(sorted(p.Items[0].Touches), ",")
		for _, it := range p.Items[1:] {
			if first == "" || strings.Join(sorted(it.Touches), ",") != first {
				same = false
			}
		}
		if same {
			out = append(out, "These items overlap and will land one after another.")
		}
	}
	return out
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// TrustRule is the card's line on folder trust (groups.md 4.2): which new worktrees the
// confirmation answers the prompt for.
func TrustRule(group string, hosts, harnesses []string) string {
	covered := slices.DeleteFunc(slices.Clone(harnesses), func(h string) bool { return h == "codex" })
	line := fmt.Sprintf("Confirming answers the folder trust prompt for every worktree a hab makes for this group (branches under %s) on %s", confirm.GroupBranchPrefix(group), strings.Join(hosts, ", "))
	switch {
	case len(covered) == 0:
		return line + ": none of its harnesses; each prompt shows its card."
	case len(covered) < len(harnesses):
		return line + ", for " + strings.Join(covered, ", ") + ". Codex's prompt shows its card: Codex trusts the whole repository."
	}
	return line + ", for " + strings.Join(covered, ", ") + "."
}
