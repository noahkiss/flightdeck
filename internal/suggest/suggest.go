// Package suggest ranks the hosts that could run new work, from the merged catalog (capcom.md
// 3.3). It filters out hosts that are offline or lack a capability or harness the work needs,
// then orders the rest: the project checked out and ready on the right branch first, then the
// host the user named, then the host used last for the project. It says why for each host, and
// when none fits it says why and offers a project set-up. It reads only. Pure Go.
package suggest

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Request is what the work needs.
type Request struct {
	Project string   // a project name, remote, or folder; "" for work outside a project
	Branch  string   // the branch the work wants; "" for any
	Needs   []string // capability words the host must have
	Harness string   // a harness the host must have installed; "" for any
	Host    string   // the host the user named; "" for none
}

// Candidate is one host that fits, with its reasons.
type Candidate struct {
	Host    string   `json:"host" doc:"The host's label as this hab names it; this hab's host id for itself."`
	Self    bool     `json:"self,omitempty"`
	Folder  string   `json:"folder,omitempty" doc:"The project folder to start in. Absent when the project is not on the host."`
	Branch  string   `json:"branch,omitempty"`
	Ready   bool     `json:"ready,omitempty" doc:"The project is checked out on the right branch, with no changes."`
	Reasons []string `json:"reasons"`
}

// Excluded is one host that does not fit, and why.
type Excluded struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// Result is the ranked suggestion.
type Result struct {
	Top        *Candidate  `json:"top,omitempty" doc:"The host to start on. Absent when none fits."`
	Next       []Candidate `json:"next,omitempty" doc:"The other hosts that fit, best first."`
	Excluded   []Excluded  `json:"excluded,omitempty"`
	Why        string      `json:"why,omitempty" doc:"When no host fits: why, naming the missing capability, the project not set up, or every fitting host offline."`
	SetupOn    string      `json:"setup_on,omitempty" doc:"When the project is on no fitting host: the host to set it up on, with set_up_project."`
	Missing    []string    `json:"missing,omitempty" doc:"Capabilities no online host has."`
	NotProject bool        `json:"not_project,omitempty" doc:"The project is on no host that fits."`
}

type scored struct {
	c        Candidate
	tier     int // lower is better: 0 ready, 1 right branch with changes, 2 other branch, 3 not present
	named    bool
	lastUsed time.Time
	order    int
}

// Rank ranks the hosts of cl for r.
func Rank(cl api.CatalogList, r Request) Result {
	var res Result
	var fit []scored
	missingOnline := map[string]bool{}
	offlineFit := []string{}
	for i, h := range cl.Hosts {
		name := label(h)
		c := h.Catalog
		var lacks []string
		for _, n := range r.Needs {
			if !slices.ContainsFunc(c.Capabilities, func(x api.CatalogCapability) bool { return strings.EqualFold(x.Name, n) }) {
				lacks = append(lacks, n)
			}
		}
		if r.Harness != "" && !slices.ContainsFunc(c.Harnesses, func(x api.CatalogTool) bool { return x.Name == r.Harness }) {
			lacks = append(lacks, "harness "+r.Harness)
		}
		if h.Stale {
			reason := "offline, last seen " + h.SeenAt
			if len(lacks) > 0 {
				reason += "; also lacks " + strings.Join(lacks, ", ")
			} else {
				offlineFit = append(offlineFit, name)
			}
			res.Excluded = append(res.Excluded, Excluded{name, reason})
			continue
		}
		if len(lacks) > 0 {
			for _, l := range lacks {
				missingOnline[l] = true
			}
			res.Excluded = append(res.Excluded, Excluded{name, "lacks " + strings.Join(lacks, ", ")})
			continue
		}
		s := scored{c: Candidate{Host: name, Self: h.Own}, tier: 3, order: i,
			named: r.Host != "" && (r.Host == name || r.Host == c.Host)}
		if len(r.Needs) > 0 {
			s.c.Reasons = append(s.c.Reasons, "has "+strings.Join(r.Needs, ", "))
		}
		if r.Harness != "" {
			s.c.Reasons = append(s.c.Reasons, "has "+r.Harness+" installed")
		}
		if r.Project != "" {
			placeProject(&s, cl, c.DeviceID, r)
		}
		for _, a := range c.Agents {
			if r.Project != "" && strings.EqualFold(a.Project, r.Project) {
				if t, err := time.Parse(time.RFC3339Nano, a.Since); err == nil && t.After(s.lastUsed) {
					s.lastUsed = t
				}
			}
		}
		if s.named {
			s.c.Reasons = append(s.c.Reasons, "the host you named")
		}
		fit = append(fit, s)
	}
	if len(fit) > 0 {
		missingOnline = nil // a fitting host has every need
	}
	// The most recent use among the fitting hosts marks one host "used last".
	var last *scored
	for i := range fit {
		if !fit[i].lastUsed.IsZero() && (last == nil || fit[i].lastUsed.After(last.lastUsed)) {
			last = &fit[i]
		}
	}
	if last != nil {
		last.c.Reasons = append(last.c.Reasons, "used last for "+r.Project)
	}
	sort.SliceStable(fit, func(i, j int) bool {
		a, b := fit[i], fit[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.named != b.named {
			return a.named
		}
		if !a.lastUsed.Equal(b.lastUsed) {
			return a.lastUsed.After(b.lastUsed)
		}
		return a.order < b.order
	})
	for m := range missingOnline {
		res.Missing = append(res.Missing, m)
	}
	sort.Strings(res.Missing)
	switch {
	case len(fit) == 0 && len(offlineFit) > 0:
		res.Why = "every host that fits is offline: " + strings.Join(offlineFit, ", ")
		if len(res.Missing) > 0 {
			res.Why += "; no online host has " + strings.Join(res.Missing, ", ")
		}
	case len(fit) == 0 && len(res.Missing) > 0:
		res.Why = "no online host has " + strings.Join(res.Missing, ", ")
	case len(fit) == 0:
		res.Why = "no host is connected"
	case r.Project != "" && fit[0].tier == 3:
		res.NotProject = true
		res.SetupOn = fit[0].c.Host
		res.Why = fmt.Sprintf("project %s is not set up on any host that fits; set it up on %s first", r.Project, fit[0].c.Host)
	}
	if res.Why != "" && !res.NotProject {
		return res
	}
	if res.NotProject {
		for _, s := range fit {
			res.Next = append(res.Next, s.c)
		}
		return res
	}
	top := fit[0].c
	res.Top = &top
	for _, s := range fit[1:] {
		res.Next = append(res.Next, s.c)
	}
	return res
}

func label(h api.CatalogEntry) string {
	if h.Label != "" {
		return h.Label
	}
	return h.Catalog.Host
}

// placeProject finds the project's folders on the host and sets the tier and the folder.
func placeProject(s *scored, cl api.CatalogList, device string, r Request) {
	for _, p := range cl.Projects {
		if !matches(p, r.Project) {
			continue
		}
		for _, l := range p.Locations {
			if l.DeviceID != device {
				continue
			}
			for _, f := range l.Folders {
				tier := 2
				if r.Branch == "" || f.Branch == r.Branch {
					tier = 1
					if f.Dirty != nil && !*f.Dirty {
						tier = 0
					}
				}
				if tier < s.tier || s.c.Folder == "" {
					s.tier, s.c.Folder, s.c.Branch, s.c.Ready = tier, f.Path, f.Branch, tier == 0
				}
				if t, err := time.Parse(time.RFC3339Nano, f.LastCommit); err == nil && t.After(s.lastUsed) {
					s.lastUsed = t
				}
			}
		}
	}
	switch s.tier {
	case 0:
		s.c.Reasons = append(s.c.Reasons, fmt.Sprintf("%s is checked out at %s on %s, with no changes", r.Project, s.c.Folder, branchName(s.c.Branch)))
	case 1:
		s.c.Reasons = append(s.c.Reasons, fmt.Sprintf("%s is checked out at %s on %s, with changes or not read yet", r.Project, s.c.Folder, branchName(s.c.Branch)))
	case 2:
		s.c.Reasons = append(s.c.Reasons, fmt.Sprintf("%s is checked out at %s, but on %s, not %s", r.Project, s.c.Folder, branchName(s.c.Branch), r.Branch))
	default:
		s.c.Reasons = append(s.c.Reasons, fmt.Sprintf("%s is not checked out here", r.Project))
	}
}

func branchName(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return "branch " + b
}

// matches reports whether a project group is the one named: by name, remote, or a folder.
func matches(p api.CatalogProjectGroup, want string) bool {
	if strings.EqualFold(p.Name, want) || (p.Remote != "" && strings.EqualFold(p.Remote, want)) {
		return true
	}
	for _, l := range p.Locations {
		for _, f := range l.Folders {
			if f.Path == want {
				return true
			}
		}
	}
	return false
}
