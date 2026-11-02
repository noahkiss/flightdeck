package logbook

import (
	"context"
	"database/sql"
	"sync"
)

// txn is a write transaction that runs each statement prepared once per Store. SQLite parses a
// statement on every unprepared run, and parsing was most of a write's cost.
type txn struct {
	*sql.Tx
	s *Store
}

func (s *Store) begin(ctx context.Context) (txn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	return txn{tx, s}, err
}

// stmts caches prepared statements by their text.
type stmts struct {
	mu sync.Mutex
	m  map[string]*sql.Stmt
}

func (s *Store) prepared(ctx context.Context, query string) *sql.Stmt {
	s.stmts.mu.Lock()
	defer s.stmts.mu.Unlock()
	if st, ok := s.stmts.m[query]; ok {
		return st
	}
	st, err := s.db.PrepareContext(ctx, query)
	if err != nil {
		return nil // a script of several statements: run it unprepared
	}
	if s.stmts.m == nil {
		s.stmts.m = map[string]*sql.Stmt{}
	}
	s.stmts.m[query] = st
	return st
}

func (t txn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if st := t.s.prepared(ctx, query); st != nil {
		return t.Tx.StmtContext(ctx, st).ExecContext(ctx, args...)
	}
	return t.Tx.ExecContext(ctx, query, args...)
}

func (t txn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if st := t.s.prepared(ctx, query); st != nil {
		return t.Tx.StmtContext(ctx, st).QueryContext(ctx, args...)
	}
	return t.Tx.QueryContext(ctx, query, args...)
}

func (t txn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if st := t.s.prepared(ctx, query); st != nil {
		return t.Tx.StmtContext(ctx, st).QueryRowContext(ctx, args...)
	}
	return t.Tx.QueryRowContext(ctx, query, args...)
}
