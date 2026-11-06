package lbsync

import (
	"context"
	"database/sql"
	"net/url"
)

// watcher pins one read-only connection: data_version values are comparable only on
// the same connection. Other connections' commits change it, including WAL commits.
type watcher struct {
	db   *sql.DB
	conn *sql.Conn
}

func openWatcher(ctx context.Context, path string) (*watcher, error) {
	if path == "" {
		return nil, nil
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	w := &watcher{db: db, conn: conn}
	if _, err := w.Version(ctx); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}
func (w *watcher) Version(ctx context.Context) (int64, error) {
	var v int64
	err := w.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
func (w *watcher) Close() { w.conn.Close(); w.db.Close() }
