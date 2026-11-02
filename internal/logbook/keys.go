package logbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Idempotency keys (logbook.md 4.5, stage 4): a write that carries a key records the key and
// its result in the file, in the write's own transaction. A second write with the key applies
// nothing and returns the first result, whichever writer of the file made it: gff-hab or the CLI
// fallback. Keys stay for KeyKeep, then go at the next keyed write. A refused write stores no key,
// so a retry runs again. Keys are this replica's own: sync never sends them.

// KeyKeep is how long a key is remembered: long enough for a phone that queued a write offline
// for weeks to send it again (14).
const KeyKeep = 90 * 24 * 60 * 60 * 1000 // ms

// MaxKey bounds a key's length, as the control connection's keys (hab-logic-api.md 3.9).
const MaxKey = 128

// ErrKeyReused means a key the file holds came with another request. It is the API's conflict.
var ErrKeyReused = errors.New("logbook: this idempotency key was used for another write; make a new key for a new write")

const keysSchema = `
CREATE TABLE IF NOT EXISTS keys (key TEXT PRIMARY KEY, hash TEXT NOT NULL, result TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS keys_at ON keys(at);
`

// keyed returns the stored result of key, or found false. A key held with another hash is
// ErrKeyReused.
func keyed(ctx context.Context, tx txn, key, hash string) (res WriteResult, found bool, err error) {
	var h, data string
	err = tx.QueryRowContext(ctx, `SELECT hash, result FROM keys WHERE key = ?`, key).Scan(&h, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return res, false, nil
	}
	if err != nil {
		return res, false, err
	}
	if h != hash {
		return res, true, ErrKeyReused
	}
	if err := json.Unmarshal([]byte(data), &res); err != nil {
		return res, true, err
	}
	res.Repeated = true
	return res, true, nil
}

// keep stores key with its result, and drops keys older than KeyKeep.
func keep(ctx context.Context, tx txn, key, hash string, res WriteResult, nowMS int64) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keys WHERE at < ?`, nowMS-KeyKeep); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO keys(key, hash, result, at) VALUES(?, ?, ?, ?)`, key, hash, string(data), nowMS)
	return err
}
