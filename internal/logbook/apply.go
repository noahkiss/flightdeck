package logbook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// clockAheadLimit is how far ahead of the receiver's wall clock an op may be stamped before the
// receiver shows "clock ahead" for its origin (5.1). The op is accepted either way.
const clockAheadLimit = time.Hour

// ApplyResult says what Apply did with a set of ops from a peer.
type ApplyResult struct {
	// Stored counts new ops written to the log.
	Stored int
	// Repeats counts ops the replica already held, unchanged.
	Repeats int
	// Refused counts ops not stored: an unusable envelope (2.3's id shape and seqs below 2^63
	// included), an origin that two hosts share, an op of this replica's own origin past a gap,
	// or an op the file's constraints refuse. A refused op is not held: a peer may send it again.
	Refused int
	// Changed lists, sorted, the ids of records whose folded state may have changed: the ids
	// they had before and after, so a record an id collision renames shows under both.
	Changed []string
}

// Apply stores ops a peer sent, as their bytes came, and folds the ones it can (7.1): ops of one
// origin fold in seq order, a whole batch at once, and a gap waits for the missing ops. An op of a
// later version, kind or field is stored and forwarded but not folded (4.4). A repeat changes
// nothing. This is stage 3's entry for logbook.push and a pull's answer.
func (s *Store) Apply(ctx context.Context, raws [][]byte) (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ApplyResult
	tx, err := s.begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	m, err := readMeta(ctx, tx)
	if err != nil {
		return res, err
	}
	own, err := originRow(ctx, tx, m.origin)
	if err != nil {
		return res, err
	}
	ownHeld := own.held
	nowMS := s.now().UnixMilli()
	touched := map[string]bool{}
	affected := map[string]bool{}
	var logs []string
	forgot := false
	for _, raw := range raws {
		o, err := decodeOp(raw)
		if err != nil {
			res.Refused++
			continue
		}
		g, err := originRow(ctx, tx, o.Origin)
		if err != nil {
			return res, err
		}
		if g.shared {
			res.Refused++
			continue
		}
		stored, err := storedRaw(ctx, tx, o.ID())
		if err != nil {
			return res, err
		}
		if stored != nil {
			same, adopt, err := sameOp(stored, o.raw)
			if err != nil {
				return res, err
			}
			if adopt {
				// A peer sent the op already redacted: a forget of it exists. Take the redaction now
				// rather than hold the text until the forget arrives.
				root, err := s.redactStored(ctx, tx, o.ID())
				if err != nil {
					return res, err
				}
				affected[root] = true
				forgot = true
			}
			if same {
				res.Repeats++
				continue
			}
			// Two different ops with one id: two hosts share a device id (7.4). Keep the first and
			// refuse the origin's further ops.
			if _, err := tx.ExecContext(ctx, `UPDATE origins SET shared = 1 WHERE origin = ?`, o.Origin); err != nil {
				return res, err
			}
			logs = append(logs, "two hosts share a device id: "+o.Origin)
			res.Refused++
			continue
		}
		if o.Origin == m.origin {
			// Our own ops come back from a peer only after this file lost them (a restore, 14).
			// One that extends the gap-free run is stored and moves the next seq past it, so no
			// seq is used twice. One past a gap is refused: stored, it would hold a seq a later
			// local write needs, and local writes would stop folding behind the gap. A peer sends
			// it again once the ops before it are here.
			if o.Seq != ownHeld+1 {
				res.Refused++
				continue
			}
		}
		red, ok, err := s.insertOpChecked(ctx, tx, o)
		if err != nil {
			return res, err
		}
		if !ok {
			logs = append(logs, "op "+o.ID().String()+" refused by the file's constraints")
			res.Refused++
			continue
		}
		for r := range red {
			affected[r] = true
			forgot = true
		}
		res.Stored++
		touched[o.Origin] = true
		m.last = observe(m.last, o.HLC())
		if o.Origin == m.origin {
			ownHeld = o.Seq
			m.nextSeq = max(m.nextSeq, o.Seq+1)
		}
		if o.Wall > nowMS+clockAheadLimit.Milliseconds() && !g.clockAhead {
			if _, err := tx.ExecContext(ctx, `UPDATE origins SET clock_ahead = 1 WHERE origin = ?`, o.Origin); err != nil {
				return res, err
			}
			g.clockAhead = true
			logs = append(logs, "clock ahead on "+o.Origin)
		}
	}
	var fresh []*Op
	for _, origin := range sortedKeys(touched) {
		from, to, err := advance(ctx, tx, origin)
		if err != nil {
			return res, err
		}
		ops, err := opsRange(ctx, tx, origin, from, to)
		if err != nil {
			return res, err
		}
		fresh = append(fresh, ops...)
	}
	if err := s.affectedBy(ctx, tx, fresh, affected); err != nil {
		return res, err
	}
	before, err := idsOf(ctx, tx, affected)
	if err != nil {
		return res, err
	}
	skipped, err := s.refold(ctx, tx, affected)
	if err != nil {
		return res, err
	}
	for _, o := range fresh {
		if skipped[o.ID()] {
			logs = append(logs, "op "+o.ID().String()+" on "+o.Record+" not folded: a "+string(o.Kind)+
				" of "+o.Field+" breaks the record type's rules")
		}
	}
	if forgot {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fts(fts) VALUES('optimize')`); err != nil {
			return res, err
		}
	}
	if err := m.write(ctx, tx); err != nil {
		return res, err
	}
	after, err := idsOf(ctx, tx, affected)
	if err != nil {
		return res, err
	}
	res.Changed = sortedUnique(append(before, after...))
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if forgot {
		s.scrub(ctx)
	}
	for _, l := range logs {
		s.logf("logbook: %s", l)
	}
	return res, nil
}

// insertOpChecked runs insertOp inside a savepoint. When the file's constraints refuse the op, the
// savepoint undoes what it wrote and ok is false, so one bad op never fails the rest of a page.
// Any other error is returned.
func (s *Store) insertOpChecked(ctx context.Context, tx txn, o *Op) (map[string]bool, bool, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT op`); err != nil {
		return nil, false, err
	}
	red, err := s.insertOp(ctx, tx, o)
	if err != nil {
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_CONSTRAINT {
			return nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO op`); err != nil {
			return nil, false, err
		}
		_, err = tx.ExecContext(ctx, `RELEASE op`)
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `RELEASE op`)
	return red, true, err
}

func sortedUnique(ids []string) []string {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// sameOp compares a stored op with a received one, after the redaction a forget made on either
// side. adopt is true when only the received copy is redacted.
func sameOp(stored, got []byte) (same, adopt bool, err error) {
	if bytes.Equal(stored, got) {
		return true, false, nil
	}
	rs, err := redact(stored)
	if err != nil {
		return false, false, err
	}
	rg, err := redact(got)
	if err != nil {
		return false, false, err
	}
	if !bytes.Equal(rs, rg) {
		return false, false, nil
	}
	return true, !bytes.Equal(rs, stored) && bytes.Equal(rg, got), nil
}

// redactStored redacts a held op in place and returns its record's root.
func (s *Store) redactStored(ctx context.Context, tx txn, id OpID) (string, error) {
	var raw []byte
	var root string
	if err := tx.QueryRowContext(ctx, `SELECT raw, root FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq).
		Scan(&raw, &root); err != nil {
		return "", err
	}
	r, err := redact(raw)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE ops SET raw = ? WHERE origin = ? AND seq = ?`, r, id.Origin, id.Seq)
	return root, err
}

type originState struct {
	held, applied uint64
	clockAhead    bool
	shared        bool
}

func originRow(ctx context.Context, tx txn, origin string) (originState, error) {
	var g originState
	err := tx.QueryRowContext(ctx, `SELECT held, applied, clock_ahead, shared FROM origins WHERE origin = ?`, origin).
		Scan(&g.held, &g.applied, &g.clockAhead, &g.shared)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO origins(origin) VALUES(?)`, origin)
	}
	return g, err
}

func storedRaw(ctx context.Context, tx txn, id OpID) ([]byte, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT raw FROM ops WHERE origin = ? AND seq = ?`, id.Origin, id.Seq).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// insertOp writes one new op to the log. A forget redacts its target, and an op that a stored
// forget names is redacted before it is written. It returns the roots a redaction touched.
func (s *Store) insertOp(ctx context.Context, tx txn, o *Op) (map[string]bool, error) {
	touched := map[string]bool{}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ops WHERE target = ?`, o.ID().String()).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		r, err := redact(o.raw)
		if err != nil {
			return nil, err
		}
		o.raw = r
	}
	var target any
	if o.Kind == KindForget && o.Target != nil {
		target = o.Target.String()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ops(origin, seq, wall, counter, batch_last, kind, record, root, target, known, raw)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Origin, o.Seq, o.Wall, o.Counter, o.BatchLast, string(o.Kind), o.Record, o.Root.String(), target,
		boolInt(o.known()), o.raw); err != nil {
		return nil, err
	}
	if o.Kind == KindForget && o.Target != nil {
		var raw []byte
		var root string
		err := tx.QueryRowContext(ctx, `SELECT raw, root FROM ops WHERE origin = ? AND seq = ?`,
			o.Target.Origin, o.Target.Seq).Scan(&raw, &root)
		if err == nil {
			r, err := redact(raw)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(r, raw) {
				if _, err := tx.ExecContext(ctx, `UPDATE ops SET raw = ? WHERE origin = ? AND seq = ?`,
					r, o.Target.Origin, o.Target.Seq); err != nil {
					return nil, err
				}
				touched[root] = true
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return touched, nil
}

// advance moves an origin's held and applied seqs over the ops now present: held is the end of
// the run with no gap from 1, applied the end of the last whole batch in it. It returns the
// newly applied range (from, to].
func advance(ctx context.Context, tx txn, origin string) (uint64, uint64, error) {
	g, err := originRow(ctx, tx, origin)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, batch_last FROM ops WHERE origin = ? AND seq > ? ORDER BY seq`, origin, g.held)
	if err != nil {
		return 0, 0, err
	}
	held, applied := g.held, g.applied
	for rows.Next() {
		var seq, last uint64
		if err := rows.Scan(&seq, &last); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if seq != held+1 {
			break
		}
		held = seq
		if seq == last {
			applied = seq
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if held != g.held || applied != g.applied {
		if _, err := tx.ExecContext(ctx, `UPDATE origins SET held = ?, applied = ? WHERE origin = ?`, held, applied, origin); err != nil {
			return 0, 0, err
		}
	}
	return g.applied, applied, nil
}

func opsRange(ctx context.Context, tx txn, origin string, from, to uint64) ([]*Op, error) {
	if to <= from {
		return nil, nil
	}
	return scanOps(tx.QueryContext(ctx, `SELECT raw FROM ops WHERE origin = ? AND seq > ? AND seq <= ? ORDER BY seq`, origin, from, to))
}

func scanOps(rows *sql.Rows, err error) ([]*Op, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		o, err := decodeOp(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// affectedBy adds the roots newly applied ops change: each op's own record, and for a create
// every record that claims the same id, whose collision ids may move.
func (s *Store) affectedBy(ctx context.Context, tx txn, ops []*Op, affected map[string]bool) error {
	for _, o := range ops {
		affected[o.Root.String()] = true
		if o.Kind == KindCreate {
			roots, err := queryStrings(ctx, tx, `SELECT root FROM ops WHERE kind = 'create' AND record = ?`, o.Record)
			if err != nil {
				return err
			}
			for _, r := range roots {
				affected[r] = true
			}
		}
	}
	return nil
}

// appliedKnown selects the ops a fold may use: known and inside their origin's applied run.
const appliedKnown = ` FROM ops o JOIN origins g ON g.origin = o.origin WHERE o.known = 1 AND o.seq <= g.applied`

// refold folds the given records again from their ops and replaces their folded rows. It returns
// the ops a record type's rules kept out.
func (s *Store) refold(ctx context.Context, tx txn, roots map[string]bool) (map[OpID]bool, error) {
	skipped := map[OpID]bool{}
	list := sortedKeys(roots)
	projects := false
	// Delete first: two records of one id may swap ids.
	for _, root := range list {
		typ, err := deleteFolded(ctx, tx, root)
		if err != nil {
			return nil, err
		}
		projects = projects || typ == TypeProject
	}
	for _, root := range list {
		id, err := ParseOpID(root)
		if err != nil {
			return nil, err
		}
		creates, err := scanOps(tx.QueryContext(ctx, `SELECT o.raw`+appliedKnown+` AND o.origin = ? AND o.seq = ? AND o.kind = 'create'`,
			id.Origin, id.Seq))
		if err != nil {
			return nil, err
		}
		if len(creates) == 0 {
			continue
		}
		create := creates[0]
		siblings, err := scanOps(tx.QueryContext(ctx, `SELECT o.raw`+appliedKnown+` AND o.kind = 'create' AND o.record = ?`, create.Record))
		if err != nil {
			return nil, err
		}
		slices.SortFunc(siblings, opCmp)
		collision := slices.IndexFunc(siblings, func(p *Op) bool { return p.ID() == create.ID() })
		ops, err := scanOps(tx.QueryContext(ctx, `SELECT o.raw`+appliedKnown+` AND o.root = ? AND o.kind != 'create'`, root))
		if err != nil {
			return nil, err
		}
		f := fold(create, ops, collision)
		projects = projects || f.Type == TypeProject
		for _, sk := range f.Skipped {
			skipped[sk] = true
		}
		if err := writeFolded(ctx, tx, f); err != nil {
			return nil, err
		}
	}
	if projects {
		if err := joinProjects(ctx, tx); err != nil {
			return nil, err
		}
	}
	return skipped, nil
}

// deleteFolded removes a record's folded rows and returns its type.
func deleteFolded(ctx context.Context, tx txn, root string) (string, error) {
	var rid int64
	var typ string
	err := tx.QueryRowContext(ctx, `SELECT rid, type FROM records WHERE root = ?`, root).Scan(&rid, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fts WHERE rowid = ?`, rid); err != nil {
		return "", err
	}
	for _, t := range rootTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE root = ?`, root); err != nil {
			return "", err
		}
	}
	return typ, nil
}

// joinProjects recomputes the project aliases (5.6): projects that share an identity, directly or
// through others, are one project, and the one with the lowest create stamp keeps its id. Every
// project maps to its canonical id, itself included. It is a pure function of the folded
// project records.
func joinProjects(ctx context.Context, tx txn) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM aliases`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM records WHERE type = ?`, TypeProject)
	if err != nil {
		return err
	}
	var projects []*folded
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var f folded
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, &f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Oldest first, so the first project of a group is its canonical one.
	slices.SortFunc(projects, func(a, b *folded) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Root.String(), b.Root.String())
	})
	parent := make([]int, len(projects))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, p := range projects {
		for _, m := range p.Sets["identities"] {
			j, ok := owner[m.Key]
			if !ok {
				owner[m.Key] = i
				continue
			}
			a, b := find(i), find(j)
			if a > b {
				a, b = b, a
			}
			parent[b] = a // the lower index, the older project, stays the root
		}
	}
	for i, p := range projects {
		if _, err := tx.ExecContext(ctx, `INSERT INTO aliases(project, canonical) VALUES(?, ?)`,
			p.ID, projects[find(i)].ID); err != nil {
			return err
		}
	}
	return nil
}

func writeFolded(ctx context.Context, tx txn, f *folded) error {
	root := f.Root.String()
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO records(root, id, type, project, status, title, deleted,
		created_wall, created_counter, created_origin, revision, data) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		root, f.ID, f.Type, f.str("project"), f.str("status"), f.str("title"), boolInt(f.boolean("deleted")),
		f.Created.Wall, f.Created.Counter, f.Created.Origin, f.Revision, string(data))
	if err != nil {
		return err
	}
	rid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(f.Fields) {
		v := f.Fields[name]
		if _, err := tx.ExecContext(ctx, `INSERT INTO fields(root, field, value, op) VALUES(?, ?, ?, ?)`,
			root, name, string(v.Value), v.Op.String()); err != nil {
			return err
		}
	}
	var tags []string
	for _, name := range sortedKeys(f.Sets) {
		for _, m := range f.Sets[name] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO members(root, field, key, value, op) VALUES(?, ?, ?, ?, ?)`,
				root, name, m.Key, string(m.Value), m.Op.String()); err != nil {
				return err
			}
			switch name {
			case "tags":
				tags = append(tags, m.Key)
			case "links":
				var l Link
				if json.Unmarshal(m.Value, &l) == nil {
					if _, err := tx.ExecContext(ctx, `INSERT INTO links(root, kind, target, pos) VALUES(?, ?, ?, ?)`,
						root, l.Kind, l.To, l.Pos); err != nil {
						return err
					}
				}
			}
		}
	}
	var notes []string
	for _, n := range f.Notes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes(root, op, wall, counter, origin, seq, text) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			root, n.Op.String(), n.At.Wall, n.At.Counter, n.At.Origin, n.Op.Seq, n.Text); err != nil {
			return err
		}
		notes = append(notes, n.Text)
	}
	for _, fl := range f.Flags {
		d, _ := json.Marshal(fl)
		if _, err := tx.ExecContext(ctx, `INSERT INTO flags(root, kind, field, data) VALUES(?, ?, ?, ?)`,
			root, fl.Kind, fl.Field, string(d)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fts(rowid, title, body, notes, tags) VALUES(?, ?, ?, ?, ?)`,
		rid, f.str("title"), f.str("body"), strings.Join(notes, "\n"), strings.Join(tags, " "))
	return err
}

func idsOf(ctx context.Context, tx txn, roots map[string]bool) ([]string, error) {
	var out []string
	for _, r := range sortedKeys(roots) {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM records WHERE root = ?`, r).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}
