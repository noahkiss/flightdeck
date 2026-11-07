package groups

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Review (groups.md 6): the reviewer each item gets at plan time, the lead's submit, the
// reviewer's verdict with its findings and its fixes in place, and the coordinator's one line.

// ReviewerOrder is the order the hab picks a reviewer's harness in (groups.md 6.2). Not a cap:
// the plan names the reviewer, and the user edits it on the card.
var ReviewerOrder = []string{"codex", "claude", "opencode"}

// Bounds of one finding's text. Not caps the user tunes: they bound one message.
const (
	MaxFindingTitle = 200
	MaxFindingPlace = 200
	MaxFindingText  = 2000
)

// ReviewBranch is an item's review branch (groups.md 6.4).
func ReviewBranch(group, item string) string { return Branch(group, item) + "-review" }

// FixInPlace reports whether the plan lets a reviewer fix in place (groups.md 6.4): true unless
// the plan says false.
func FixInPlace(p api.GroupPlan) bool { return p.Lane.FixInPlace == nil || *p.Lane.FixInPlace }

// AddReviewers gives each item with review on that has no reviewer one, and drops the
// reviewers of an item with review off (groups.md 6.1, 6.2). The harness is the first of
// ReviewerOrder that is not the lead's and that installed says is on a plan host: the lead's
// host first, then the other plan hosts in member order. With none, the reviewer runs in the
// lead's harness on the lead's host. The new members have no name: Normalize gives them one.
// It returns a copy.
func AddReviewers(p api.GroupPlan, installed func(host, harness string) bool) api.GroupPlan {
	p.Members = slices.Clone(p.Members)
	off := map[string]bool{}
	for _, it := range p.Items {
		off[it.ID] = it.Review == "off"
	}
	p.Members = slices.DeleteFunc(p.Members, func(m api.GroupPlanMember) bool { return m.Role == RoleReviewer && off[m.Item] })
	var hosts []string
	for _, m := range p.Members {
		if !slices.Contains(hosts, m.Host) {
			hosts = append(hosts, m.Host)
		}
	}
	for _, it := range p.Items {
		if off[it.ID] || slices.ContainsFunc(p.Members, func(m api.GroupPlanMember) bool { return m.Role == RoleReviewer && m.Item == it.ID }) {
			continue
		}
		li := slices.IndexFunc(p.Members, func(m api.GroupPlanMember) bool { return m.Role == RoleLead && m.Item == it.ID })
		if li < 0 {
			continue // Check refuses an item with no lead
		}
		lead := p.Members[li]
		order := append([]string{lead.Host}, slices.DeleteFunc(slices.Clone(hosts), func(h string) bool { return h == lead.Host })...)
		r := api.GroupPlanMember{Role: RoleReviewer, Item: it.ID, Host: lead.Host, Harness: lead.Harness}
	pick:
		for _, h := range ReviewerOrder {
			if h == lead.Harness {
				continue
			}
			for _, host := range order {
				if installed(host, h) {
					r.Host, r.Harness = host, h
					break pick
				}
			}
		}
		p.Members = append(p.Members, r)
	}
	return p
}

// SameHarnessLines are the card's lines for items whose reviewer runs in the lead's harness.
func SameHarnessLines(p api.GroupPlan) []string {
	var out []string
	for _, it := range p.Items {
		var lead, rev string
		for _, m := range p.Members {
			if m.Item != it.ID {
				continue
			}
			switch m.Role {
			case RoleLead:
				lead = m.Harness
			case RoleReviewer:
				rev = m.Harness
			}
		}
		if lead != "" && lead == rev {
			out = append(out, fmt.Sprintf("Item %s: same harness. Its reviewer runs in %s, as its lead does: no other harness is on a plan host.", it.ID, lead))
		}
	}
	return out
}

// CheckVerdict checks a verdict's own shape (groups.md 6.3): pass or fix_needed, at most max
// findings, each high or medium with a title and one paragraph.
func CheckVerdict(p api.ReviewVerdictParams, max int) error {
	if p.Verdict != "pass" && p.Verdict != "fix_needed" {
		return invalid("verdict %q is not pass or fix_needed", p.Verdict)
	}
	if len(p.Findings) > max {
		return invalid("the verdict has %d findings; at most %d ([groups] max_findings). Report the most severe %d", len(p.Findings), max, max)
	}
	for i, f := range p.Findings {
		n := i + 1
		switch {
		case f.Severity == "low":
			return invalid("finding %d is low: report high and medium findings only", n)
		case f.Severity != "high" && f.Severity != "medium":
			return invalid("finding %d: severity %q is not high or medium", n, f.Severity)
		case strings.TrimSpace(f.Title) == "" || utf8.RuneCountInString(f.Title) > MaxFindingTitle || strings.ContainsAny(f.Title, "\n\r"):
			return invalid("finding %d needs a title: one line of at most %d characters", n, MaxFindingTitle)
		case utf8.RuneCountInString(f.Place) > MaxFindingPlace || strings.ContainsAny(f.Place, "\n\r"):
			return invalid("finding %d: the place is one line of at most %d characters", n, MaxFindingPlace)
		case strings.TrimSpace(f.Text) == "" || utf8.RuneCountInString(f.Text) > MaxFindingText:
			return invalid("finding %d needs its paragraph: at most %d characters", n, MaxFindingText)
		}
	}
	if p.Verdict == "fix_needed" && len(p.Findings) == 0 {
		return invalid("fix_needed needs at least one finding for the lead")
	}
	return nil
}

// Submitted is what a submit did.
type Submitted struct {
	State    api.GroupItemState
	Restart  bool // a review of an earlier head ran
	Same     bool // the head is in review already: nothing changed
	Reviewer string
}

// Submit takes a lead's submit of head (groups.md 6.2): the item goes to review, or to ready
// with review off. A submit of another head while a review runs, or after a verdict, starts the
// review again on the new head; the same head in review changes nothing.
func Submit(g *api.Group, item, head string, now time.Time) (Submitted, error) {
	i := ItemIndex(*g, item)
	if i < 0 {
		return Submitted{}, control.Errorf(api.ErrorKindNotFound, "group %s has no item %q", g.ID, item)
	}
	it := &g.Items[i]
	switch it.State {
	case ItemWorking, ItemFix, ItemReview, ItemReady:
	default:
		return Submitted{}, control.Errorf(api.ErrorKindConflict, "item %s is %s; a lead submits while it works, fixes or is in review", it.ID, it.State)
	}
	if head == "" {
		return Submitted{}, invalid("the submit names no commit: commit on branch %s first", it.Branch)
	}
	if it.State == ItemReview && it.Submitted == head {
		return Submitted{State: ItemReview, Same: true, Reviewer: it.Reviewer}, nil
	}
	if it.State == ItemReady && head == it.Land {
		// Ready on this commit: the lead submitted it again, or fast-forwarded to the review
		// branch after a pass with fixes and submitted with no new change.
		return Submitted{State: ItemReady, Same: true, Reviewer: it.Reviewer}, nil
	}
	restart := it.State == ItemReview
	it.Submitted, it.Submits = head, it.Submits+1
	it.Verdict, it.Land, it.Stuck, it.IdleSince, it.Reason = nil, "", "", "", ""
	it.Attempts, it.NoCI = nil, false
	g.Lane.Queue = slices.DeleteFunc(g.Lane.Queue, func(id string) bool { return id == it.ID })
	if it.Review == "off" || it.Reviewer == "" {
		it.State, it.Land = ItemReady, head
		g.Lane.Queue = append(g.Lane.Queue, it.ID)
		return Submitted{State: ItemReady}, nil
	}
	it.State, it.ReviewSince = ItemReview, stamp(now)
	return Submitted{State: ItemReview, Restart: restart, Reviewer: it.Reviewer}, nil
}

// TakeVerdict records a reviewer's verdict on an item in review (groups.md 6.3, 6.4). A pass
// makes the item ready with the commit the lane lands; fix_needed sends it to fix. Fixes count
// only when the plan allows them, the reviewer's hab checked the review branch head against
// the submitted head, and that head descends from it; otherwise they are ignored with the
// reason the card shows. The caller checked the verdict's shape.
func TakeVerdict(g *api.Group, p api.ReviewVerdictParams, now time.Time) (api.ReviewVerdictTaken, error) {
	i := ItemIndex(*g, p.Item)
	if i < 0 {
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindNotFound, "group %s has no item %q", g.ID, p.Item)
	}
	it := &g.Items[i]
	if it.State != ItemReview {
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindConflict, "item %s is %s; a verdict comes while it is in review", it.ID, it.State)
	}
	// The verdict names the head it reviewed (groups.md 6.1): a verdict on an earlier submit
	// never lets a later one land unreviewed.
	switch {
	case p.Checked != it.Submitted:
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindConflict, "the lead submitted %s; review it (git reset --hard %s), then give the verdict on it", Short7(it.Submitted), it.Submitted)
	case p.Reviewed != p.Checked && !p.Descends:
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindConflict, "your review branch head %s does not descend from the submitted head %s: run git reset --hard %s, review it, and commit any fixes on top", Short7(p.Reviewed), Short7(it.Submitted), it.Submitted)
	}
	v := api.GroupVerdict{Verdict: p.Verdict, Findings: slices.Clone(p.Findings), Head: it.Submitted, At: stamp(now)}
	if v.Findings == nil {
		v.Findings = []api.GroupFinding{}
	}
	if f := p.Fixed; f != nil && (len(f.Commits) > 0 || f.Head != "") {
		v.Fixed = slices.Clone(f.Commits)
		switch {
		case !FixInPlace(g.Plan):
			v.FixesIgnored = "the plan does not allow fixes in place; the fixes are ignored"
		case !p.Descends:
			v.FixesIgnored = fmt.Sprintf("the review branch has no commit past the submitted head %s; the fixes are ignored", Short7(it.Submitted))
		default:
			v.FixedHead = p.Reviewed
		}
	}
	it.Verdict, it.IdleSince, it.Stuck = &v, "", ""
	out := api.ReviewVerdictTaken{FixesIgnored: v.FixesIgnored}
	if p.Verdict == "pass" {
		it.State, it.Land = ItemReady, it.Submitted
		if v.FixedHead != "" {
			it.Land = v.FixedHead
		}
		g.Lane.Queue = append(slices.DeleteFunc(g.Lane.Queue, func(id string) bool { return id == it.ID }), it.ID)
		out.Land = it.Land
	} else {
		it.State, it.Land = ItemFix, ""
	}
	out.State = it.State
	return out, nil
}

// Short7 is a commit id's first seven digits, for messages.
func Short7(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// VerdictLine is the one report line the coordinator gets for a verdict (groups.md 6.3): never
// the review text.
func VerdictLine(item string, v api.GroupVerdict) string {
	word := "pass"
	if v.Verdict == "fix_needed" {
		word = "fix needed"
	}
	n := len(v.Findings)
	line := fmt.Sprintf("item %s: %s, %d finding%s", item, word, n, plural(n))
	switch {
	case v.FixedHead != "":
		line += ", with the reviewer's fixes"
	case v.FixesIgnored != "":
		line += ", the reviewer's fixes ignored"
	}
	return line
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// FindingsText is the message a lead gets after fix_needed (groups.md 6.3): every finding.
func FindingsText(g api.Group, it api.GroupItem, v api.GroupVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Group %s, item %s: the reviewer %s needs fixes on %s.\n", g.ID, it.ID, it.Reviewer, Short7(v.Head))
	for i, f := range v.Findings {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, f.Severity, f.Title)
		if f.Place != "" {
			fmt.Fprintf(&b, " (%s)", f.Place)
		}
		fmt.Fprintf(&b, ": %s\n", f.Text)
	}
	if v.FixesIgnored != "" {
		fmt.Fprintf(&b, "The reviewer's fixes were ignored: %s.\n", v.FixesIgnored)
	} else if v.FixedHead != "" {
		fmt.Fprintf(&b, "The reviewer pushed fixes to %s (head %s); build on them with git merge --ff-only %s, or redo them.\n", it.ReviewBranch, Short7(v.FixedHead), v.FixedHead)
	}
	fmt.Fprintf(&b, "Fix them on %s, commit, then call lane_submit again (group %s, item %s); the same reviewer reviews the new head.\n", it.Branch, g.ID, it.ID)
	return b.String()
}

// CardFindings are the findings the card shows: the first n (groups.md 5.4).
func CardFindings(v *api.GroupVerdict, n int) []api.GroupFinding {
	if v == nil {
		return nil
	}
	if len(v.Findings) <= n {
		return v.Findings
	}
	return v.Findings[:n]
}

// Stuck is one review the coordinator hears about: its reviewer exited, or stayed idle.
type Stuck struct {
	Item, Reviewer, Why string
}

// StuckReviews marks the reviews whose reviewer exited or stayed idle for idle with no verdict
// (groups.md 6.3) and returns each newly marked one once. The item stays in review. A reviewer
// that works again clears the idle time; the next submit clears the mark.
func StuckReviews(g *api.Group, idle time.Duration, now time.Time) []Stuck {
	var out []Stuck
	for i := range g.Items {
		it := &g.Items[i]
		if it.State != ItemReview || it.Reviewer == "" {
			continue
		}
		mi := MemberIndex(*g, it.Reviewer)
		if mi < 0 {
			continue
		}
		m := g.Members[mi]
		why := ""
		switch {
		case m.State == MemberExited || m.State == MemberFailed || (m.State == MemberRunning && m.Agent == api.AgentStatusExited):
			it.IdleSince = ""
			why = "exited with no verdict"
		case m.State == MemberRunning && m.Agent == api.AgentStatusIdle:
			if it.IdleSince == "" {
				it.IdleSince = stamp(now)
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, it.IdleSince)
			if err != nil || now.Sub(t) < idle {
				continue
			}
			why = fmt.Sprintf("idle %s with no verdict", idle.Round(time.Second))
		default:
			it.IdleSince = ""
			continue
		}
		if it.Stuck != "" {
			continue
		}
		it.Stuck = why
		out = append(out, Stuck{Item: it.ID, Reviewer: it.Reviewer, Why: why})
	}
	return out
}
