package lbsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// BriefCap bounds a brief's text, in bytes (11.2).
const BriefCap = 8 << 10

// BriefFindings is how far back a finding counts as recent in a brief (11.2).
const BriefFindings = 7 * 24 * time.Hour

// BriefMore ends a brief that lost lines at the cap (11.2).
const BriefMore = "more: `gff lb ready`\n"

// Brief is the session brief of a project: as a person reads it, every project visible.
func (s *Service) Brief(ctx context.Context, p api.LogbookBriefParams) (api.LogbookBrief, error) {
	return s.brief(ctx, p.Project, nil)
}

// brief builds the brief of project ("" for none) with hide's projects left out (12.1).
func (s *Service) brief(ctx context.Context, project string, hide []string) (api.LogbookBrief, error) {
	out := api.LogbookBrief{}
	if project != "" {
		r, err := s.st.Get(ctx, project)
		if err == nil && r.Type != logbook.TypeProject {
			err = logbook.ErrNotFound
		}
		if err == nil {
			err = s.hidden(ctx, r, hide)
		}
		if err != nil {
			if errors.Is(err, logbook.ErrNotFound) {
				return out, notFound("logbook.brief: no project %s", project)
			}
			return out, apiErr(err)
		}
		if project, err = s.st.Canonical(ctx, r.ID); err != nil {
			return out, err
		}
		if r.ID != project {
			if c, err := s.st.Get(ctx, project); err == nil {
				r = c
			}
		}
		out.Project, out.Name = project, r.Title
	}
	set, err := s.st.Brief(ctx, logbook.BriefQuery{Project: project, SinceMS: s.cfg.Now().Add(-BriefFindings).UnixMilli()})
	if err != nil {
		return out, apiErr(err)
	}
	out.Text, out.Records, out.Dropped = BriefText(out.Name, project, set)
	return out, nil
}

func notFound(format string, args ...any) error {
	return control.Errorf(api.ErrorKindNotFound, format, args...)
}

// briefSection is one heading of the brief and its records, in rank order.
type briefSection struct {
	head  string
	items []logbook.BriefItem
}

// BriefText renders a brief (11.2): a head line with the counts, then one line per record (id,
// status, title) under each section's heading, sections in rank order. Lines go in by rank until
// the text would pass BriefCap; the rest are dropped, and the text ends with BriefMore. It
// returns the text, the record lines kept and the record lines dropped.
func BriefText(name, project string, set logbook.BriefSet) (string, int, int) {
	var b strings.Builder
	switch {
	case project == "":
		b.WriteString("Logbook brief: no project for this folder; records for every project only.\n")
	default:
		fmt.Fprintf(&b, "Logbook brief for %s (%s): %d untriaged, %d flagged.\n", clean(name), project, set.Untriaged, set.Flagged)
	}
	b.WriteString("One line per record: id, status, title. Read one with `gff lb show <id>`.\n")
	sections := []briefSection{
		{"Doing and blocked", set.Active},
		{"Handed over from other projects", set.Handed},
		{"Ready", set.Ready},
		{"Pinned guides", set.Guides},
		{"Findings from the last 7 days", set.Findings},
		{"Open plans", set.Plans},
		{"For every project", set.Everywhere},
	}
	kept, dropped := 0, 0
	full := false
	for _, sec := range sections {
		if len(sec.items) == 0 {
			continue
		}
		head := "\n## " + sec.head + "\n"
		wroteHead := false
		for _, it := range sec.items {
			line := briefLine(it)
			need := len(line)
			if !wroteHead {
				need += len(head)
			}
			// Room for the more line stays, whether or not it is needed.
			if full || b.Len()+need+len(BriefMore) > BriefCap {
				full = true
				dropped++
				continue
			}
			if !wroteHead {
				b.WriteString(head)
				wroteHead = true
			}
			b.WriteString(line)
			kept++
		}
	}
	if kept == 0 {
		b.WriteString("\nNo open records.\n")
	}
	if dropped > 0 || set.More {
		b.WriteString(BriefMore)
	}
	return b.String(), kept, dropped
}

func briefLine(it logbook.BriefItem) string {
	if it.Status == "" {
		return it.ID + "  " + clean(it.Title) + "\n"
	}
	return it.ID + "  " + it.Status + "  " + clean(it.Title) + "\n"
}

// clean keeps a title on one line: control characters become spaces.
func clean(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Join(strings.FieldsFunc(s, isControl), " ")
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029
}

// DefaultProject reports whether a list on the pane route takes the pane's project: no project,
// general or all named (12.1).
func DefaultProject(project string, general, all bool) bool {
	return project == "" && !general && !all
}
