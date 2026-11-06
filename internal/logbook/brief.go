package logbook

import (
	"context"
	"fmt"
)

// BriefQuery names what a session brief covers (11.2): one project, and the time from which a
// finding counts as recent.
type BriefQuery struct {
	Project string // a project id, matched through joins (5.6); empty: only the everywhere records
	SinceMS int64  // findings created at or after this wall time, in ms
	Limit   int    // at most this many records per section; default and cap MaxLimit
}

// BriefItem is one record a brief may show.
type BriefItem struct {
	Summary
	Priority  int // a task or issue: 1 to 3, or 0 for none
	UpdatedMS int64
}

// BriefSet is what a brief draws from, section by section, in the brief's rank order. A record
// shows in the first section that holds it only. More marks a section cut at the limit.
type BriefSet struct {
	Active     []BriefItem // tasks and issues in doing or blocked
	Handed     []BriefItem // tasks and issues handed over: triage new, with a from link (12.2)
	Ready      []BriefItem // ready tasks and issues, by priority
	Guides     []BriefItem // pinned current guides
	Findings   []BriefItem // findings since SinceMS that nothing supersedes
	Plans      []BriefItem // open plans with an open stage
	Everywhere []BriefItem // records with no project, tagged everywhere, still open (3.2)
	More       bool
	Untriaged  int // open tasks and issues with triage new
	Flagged    int // records with a flag
}

// briefSelect reads the summary fields, the priority and the last change of each record r.
const briefSelect = `SELECT r.id, r.type, r.status, r.title, r.project, r.deleted,
	coalesce((SELECT CASE WHEN json_valid(f.value) THEN json_extract(f.value, '$') END FROM fields f
		WHERE f.root = r.root AND f.field = 'priority'), 0) AS priority,
	coalesce((SELECT max(o.wall) FROM ops o WHERE o.root = r.root AND o.known = 1), r.created_wall) AS updated
	FROM records r WHERE r.deleted = 0 AND `

// briefProject matches r's project through joins, with the project id twice as its arguments.
const briefProject = `coalesce((SELECT canonical FROM aliases WHERE project = r.project), r.project) =
	coalesce((SELECT canonical FROM aliases WHERE project = ?), ?)`

// briefField is the JSON value of field name on r, or NULL.
func briefField(name string) string {
	return `(SELECT CASE WHEN json_valid(f.value) THEN json_extract(f.value, '$') END FROM fields f
		WHERE f.root = r.root AND f.field = '` + name + `')`
}

// openTask matches tasks and issues not done or dropped.
const openTask = `r.type IN ('task', 'issue') AND r.status NOT IN ('done', 'dropped')`

// Brief reads what a session brief draws from (11.2). It reads only: a person's and an agent's
// brief differ only in which project they name.
func (s *Store) Brief(ctx context.Context, q BriefQuery) (BriefSet, error) {
	limit := q.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = MaxLimit
	}
	var out BriefSet
	seen := map[string]bool{}
	section := func(dst *[]BriefItem, cond, order string, args ...any) error {
		query := briefSelect + cond + ` ORDER BY ` + order + `, r.id LIMIT ?`
		rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var it BriefItem
			if err := rows.Scan(&it.ID, &it.Type, &it.Status, &it.Title, &it.Project, &it.Deleted, &it.Priority, &it.UpdatedMS); err != nil {
				return err
			}
			if n++; n > limit {
				out.More = true
				break
			}
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			if it.Project, err = s.canonicalProject(ctx, it.Project); err != nil {
				return err
			}
			*dst = append(*dst, it)
		}
		return rows.Err()
	}
	// Priority 1 first, a task with none after priority 3.
	byPriority := `CASE WHEN priority BETWEEN 1 AND 3 THEN priority ELSE 4 END, updated DESC`
	if q.Project != "" {
		p := []any{q.Project, q.Project}
		steps := []struct {
			dst   *[]BriefItem
			cond  string
			order string
		}{
			{&out.Active, `r.type IN ('task', 'issue') AND r.status IN ('doing', 'blocked') AND ` + briefProject,
				`CASE r.status WHEN 'doing' THEN 0 ELSE 1 END, ` + byPriority},
			{&out.Handed, openTask + ` AND ` + briefField("triage") + ` = 'new' AND EXISTS (SELECT 1 FROM links l
				WHERE l.root = r.root AND l.kind = 'from') AND ` + briefProject, byPriority},
			{&out.Ready, `r.type IN ('task', 'issue') AND r.status = 'open' AND NOT EXISTS (SELECT 1 FROM links l
				JOIN records b ON b.root = l.root WHERE l.target = r.id AND l.kind = 'blocks' AND b.deleted = 0
				AND b.status NOT IN ('done', 'dropped')) AND ` + briefProject, byPriority},
			{&out.Guides, `r.type = 'guide' AND r.status IN ('', 'current') AND ` + briefField("pinned") + ` = 1 AND ` + briefProject,
				`updated DESC`},
			{&out.Findings, `r.type = 'finding' AND r.created_wall >= ` + fmt.Sprint(q.SinceMS) + ` AND NOT EXISTS (SELECT 1 FROM links l
				JOIN records b ON b.root = l.root WHERE l.target = r.id AND l.kind = 'supersedes' AND b.deleted = 0) AND ` + briefProject,
				`r.created_wall DESC`},
			{&out.Plans, `r.type = 'plan' AND r.status IN ('', 'open') AND EXISTS (SELECT 1 FROM links l
				JOIN records t ON t.id = l.target WHERE l.root = r.root AND l.kind = 'stage' AND t.deleted = 0
				AND t.status NOT IN ('done', 'dropped')) AND ` + briefProject, `updated DESC`},
		}
		for _, st := range steps {
			if err := section(st.dst, st.cond, st.order, p...); err != nil {
				return out, err
			}
		}
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records r WHERE r.deleted = 0 AND `+openTask+` AND `+
			briefField("triage")+` = 'new' AND `+briefProject, p...).Scan(&out.Untriaged); err != nil {
			return out, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records r WHERE r.deleted = 0 AND
			EXISTS (SELECT 1 FROM flags f WHERE f.root = r.root) AND `+briefProject, p...).Scan(&out.Flagged); err != nil {
			return out, err
		}
	}
	err := section(&out.Everywhere, `r.project = '' AND r.type NOT IN ('project', 'proposal')
		AND r.status NOT IN ('done', 'dropped', 'retired', 'shelved', 'expired', 'rejected')
		AND EXISTS (SELECT 1 FROM members m WHERE m.root = r.root AND m.field = 'tags' AND m.key = ?)
		AND NOT EXISTS (SELECT 1 FROM links l JOIN records b ON b.root = l.root WHERE l.target = r.id
			AND l.kind = 'supersedes' AND b.deleted = 0)`, `updated DESC`, memberKey("tags", jsonString("everywhere")))
	return out, err
}
