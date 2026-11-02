package logbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// Record is one folded record as a reader sees it.
type Record struct {
	ID   string
	Root OpID
	Type string
	// Project is the record's project id, resolved through project joins (5.6). The id the
	// writer set is in Fields["project"].
	Project string
	Title   string
	Body    string
	Status  string
	Deleted bool
	// Fields holds every folded scalar with the op that set it.
	Fields map[string]FieldValue
	Tags   []string
	// Links are this record's links; Here is false for a target this replica does not hold yet.
	Links []RecordLink
	// Backlinks are other records' links to this one; To names the linking record.
	Backlinks []RecordLink
	Notes     []Note
	Flags     []Flag
	// Successors are the records that supersede this finding and are not deleted.
	Successors []string
	Created    HLC
	CreatedBy  *Actor
	// Source is the record's Source as JSON, or a forgotten marker.
	Source   json.RawMessage
	Revision int
}

// RecordLink is a link as Get shows it.
type RecordLink struct {
	Kind string
	To   string
	Pos  int
	Here bool
}

// Superseded reports whether a finding has a successor (5.6).
func (r *Record) Superseded() bool { return len(r.Successors) > 0 }

// Get reads one record by id, deleted or not.
func (s *Store) Get(ctx context.Context, id string) (*Record, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM records WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var f folded
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil, err
	}
	project, err := s.canonicalProject(ctx, f.str("project"))
	if err != nil {
		return nil, err
	}
	r := &Record{ID: f.ID, Root: f.Root, Type: f.Type, Project: project, Title: f.str("title"),
		Body: f.str("body"), Status: f.str("status"), Deleted: f.boolean("deleted"), Fields: f.Fields,
		Notes: f.Notes, Flags: f.Flags, Created: f.Created, CreatedBy: f.CreatedBy, Source: f.Source,
		Revision: f.Revision}
	for _, m := range f.Sets["tags"] {
		r.Tags = append(r.Tags, m.Key)
	}
	for _, m := range f.Sets["links"] {
		var l Link
		if json.Unmarshal(m.Value, &l) != nil {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE id = ?`, l.To).Scan(&n); err != nil {
			return nil, err
		}
		r.Links = append(r.Links, RecordLink{Kind: l.Kind, To: l.To, Pos: l.Pos, Here: n > 0})
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, l.kind, l.pos, r.deleted FROM links l
		JOIN records r ON r.root = l.root WHERE l.target = ? ORDER BY r.id, l.kind`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var successors []FlagValue
	for rows.Next() {
		var bl RecordLink
		var deleted bool
		if err := rows.Scan(&bl.To, &bl.Kind, &bl.Pos, &deleted); err != nil {
			return nil, err
		}
		bl.Here = true
		r.Backlinks = append(r.Backlinks, bl)
		if bl.Kind == LinkSupersedes && !deleted {
			r.Successors = append(r.Successors, bl.To)
			successors = append(successors, FlagValue{Value: jsonString(bl.To)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(r.Successors) > 1 {
		r.Flags = append(r.Flags, Flag{Kind: FlagSupersededTwice, Values: successors})
		slices.SortStableFunc(r.Flags, func(a, b Flag) int { return strings.Compare(a.Kind, b.Kind) })
	}
	return r, nil
}

// canonicalProject resolves a project id through the joins of 5.6: a project that shares an
// identity with an older one shows as the older one.
func (s *Store) canonicalProject(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var c string
	err := s.db.QueryRowContext(ctx, `SELECT canonical FROM aliases WHERE project = ?`, id).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return id, nil
	}
	return c, err
}

// History lists every op on a record, the losers and the ops this build does not fold included,
// in stamp order (10.2 history).
func (s *Store) History(ctx context.Context, id string) ([]*Op, error) {
	var root string
	err := s.db.QueryRowContext(ctx, `SELECT root FROM records WHERE id = ?`, id).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ops, err := scanOps(s.db.QueryContext(ctx, `SELECT raw FROM ops WHERE root = ?`, root))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ops, opCmp)
	return ops, nil
}

// Known reports whether this build folds the op.
func (o *Op) Known() bool { return o.known() }

// Summary is one line of a list.
type Summary struct {
	ID, Type, Status, Title, Project string
	Deleted                          bool
}

// Filter narrows List. Empty fields match everything.
type Filter struct {
	Type    string
	Project string
	Deleted bool // include deleted records
}

// List lists records, oldest first.
func (s *Store) List(ctx context.Context, fl Filter) ([]Summary, error) {
	q := `SELECT id, type, status, title, project, deleted FROM records WHERE 1 = 1`
	var args []any
	if fl.Type != "" {
		q += ` AND type = ?`
		args = append(args, fl.Type)
	}
	if fl.Project != "" {
		q += ` AND coalesce((SELECT canonical FROM aliases WHERE project = records.project), records.project) =
			coalesce((SELECT canonical FROM aliases WHERE project = ?), ?)`
		args = append(args, fl.Project, fl.Project)
	}
	if !fl.Deleted {
		q += ` AND deleted = 0`
	}
	q += ` ORDER BY created_wall, created_counter, created_origin, root`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.ID, &sm.Type, &sm.Status, &sm.Title, &sm.Project, &sm.Deleted); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// Find searches title, body, notes and tags with FTS5 and returns the ids of records that hold
// every word, best first. Deleted records are left out. Words are matched as plain terms, never
// as FTS5 syntax.
func (s *Store) Find(ctx context.Context, words string, limit int) ([]string, error) {
	var terms []string
	for _, w := range strings.Fields(words) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	return queryStrings(ctx, s.db, `SELECT r.id FROM fts JOIN records r ON r.rid = fts.rowid
		WHERE fts MATCH ? AND r.deleted = 0 ORDER BY fts.rank, r.id LIMIT ?`, strings.Join(terms, " "), limit)
}

// Vector is a version vector: per origin, the highest seq held with no gap below it (2.1).
type Vector map[string]uint64

// Vector returns this replica's version vector.
func (s *Store) Vector(ctx context.Context) (Vector, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origin, held FROM origins WHERE held > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := Vector{}
	for rows.Next() {
		var o string
		var n uint64
		if err := rows.Scan(&o, &n); err != nil {
			return nil, err
		}
		v[o] = n
	}
	return v, rows.Err()
}

// OpsAfter returns the stored bytes of the ops this replica holds that a replica with vector
// have lacks, per origin in seq order, up to maxOps ops or maxBytes bytes (at least one op), and
// whether more remain. Ops it does not fold are served like any other (4.4). This is stage 3's
// answer to logbook.pull and the source of logbook.push.
func (s *Store) OpsAfter(ctx context.Context, have Vector, maxOps, maxBytes int) ([][]byte, bool, error) {
	mine, err := s.Vector(ctx)
	if err != nil {
		return nil, false, err
	}
	var out [][]byte
	size := 0
	for _, origin := range sortedKeys(mine) {
		if mine[origin] <= have[origin] {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT raw FROM ops WHERE origin = ? AND seq > ? AND seq <= ? ORDER BY seq`,
			origin, have[origin], mine[origin])
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, false, err
			}
			if len(out) > 0 && ((maxOps > 0 && len(out) >= maxOps) || (maxBytes > 0 && size+len(raw) > maxBytes)) {
				rows.Close()
				return out, true, nil
			}
			out = append(out, raw)
			size += len(raw)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
	}
	return out, false, nil
}

// Changes returns, sorted, the ids of records whose ops arrived after cursor, by any writer of
// the file, and the new cursor. gff-hab polls it to emit events for ops the CLI wrote (4.3). A
// create also names every record that claims its id, since an id collision may have renamed them
// (2.3). Cursor 0 starts from the first op.
func (s *Store) Changes(ctx context.Context, cursor int64) ([]string, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, root, kind, record FROM ops WHERE rowid > ? ORDER BY rowid`, cursor)
	if err != nil {
		return nil, cursor, err
	}
	roots := map[string]bool{}
	claims := map[string]bool{}
	next := cursor
	for rows.Next() {
		var rid int64
		var root, kind, record string
		if err := rows.Scan(&rid, &root, &kind, &record); err != nil {
			rows.Close()
			return nil, cursor, err
		}
		roots[root] = true
		if Kind(kind) == KindCreate {
			claims[record] = true
		}
		next = rid
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, cursor, err
	}
	for _, c := range sortedKeys(claims) {
		siblings, err := queryStrings(ctx, s.db, `SELECT root FROM ops WHERE kind = 'create' AND record = ?`, c)
		if err != nil {
			return nil, cursor, err
		}
		for _, r := range siblings {
			roots[r] = true
		}
	}
	var ids []string
	for _, r := range sortedKeys(roots) {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM records WHERE root = ?`, r).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, cursor, err
		}
		ids = append(ids, id)
	}
	return sortedUnique(ids), next, nil
}

// Status is the replica's size and health.
type Status struct {
	Ops      int
	Records  int
	Unfolded int      // ops this build stores but does not fold
	Waiting  int      // ops held behind a gap or an incomplete batch
	Ahead    []string // origins whose clock ran more than an hour ahead
	Shared   []string // origins two hosts share; their further ops are refused
	Vector   Vector
}

// Status reads the replica's counts and flags.
func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(1 - known), 0) FROM ops`).Scan(&st.Ops, &st.Unfolded); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&st.Records); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM ops o JOIN origins g ON g.origin = o.origin WHERE o.seq > g.applied`).
		Scan(&st.Waiting); err != nil {
		return st, err
	}
	var err error
	if st.Ahead, err = queryStrings(ctx, s.db, `SELECT origin FROM origins WHERE clock_ahead = 1 ORDER BY origin`); err != nil {
		return st, err
	}
	if st.Shared, err = queryStrings(ctx, s.db, `SELECT origin FROM origins WHERE shared = 1 ORDER BY origin`); err != nil {
		return st, err
	}
	st.Vector, err = s.Vector(ctx)
	return st, err
}
