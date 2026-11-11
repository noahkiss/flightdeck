package logbook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite" // pure-Go SQLite with FTS5
	sqlite3 "modernc.org/sqlite/lib"
)

// foldVersion names the folded tables' shape and this build's fold rules. A file folded by
// another version is rebuilt from its ops at open (4.4): no migrations. 5: the actor gained
// session_name, which a build before it dropped from the folded tables. 6: the fields created
// and imported (13.1), which a build before it does not fold.
const foldVersion = 6

// ErrNotFound means the record or op is not on this replica.
var ErrNotFound = errors.New("logbook: not found")

// ErrInvalid is the class of a refused write; the error is an *InvalidError with a hint.
var ErrInvalid = errors.New("logbook: invalid")

// ErrSharedOrigin means another host sent ops under this replica's own origin (7.4): two hosts
// share a device id. The replica refuses to write until the host gets a new device id.
var ErrSharedOrigin = errors.New("logbook: this host's device id is shared with another host; give it a new device id")

// ErrRestoring means a peer holds ops of this replica's own origin that its file lacks: the file
// was restored from an older copy. A write now would reuse a seq the peers hold, so writes are
// refused until sync brings those ops back (7.6). The error names the seqs.
var ErrRestoring = errors.New("logbook: this file was restored from an older copy: a peer holds newer ops of this host; writes are refused until sync brings them back")

// ErrNoOrigin means the file has no origin yet and none was given.
var ErrNoOrigin = errors.New("logbook: no origin: the file is new and Options.Origin is empty")

// ErrOtherOrigin means the file belongs to another origin than the one Open was given: the host
// got a new device id and kept its old file.
var ErrOtherOrigin = errors.New("logbook: the file belongs to another origin")

// ErrCorrupt means SQLite found the file is not a database, or a damaged one (logbook.md 14).
var ErrCorrupt = errors.New("logbook: the file is corrupt")

// InvalidError is a refused write (the API's invalid): what is wrong and what to do instead.
type InvalidError struct {
	Msg  string
	Hint string
}

func (e *InvalidError) Error() string {
	if e.Hint == "" {
		return "logbook: " + e.Msg
	}
	return "logbook: " + e.Msg + " (" + e.Hint + ")"
}

// Is makes errors.Is(err, ErrInvalid) true.
func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(hint, format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...), Hint: hint}
}

// Options configure Open.
type Options struct {
	// Origin is this hab's device id. Empty uses the origin the file records, so the CLI can
	// write while gff-hab is down without knowing it (4.3). A file made by another origin is
	// refused.
	Origin string
	// Now is the wall clock; tests pass a fake one. Default time.Now.
	Now func() time.Time
	// Logf logs what the user should know once: a skipped op, a clock far ahead, a shared origin.
	Logf func(format string, args ...any)
	// BusyTimeout is how long a write waits for the other writer's lock. Default 5 s (4.3).
	BusyTimeout time.Duration

	// noSync turns fsync off: the property tests open thousands of throwaway files.
	noSync bool
}

// Store is one replica: the op log and the records folded from it, in one SQLite file. Several
// Stores, in one process or in several, may open the same file; SQLite's write lock orders their
// writes.
type Store struct {
	db     *sql.DB
	origin string
	now    func() time.Time
	logf   func(string, ...any)
	mu     sync.Mutex // one write at a time per Store; SQLite's lock orders Stores and processes
	// repairMu runs one RepairOrphans at a time, so two callers never forget one orphan twice.
	repairMu sync.Mutex
	stmts    stmts
	// maxPages bounds the pages of one Sync in each direction; 0 means maxSyncPages. Tests lower it.
	maxPages int
}

// Open opens or makes the replica at path, with owner-only permissions, in WAL mode.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("logbook: path %q holds ? or #", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	// FULL: in WAL mode NORMAL can lose a commit after a power loss, and the next write would
	// then reuse a seq that peers already hold.
	syncMode := "FULL"
	if opts.noSync {
		syncMode = "OFF"
	}
	dsn := path + "?_pragma=busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")" +
		"&_pragma=synchronous(" + syncMode + ")&_pragma=journal_mode(WAL)" +
		// secure_delete zeroes freed space, so a forgotten value leaves no copy in the page slack.
		"&_pragma=secure_delete(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, now: opts.Now, logf: opts.Logf}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if err := s.init(ctx, opts.Origin); err != nil {
		_ = db.Close()
		var se *sqlite.Error
		if errors.As(err, &se) && (se.Code()&0xff == sqlite3.SQLITE_NOTADB || se.Code()&0xff == sqlite3.SQLITE_CORRUPT) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil, err
	}
	return s, nil
}

// Close closes the file.
func (s *Store) Close() error {
	s.stmts.mu.Lock()
	for _, st := range s.stmts.m {
		_ = st.Close()
	}
	s.stmts.m = nil
	s.stmts.mu.Unlock()
	return s.db.Close()
}

// Origin is the device id this replica writes as.
func (s *Store) Origin() string { return s.origin }

const schema = `
CREATE TABLE IF NOT EXISTS ops (
	origin TEXT NOT NULL,
	seq INTEGER NOT NULL,
	wall INTEGER NOT NULL,
	counter INTEGER NOT NULL,
	batch_last INTEGER NOT NULL,
	kind TEXT NOT NULL,
	record TEXT NOT NULL,
	root TEXT NOT NULL,
	target TEXT,
	known INTEGER NOT NULL,
	raw BLOB NOT NULL,
	PRIMARY KEY (origin, seq)
);
CREATE INDEX IF NOT EXISTS ops_root ON ops(root);
CREATE INDEX IF NOT EXISTS ops_create ON ops(record) WHERE kind = 'create';
CREATE INDEX IF NOT EXISTS ops_target ON ops(target) WHERE target IS NOT NULL;
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS origins (
	origin TEXT PRIMARY KEY,
	held INTEGER NOT NULL DEFAULT 0,
	applied INTEGER NOT NULL DEFAULT 0,
	clock_ahead INTEGER NOT NULL DEFAULT 0,
	shared INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS folds (
	n INTEGER PRIMARY KEY AUTOINCREMENT,
	origin TEXT NOT NULL,
	seq INTEGER NOT NULL,
	UNIQUE (origin, seq)
);
`

// foldedSchema is the folded tables. Rebuild drops and refills their rows.
const foldedSchema = `
CREATE TABLE IF NOT EXISTS records (
	rid INTEGER PRIMARY KEY,
	root TEXT NOT NULL UNIQUE,
	id TEXT NOT NULL UNIQUE,
	type TEXT NOT NULL,
	project TEXT NOT NULL,
	status TEXT NOT NULL,
	title TEXT NOT NULL,
	deleted INTEGER NOT NULL,
	created_wall INTEGER NOT NULL,
	created_counter INTEGER NOT NULL,
	created_origin TEXT NOT NULL,
	revision INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS records_project ON records(project, type);
CREATE TABLE IF NOT EXISTS fields (root TEXT NOT NULL, field TEXT NOT NULL, value TEXT NOT NULL, op TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS fields_root ON fields(root);
CREATE TABLE IF NOT EXISTS members (root TEXT NOT NULL, field TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, op TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS members_root ON members(root);
CREATE TABLE IF NOT EXISTS links (root TEXT NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, pos INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS links_root ON links(root);
CREATE INDEX IF NOT EXISTS links_target ON links(target);
CREATE TABLE IF NOT EXISTS notes (root TEXT NOT NULL, op TEXT NOT NULL, wall INTEGER NOT NULL, counter INTEGER NOT NULL, origin TEXT NOT NULL, seq INTEGER NOT NULL, text TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS notes_root ON notes(root);
CREATE TABLE IF NOT EXISTS flags (root TEXT NOT NULL, kind TEXT NOT NULL, field TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS flags_root ON flags(root);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(title, body, notes, tags);
CREATE TABLE IF NOT EXISTS aliases (project TEXT PRIMARY KEY, canonical TEXT NOT NULL);
`

// foldedTables are the tables Rebuild empties; rootTables the ones keyed by a record's root.
var (
	foldedTables = []string{"records", "fields", "members", "links", "notes", "flags", "fts", "aliases"}
	rootTables   = []string{"records", "fields", "members", "links", "notes", "flags"}
)

func (s *Store) init(ctx context.Context, origin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema+foldedSchema+keysSchema+foldersSchema+orphansSchema); err != nil {
		return fmt.Errorf("logbook: schema: %w", err)
	}
	m, err := readMeta(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case m.origin == "" && origin == "":
		return ErrNoOrigin
	case m.origin == "":
		m.origin = origin
		m.nextSeq = 1
		m.foldVersion = foldVersion // a new file has nothing to rebuild
		if err := m.write(ctx, tx); err != nil {
			return err
		}
	case origin != "" && origin != m.origin:
		return fmt.Errorf("%w: %q, not %q", ErrOtherOrigin, m.origin, origin)
	}
	s.origin = m.origin
	if m.foldVersion != foldVersion {
		if err := s.rebuild(ctx, tx); err != nil {
			return err
		}
		m.foldVersion = foldVersion
		if err := m.write(ctx, tx); err != nil {
			return err
		}
	}
	var scanned int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key = 'orphans_scanned'`).Scan(&scanned); err != nil {
		return err
	}
	if scanned == 0 {
		if err := scanOrphans(ctx, tx, s.now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// meta is this replica's own counters (4.1).
type meta struct {
	origin      string
	nextSeq     uint64
	last        HLC
	foldVersion int
	// ownSeen is the highest seq of this replica's own origin that any peer said it holds (7.6,
	// Restored file). Above the replica's own gap-free run, the file was restored from an older
	// copy, and writes wait for the ops to come back.
	ownSeen uint64
}

func readMeta(ctx context.Context, tx txn) (meta, error) {
	m := meta{}
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return m, err
		}
		switch k {
		case "origin":
			m.origin = v
		case "next_seq":
			m.nextSeq, _ = strconv.ParseUint(v, 10, 64)
		case "last_wall":
			m.last.Wall, _ = strconv.ParseInt(v, 10, 64)
		case "last_counter":
			c, _ := strconv.ParseUint(v, 10, 32)
			m.last.Counter = uint32(c)
		case "fold_version":
			m.foldVersion, _ = strconv.Atoi(v)
		case "own_seen":
			m.ownSeen, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	m.last.Origin = m.origin
	return m, rows.Err()
}

func (m meta) write(ctx context.Context, tx txn) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES
		('origin', ?), ('next_seq', ?), ('last_wall', ?), ('last_counter', ?), ('fold_version', ?), ('own_seen', ?), ('schema', '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		m.origin, strconv.FormatUint(m.nextSeq, 10), strconv.FormatInt(m.last.Wall, 10),
		strconv.FormatUint(uint64(m.last.Counter), 10), strconv.Itoa(m.foldVersion), strconv.FormatUint(m.ownSeen, 10))
	return err
}

// scrub runs after a forget lands: a TRUNCATE checkpoint copies the redacted pages into the
// file and empties the WAL, whose older frames still hold the forgotten text (5.7). With
// secure_delete on, the file keeps no copy either. A reader that holds the WAL open makes the
// checkpoint stop short; it is logged, and the next forget's checkpoint tries again.
func (s *Store) scrub(ctx context.Context) {
	var busy, frames, done int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &done); err != nil {
		s.logf("logbook: checkpoint after a forget: %v", err)
	} else if busy != 0 {
		s.logf("logbook: checkpoint after a forget stopped short: a reader holds the log")
	}
}

// Rebuild drops the folded tables and folds every op again (4.1). An op this build has learned
// to fold since it was stored is folded now.
func (s *Store) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.rebuild(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) rebuild(ctx context.Context, tx txn) error {
	for _, t := range foldedTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fts(fts) VALUES('optimize')`); err != nil {
		return err
	}
	// Which ops this build folds may have changed.
	type kn struct {
		origin string
		seq    uint64
		known  bool
	}
	var all []kn
	rows, err := tx.QueryContext(ctx, `SELECT raw FROM ops`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		o, err := decodeOp(raw)
		if err != nil {
			rows.Close()
			return fmt.Errorf("logbook: stored op unreadable: %w", err)
		}
		all = append(all, kn{o.Origin, o.Seq, o.known()})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range all {
		if _, err := tx.ExecContext(ctx, `UPDATE ops SET known = ? WHERE origin = ? AND seq = ?`,
			boolInt(k.known), k.origin, k.seq); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE origins SET held = 0, applied = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO origins(origin) SELECT DISTINCT origin FROM ops`); err != nil {
		return err
	}
	origins, err := queryStrings(ctx, tx, `SELECT origin FROM origins`)
	if err != nil {
		return err
	}
	for _, o := range origins {
		if _, _, err := advance(ctx, tx, o); err != nil {
			return err
		}
	}
	roots, err := queryStrings(ctx, tx, `SELECT o.root FROM ops o JOIN origins g ON g.origin = o.origin
		WHERE o.kind = 'create' AND o.known = 1 AND o.seq <= g.applied`)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, r := range roots {
		set[r] = true
	}
	_, err = s.refold(ctx, tx, set)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryStrings(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
