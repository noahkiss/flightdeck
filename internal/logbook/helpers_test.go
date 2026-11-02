package logbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// base is 2026-11-02 00:00 UTC; examples give times as minutes after it.
var base = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func at(hhmm string) time.Time {
	parts := strings.SplitN(hhmm, ":", 2)
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return base.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// fakeClock is a replica's wall clock: a real time plus an offset the test moves.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	offset time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t.Add(c.offset)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// replica is a store on a temp file with its own fake clock and a log of what it reported.
type replica struct {
	*Store
	name  string
	path  string
	clock *fakeClock
	mu    sync.Mutex
	logs  []string
}

func newReplica(t testing.TB, name string) *replica {
	t.Helper()
	return openReplica(t, name, filepath.Join(t.TempDir(), "logbook.db"))
}

func openReplica(t testing.TB, name, path string) *replica {
	t.Helper()
	r := &replica{name: name, path: path, clock: &fakeClock{t: base}}
	s, err := Open(context.Background(), path, Options{Origin: "SHA256:" + name, Now: r.clock.Now, noSync: true,
		Logf: func(f string, a ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(f, a...))
			r.mu.Unlock()
		}})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.Store = s
	return r
}

func (r *replica) logged() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

func (r *replica) write(t testing.TB, actor *Actor, steps ...Step) WriteResult {
	t.Helper()
	res, err := r.Write(context.Background(), Batch{Actor: actor, Steps: steps})
	if err != nil {
		t.Fatalf("%s write: %v", r.name, err)
	}
	return res
}

func (r *replica) get(t testing.TB, id string) *Record {
	t.Helper()
	rec, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("%s get %s: %v", r.name, id, err)
	}
	return rec
}

// push sends every op from holds that to lacks, as sync will, in pages of 7.
func push(t testing.TB, from, to *replica) { pushPages(t, from, to, 7) }

func pushPages(t testing.TB, from, to *replica, page int) {
	t.Helper()
	ctx := context.Background()
	for {
		have, err := to.Vector(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ops, more, err := from.OpsAfter(ctx, have, page, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) == 0 {
			return
		}
		res, err := to.Apply(ctx, ops)
		if err != nil {
			t.Fatalf("apply %s -> %s: %v", from.name, to.name, err)
		}
		if !more || res.Stored == 0 {
			return
		}
	}
}

// link exchanges ops both ways.
func link(t testing.TB, a, b *replica) {
	t.Helper()
	push(t, a, b)
	push(t, b, a)
}

func set(id, field string, v any) Step {
	return Step{Kind: KindSet, Record: id, Field: field, Value: v}
}
func add(id, field string, v any) Step {
	return Step{Kind: KindAdd, Record: id, Field: field, Value: v}
}
func remove(id, field string, v any) Step {
	return Step{Kind: KindRemove, Record: id, Field: field, Value: v}
}
func note(id, text string) Step {
	return Step{Kind: KindAppend, Record: id, Field: "notes", Value: text}
}

func create(id, typ string, fields map[string]any) Step {
	return Step{Kind: KindCreate, Record: id, Type: typ, Fields: fields}
}

func flagsOf(r *Record, kind string) []Flag {
	var out []Flag
	for _, f := range r.Flags {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func str(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return string(v)
	}
	return s
}

// dump writes every folded table in a fixed order, without row ids, so two replicas, or one
// before and after a rebuild, compare as text.
func dump(t testing.TB, s *Store) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	q := func(title, query string) {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("dump %s: %v", title, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		b.WriteString("== " + title + "\n")
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if i > 0 {
					b.WriteByte('|')
				}
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprint(&b, v)
			}
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	q("records", `SELECT root, id, type, project, status, title, deleted, created_wall, created_counter, created_origin, revision, data FROM records ORDER BY root`)
	q("fields", `SELECT * FROM fields ORDER BY root, field`)
	q("members", `SELECT * FROM members ORDER BY root, field, key`)
	q("links", `SELECT * FROM links ORDER BY root, kind, target, pos`)
	q("notes", `SELECT * FROM notes ORDER BY root, wall, counter, origin, seq`)
	q("flags", `SELECT * FROM flags ORDER BY root, kind, field, data`)
	q("aliases", `SELECT * FROM aliases ORDER BY project`)
	q("fts", `SELECT r.root, f.title, f.body, f.notes, f.tags FROM fts f JOIN records r ON r.rid = f.rowid ORDER BY r.root`)
	return b.String()
}

// rawOp encodes an op as another build would send it.
func rawOp(t testing.TB, o map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func isInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie) && errors.Is(err, ErrInvalid)
}

// openRaw opens a store file with database/sql for checks the API does not expose.
func openRaw(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// nightly reports whether the nightly counts apply ($GFF_LOGBOOK_NIGHTLY=1).
func nightly() bool { return os.Getenv("GFF_LOGBOOK_NIGHTLY") == "1" }

// seed returns $GFF_LOGBOOK_SEED, else a fresh seed, and logs it.
func seed(t testing.TB) int64 {
	t.Helper()
	s := time.Now().UnixNano()
	if v, err := strconv.ParseInt(os.Getenv("GFF_LOGBOOK_SEED"), 10, 64); err == nil {
		s = v
	}
	t.Logf("seed %d (rerun with GFF_LOGBOOK_SEED=%d)", s, s)
	return s
}
