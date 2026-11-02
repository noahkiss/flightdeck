package logbook

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A file restored from an older copy, with the same device id, holds fewer of its own ops than
// the peers do. Its next write would take a seq a peer already holds under another op, the peers
// would mark the origin shared, and replicas would keep whichever op came first: they diverge
// for good. Two guards keep that from happening (logbook.md 7.6, Restored file):
//
//   - Own seq seen. Every pull answer, push answer and pull request names the peer's vector. The
//     store keeps the highest seq of its own origin any of them held (meta own_seen). While that
//     is above its own gap-free run, Write refuses with ErrRestoring; sync brings the ops back,
//     in seq order, and writes go on. It costs one meta read per write and one per page.
//   - Start hold. gff-hab holds writes for a few seconds after it starts, while a peer link
//     comes up and the first exchange runs (HoldWrites). A write in that time waits. The hold is
//     a lease in the file, so a crash never leaves writes held for longer than the lease.

// maxHold bounds one hold: a write never waits longer, whatever the file says.
const maxHold = 30 * time.Second

// holdPoll is how often a waiting write looks at the hold again.
const holdPoll = 50 * time.Millisecond

// NoteOwnSeen records that a peer holds this replica's own ops up to seq. When that is above the
// replica's own gap-free run, the file was restored from an older copy: it logs that once per
// rise, and Write refuses until the ops are back.
func (s *Store) NoteOwnSeen(ctx context.Context, seq uint64) error {
	var cur uint64
	var v string
	switch err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'own_seen'`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		cur, _ = strconv.ParseUint(v, 10, 64)
	}
	if seq <= cur {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m, err := readMeta(ctx, tx)
	if err != nil {
		return err
	}
	if seq <= m.ownSeen {
		return nil
	}
	m.ownSeen = seq
	if err := m.write(ctx, tx); err != nil {
		return err
	}
	g, err := originRow(ctx, tx, m.origin)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if seq > g.held {
		s.logf("logbook: RESTORED FILE: a peer holds this host's own ops up to seq %d, but this file holds only up to %d. "+
			"It was restored from an older copy. Writes are refused until sync brings the missing ops back; "+
			"if no peer that holds them will come back, give this host a new device id", seq, g.held)
	}
	return nil
}

// noteOwnIn is NoteOwnSeen with this replica's own seq read from a peer's vector.
func (s *Store) noteOwnIn(ctx context.Context, v Vector) error {
	if n := v[s.origin]; n > 0 {
		return s.NoteOwnSeen(ctx, n)
	}
	return nil
}

// HoldWrites makes writes to the file wait until until, at most maxHold from now: gff-hab's start
// hold. A zero until ends a hold. Every Store on the file, the CLI's too, honours it.
func (s *Store) HoldWrites(ctx context.Context, until time.Time) error {
	var ms int64
	if !until.IsZero() {
		ms = min(until.UnixMilli(), s.now().Add(maxHold).UnixMilli())
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('hold_until', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(ms, 10))
	return err
}

// heldUntil is the end of the start hold, zero when none.
func (s *Store) heldUntil(ctx context.Context) (time.Time, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'hold_until'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	ms, _ := strconv.ParseInt(v, 10, 64)
	if ms == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms), nil
}

// waitHold waits while the file holds writes: until the hold ends or is lifted, at most maxHold.
func (s *Store) waitHold(ctx context.Context) error {
	deadline := time.Now().Add(maxHold)
	for {
		until, err := s.heldUntil(ctx)
		if err != nil {
			return err
		}
		if until.IsZero() || !s.now().Before(until) || !time.Now().Before(deadline) {
			return nil
		}
		t := time.NewTimer(min(until.Sub(s.now()), holdPoll))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// NoteUncopied records that the ops of the set-aside file name are not all copied into this
// replica yet, with why; a nil err clears it once they are. Status lists what is left.
func (s *Store) NoteUncopied(ctx context.Context, name string, err error) error {
	key := "uncopied:" + name
	if err == nil {
		_, e := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key)
		return e
	}
	_, e := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, err.Error())
	return e
}

// uncopied lists the set-aside files whose ops are not all copied in, as "name: why".
func (s *Store) uncopied(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta WHERE key LIKE 'uncopied:%' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimPrefix(k, "uncopied:")+": "+v)
	}
	return out, rows.Err()
}
