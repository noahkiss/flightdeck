package logbook

import (
	"context"
	"strings"
)

// Query narrows Search (stage 4: logbook.find, logbook.list and logbook.ready). Empty fields
// match everything.
type Query struct {
	// Words, when set, searches title, body, notes and tags as Find does; the best match comes
	// first. Without words the newest change comes first.
	Words   string
	Type    string
	Status  string
	Project string // a project id, matched through joins (5.6) as List does
	Tag     string
	// Host matches the host that created the record: the actor's host, or the create's origin.
	Host string
	// Author matches who created the record: the actor's session, device or harness.
	Author string
	// Session matches the session a record came from: the creating actor's, or its source's.
	Session string
	// SinceMS keeps records whose last folded op is at or after this wall time, in ms.
	SinceMS int64
	General   bool // only records with no project (3.2)
	Flagged   bool // only records with a flag: a conflict and the like (10.2 conflicts)
	Ready     bool // open tasks and issues with no open blocker (10.2 ready)
	Deleted   bool // include deleted records
	Limit     int  // default 50
}

// Item is one line of a Search answer.
type Item struct {
	Summary
	Revision int
	// UpdatedMS is the wall time of the record's last folded op.
	UpdatedMS int64
}

// MaxLimit bounds one Search answer.
const MaxLimit = 500

// Search lists the records that match q, at most q.Limit of them, and whether more match.
func (s *Store) Search(ctx context.Context, q Query) ([]Item, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, MaxLimit)
	sel := `SELECT r.id, r.type, r.status, r.title, r.project, r.deleted, r.revision,
		coalesce((SELECT max(o.wall) FROM ops o WHERE o.root = r.root AND o.known = 1), r.created_wall) AS updated
		FROM records r`
	var where []string
	var args []any
	order := ` ORDER BY updated DESC, r.id`
	if terms := ftsTerms(q.Words); terms != "" {
		sel += ` JOIN fts ON fts.rowid = r.rid`
		where = append(where, `fts MATCH ?`)
		args = append(args, terms)
		order = ` ORDER BY fts.rank, r.id`
	}
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if q.Type != "" {
		add(`r.type = ?`, q.Type)
	}
	if q.Status != "" {
		add(`r.status = ?`, q.Status)
	}
	if q.Project != "" {
		add(`coalesce((SELECT canonical FROM aliases WHERE project = r.project), r.project) =
			coalesce((SELECT canonical FROM aliases WHERE project = ?), ?)`, q.Project, q.Project)
	}
	if q.General {
		add(`r.project = ''`)
	}
	if q.Tag != "" {
		add(`EXISTS (SELECT 1 FROM members m WHERE m.root = r.root AND m.field = 'tags' AND m.key = ?)`, memberKey("tags", jsonString(q.Tag)))
	}
	if q.Host != "" {
		add(`(json_extract(r.data, '$.created_by.host') = ? OR r.created_origin = ?)`, q.Host, q.Host)
	}
	if q.Author != "" {
		add(`(json_extract(r.data, '$.created_by.session') = ? OR json_extract(r.data, '$.created_by.device') = ?
			OR json_extract(r.data, '$.created_by.harness') = ?)`, q.Author, q.Author, q.Author)
	}
	if q.Session != "" {
		add(`(json_extract(r.data, '$.created_by.session') = ? OR json_extract(r.data, '$.source.session') = ?)`, q.Session, q.Session)
	}
	if q.Flagged {
		add(`EXISTS (SELECT 1 FROM flags f WHERE f.root = r.root)`)
	}
	if q.Ready {
		add(`r.type IN ('task', 'issue') AND r.status = 'open' AND NOT EXISTS (SELECT 1 FROM links l
			JOIN records b ON b.root = l.root WHERE l.target = r.id AND l.kind = 'blocks' AND b.deleted = 0
			AND b.status NOT IN ('done', 'dropped'))`)
	}
	if !q.Deleted {
		add(`r.deleted = 0`)
	}
	if q.SinceMS > 0 {
		add(`coalesce((SELECT max(o.wall) FROM ops o WHERE o.root = r.root AND o.known = 1), r.created_wall) >= ?`, q.SinceMS)
	}
	query := sel
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += order + ` LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Type, &it.Status, &it.Title, &it.Project, &it.Deleted, &it.Revision, &it.UpdatedMS); err != nil {
			return nil, false, err
		}
		if it.Project, err = s.canonicalProject(ctx, it.Project); err != nil {
			return nil, false, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// ftsTerms quotes each word as a plain FTS5 term, never as syntax.
func ftsTerms(words string) string {
	var terms []string
	for _, w := range strings.Fields(words) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " ")
}

// Updated is the wall time, in ms, of the record's last folded op.
func (s *Store) Updated(ctx context.Context, r *Record) (int64, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(wall), ?) FROM ops WHERE root = ? AND known = 1`,
		r.Created.Wall, r.Root.String()).Scan(&ms)
	return ms, err
}
