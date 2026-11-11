package logbook

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"time"
)

// Orphan redactions (logbook.md 7.6, Redacted ops). A page carries a redacted op with a forget
// that names it wherever paging can, but an op can still arrive alone: its forget lies more than
// a page ahead on the op's own origin, or two origins' forgets need each other across more than
// a page. If the forget's writer dies then, no replica will ever send the forget, and a replica
// that held the op's text before the forget keeps it. So a replica that holds a redacted op
// while no forget of it is held writes a forget of its own, as the hab, once the forget had its
// chance to arrive. That forget travels like any op and redacts the text holder's copy: every
// replica converges on the redacted form, and no text travels.

// markerRe matches a JSON string that is exactly a forgotten value.
var markerRe = regexp.MustCompile(`"` + forgottenPrefix + `[0-9a-f]{64}"`)

// OrphanGrace is how long an orphan waits for its forget outside an exchange this replica dials:
// longer than one exchange, so a forget a later page carries arrives first. gff-hab's sync
// service repairs older orphans at each round (lbsync), so a hab that only answers links still
// repairs; the dialer also repairs at the end of each Sync.
const OrphanGrace = 2 * time.Minute

// RepairActor is the actor of a forget the hab writes to repair one it never received.
var RepairActor = &Actor{Kind: ActorHab}

// ActorHab is the actor kind of a change the hab itself made, not a person or an agent.
const ActorHab = "hab"

const orphansSchema = `
CREATE TABLE IF NOT EXISTS orphans (origin TEXT NOT NULL, seq INTEGER NOT NULL, seen INTEGER NOT NULL, PRIMARY KEY (origin, seq));
`

// redactedForm reports whether raw is an op in redacted form: it holds a forgotten value and a
// redaction would change no byte of it. A redaction writes the op's keys sorted, and an op as
// written (encode) never has them sorted, so only bytes a redaction made pass: a value that only
// looks like a forgotten one, written by a person or an agent, never does.
func redactedForm(raw []byte) bool {
	if !markerRe.Match(raw) {
		return false
	}
	r, err := redact(raw)
	return err == nil && bytes.Equal(r, raw)
}

// noteOrphan records op id as an orphan when raw is in redacted form and no held forget names it.
func noteOrphan(ctx context.Context, tx txn, id OpID, kind Kind, raw []byte, now int64) error {
	if kind == KindForget || !redactedForm(raw) {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ops WHERE target = ?`, id.String()).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO orphans(origin, seq, seen) VALUES(?, ?, ?)`, id.Origin, id.Seq, now)
	return err
}

// scanOrphans fills the orphans table from the log, once per file: a file from before the table
// may already hold orphans.
func scanOrphans(ctx context.Context, tx txn, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT origin, seq, kind, raw FROM ops WHERE kind != 'forget' AND instr(raw, ?) > 0`, forgottenPrefix)
	if err != nil {
		return err
	}
	type cand struct {
		id   OpID
		kind Kind
		raw  []byte
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var kind string
		if err := rows.Scan(&c.id.Origin, &c.id.Seq, &kind, &c.raw); err != nil {
			rows.Close()
			return err
		}
		c.kind = Kind(kind)
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cs {
		if err := noteOrphan(ctx, tx, c.id, c.kind, c.raw, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES('orphans_scanned', '1')`)
	return err
}

// RepairOrphans writes a forget, as the hab, of each orphan that has waited longer than grace for
// its forget (OrphanGrace from gff-hab).
func (s *Store) RepairOrphans(ctx context.Context, grace time.Duration) error {
	return s.repairBefore(ctx, s.now().Add(-grace))
}

// repairBefore writes a forget, as the hab, of each orphan first seen before cutoff whose forget
// still has not arrived. An orphan whose op a forget cannot name (a kind this build does not
// know) is dropped. It does nothing while the start hold lasts; a failed write is logged and
// tried again at the next call.
func (s *Store) repairBefore(ctx context.Context, cutoff time.Time) error {
	if until, err := s.heldUntil(ctx); err != nil || (!until.IsZero() && s.now().Before(until)) {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT origin, seq FROM orphans WHERE seen < ? ORDER BY seen, origin, seq`, cutoff.UnixMilli())
	if err != nil {
		return err
	}
	var ids []OpID
	for rows.Next() {
		var id OpID
		if err := rows.Scan(&id.Origin, &id.Seq); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		_, err := s.Write(ctx, Batch{Actor: RepairActor, Steps: []Step{{Kind: KindForget, Target: id}}})
		var inv *InvalidError
		switch {
		case err == nil:
		case errors.As(err, &inv) || errors.Is(err, ErrNotFound):
			if _, err := s.db.ExecContext(ctx, `DELETE FROM orphans WHERE origin = ? AND seq = ?`, id.Origin, id.Seq); err != nil {
				return err
			}
		default:
			s.logf("logbook: repairing the missing forget of %s: %v", id, err)
			return nil
		}
	}
	return nil
}

// repair runs RepairOrphans for an exchange: an error is logged, never the exchange's.
func (s *Store) repair(ctx context.Context, cutoff time.Time) {
	if err := s.repairBefore(ctx, cutoff); err != nil {
		s.logf("logbook: repairing missing forgets: %v", err)
	}
}
