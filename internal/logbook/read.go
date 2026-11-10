package logbook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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
	// Identities are a project's identities (3.1).
	Identities []string
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
	for _, m := range f.Sets["identities"] {
		var v string
		if json.Unmarshal(m.Value, &v) == nil {
			r.Identities = append(r.Identities, v)
		}
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

// FoldedOp is an op with the local fold number this replica gave it when it folded it.
type FoldedOp struct {
	*Op
	Fold int64
}

// FoldedAfter lists a record's ops that this replica folded after the fold number after, in
// fold order (rounds.md 4.3, folded since). The numbers are local and never synced.
func (s *Store) FoldedAfter(ctx context.Context, id string, after int64) ([]FoldedOp, error) {
	var root string
	err := s.db.QueryRowContext(ctx, `SELECT root FROM records WHERE id = ?`, id).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.n, o.raw FROM ops o JOIN folds f ON f.origin = o.origin AND f.seq = o.seq
		WHERE o.root = ? AND f.n > ? ORDER BY f.n`, root, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FoldedOp
	for rows.Next() {
		var n int64
		var raw []byte
		if err := rows.Scan(&n, &raw); err != nil {
			return nil, err
		}
		o, err := decodeOp(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, FoldedOp{Op: o, Fold: n})
	}
	return out, rows.Err()
}

// LastFold is the highest fold number this replica gave, 0 before any.
func (s *Store) LastFold(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(n) FROM folds`).Scan(&n)
	return n.Int64, err
}

// Op reads one op by its id, or ErrNotFound.
func (s *Store) Op(ctx context.Context, id OpID) (*Op, error) {
	ops, err := scanOps(s.db.QueryContext(ctx, `SELECT raw FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq))
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, ErrNotFound
	}
	return ops[0], nil
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
	ops, _, more, err := s.opsAfter(ctx, have, maxOps, maxBytes)
	return ops, more, err
}

// opsAfter is OpsAfter that also returns, per origin in the page, the last seq it holds: a
// page is a gap-free run per origin from have, so the caller holds up to it once it lands.
//
// A redacted op never travels without a forget that names it (5.7). Otherwise a replica that
// lacks the op could store the redacted bytes while the forget is cut off by the page bound, a
// lost answer or a dead disk, and a replica that already holds the op's text is never sent it
// again: the text would stay there for good. So when the caller holds no forget of a redacted op
// by have, the op goes in the page with one forget this replica holds and the ops of that
// forget's origin up to it, so every origin's run in the page stays gap-free (forgetDeps). The
// group goes whole into a page. In an empty page it may pass the limits asked for, never PageOps
// or PageBytes. A group that does not fit stops its origin's run for this page, and the other
// origins go on, the forget's among them, so the group shrinks on later pages. Only when no
// origin can move, or when every forget of the op lies past a gap here, does it travel alone.
func (s *Store) opsAfter(ctx context.Context, have Vector, maxOps, maxBytes int) ([][]byte, Vector, bool, error) {
	mine, err := s.Vector(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	var out [][]byte
	last := Vector{}
	inPage := map[OpID]bool{}
	size := 0
	more := false
	var waiting *sentOp // the first op whose group waits for a later page
	for _, origin := range sortedKeys(mine) {
		if mine[origin] <= have[origin] {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT seq, raw FROM ops WHERE origin = ? AND seq > ? AND seq <= ? ORDER BY seq`,
			origin, have[origin], mine[origin])
		if err != nil {
			return nil, nil, false, err
		}
		for rows.Next() {
			var seq uint64
			var raw []byte
			if err := rows.Scan(&seq, &raw); err != nil {
				rows.Close()
				return nil, nil, false, err
			}
			id := OpID{Origin: origin, Seq: seq}
			if inPage[id] {
				continue // sent already, in a redacted op's group
			}
			group, err := s.forgetDeps(ctx, sentOp{id, raw}, have, last, inPage, mine)
			if err != nil {
				rows.Close()
				return nil, nil, false, err
			}
			need := 0
			for _, g := range group {
				need += len(g.raw)
			}
			over := len(out) > 0 && ((maxOps > 0 && len(out)+len(group) > maxOps) || (maxBytes > 0 && size+need > maxBytes))
			if len(group) == 1 && over {
				rows.Close()
				return out, last, true, nil
			}
			if len(group) > 1 && (over || len(group) > PageOps || need > PageBytes) {
				// The group waits: this origin stops here for this page, the others go on.
				if waiting == nil {
					waiting = &sentOp{id, raw}
				}
				more = true
				break
			}
			for _, g := range group {
				out = append(out, g.raw)
				inPage[g.id] = true
				last[g.id.Origin] = max(last[g.id.Origin], g.id.Seq)
			}
			size += need
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, false, err
		}
	}
	if len(out) == 0 && waiting != nil {
		// No origin could move: the op travels alone, so sync never stops for good.
		return [][]byte{waiting.raw}, Vector{waiting.id.Origin: waiting.id.Seq}, true, nil
	}
	return out, last, more, nil
}

type sentOp struct {
	id  OpID
	raw []byte
}

// forgetDeps returns op first, then what must travel with it so that no redacted op in the group
// reaches a caller with vector have that would hold no forget of it: for each such op, the
// forget this replica holds in its gap-free run with the shortest run to send, and the ops of the
// forget's origin from where the caller and the page (last, inPage) leave off up to it, each
// checked the same way. A redacted op whose forgets all lie past a gap here gets none. A forget
// in an origin the caller skips counts as held: the caller would refuse it. The group stops
// growing once it passes PageOps ops or PageBytes bytes, which no page can carry.
func (s *Store) forgetDeps(ctx context.Context, op sentOp, have, last Vector, inPage map[OpID]bool, mine Vector) ([]sentOp, error) {
	group := []sentOp{op}
	if !bytes.Contains(op.raw, []byte(forgottenPrefix)) {
		return group, nil
	}
	in := map[OpID]bool{op.id: true}
	size := len(op.raw)
	upTo := Vector{} // per origin, the last seq the group holds from a forget's run
	covered := func(o string) uint64 { return max(have[o], last[o], upTo[o]) }
	full := func() bool { return len(group) > PageOps || size > PageBytes }
	for i := 0; i < len(group) && !full(); i++ {
		g := group[i]
		if !bytes.Contains(g.raw, []byte(forgottenPrefix)) {
			continue
		}
		fs, err := s.forgetsOf(ctx, g.id)
		if err != nil {
			return nil, err
		}
		var pick *OpID
		held := false
		for _, f := range fs {
			if f.Seq <= covered(f.Origin) || inPage[f] || in[f] {
				held = true // the caller holds one, or gets one with this page
				break
			}
			if f.Seq <= mine[f.Origin] && (pick == nil || f.Seq-covered(f.Origin) < pick.Seq-covered(pick.Origin)) {
				pick = &f
			}
		}
		if held || pick == nil {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT seq, raw FROM ops WHERE origin = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`,
			pick.Origin, covered(pick.Origin), pick.Seq, PageOps+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() && !full() {
			var seq uint64
			var raw []byte
			if err := rows.Scan(&seq, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			id := OpID{Origin: pick.Origin, Seq: seq}
			if !in[id] && !inPage[id] {
				in[id] = true
				group = append(group, sentOp{id, raw})
				size += len(raw)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		upTo[pick.Origin] = max(upTo[pick.Origin], pick.Seq)
	}
	return group, nil
}

// forgetsOf lists the forgets this replica holds that name id, by origin and seq.
func (s *Store) forgetsOf(ctx context.Context, id OpID) ([]OpID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origin, seq FROM ops WHERE target = ? ORDER BY origin, seq`, id.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpID
	for rows.Next() {
		var f OpID
		if err := rows.Scan(&f.Origin, &f.Seq); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
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
	// A decision on a proposal changes the standing of what its approves made (9.3).
	for _, r := range sortedKeys(roots) {
		var typ string
		err := s.db.QueryRowContext(ctx, `SELECT type FROM records WHERE root = ?`, r).Scan(&typ)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && typ != TypeProposal) {
			continue
		}
		if err != nil {
			return nil, cursor, err
		}
		made, err := decisionRoots(ctx, s.db, r)
		if err != nil {
			return nil, cursor, err
		}
		for _, m := range made {
			roots[m] = true
		}
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
	// Restoring is the highest own seq a peer holds when this file holds less: it was restored
	// from an older copy, and writes are refused until sync brings the ops back. 0 otherwise.
	Restoring uint64
	// Uncopied lists set-aside files of an older origin whose ops are not all copied in yet, as
	// "name: why". gff-hab tries again at each start.
	Uncopied []string
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
	if st.Vector, err = s.Vector(ctx); err != nil {
		return st, err
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'own_seen'`).Scan(&v); err == nil {
		if n, _ := strconv.ParseUint(v, 10, 64); n > st.Vector[s.origin] {
			st.Restoring = n
		}
	}
	st.Uncopied, err = s.uncopied(ctx)
	return st, err
}
