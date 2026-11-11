package lbsync

import (
	"context"
	"database/sql"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// waitingDSN opens path with a busy timeout, like every connection the store opens: these
// tests write beside a running hab, and a connection without one fails at once with
// SQLITE_BUSY while the hab holds the write lock.
func waitingDSN(path string) string {
	return path + "?_pragma=busy_timeout(10000)"
}

// A WAL rewrite can keep both sizes and timestamps unchanged. The committed
// data_version still changes on the observer's pinned connection.
func TestWatcherSameFileSignature(t *testing.T) {
	if path := os.Getenv("GFF_TEST_WATCHER_DB"); path != "" {
		db, err := sql.Open("sqlite", waitingDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("UPDATE probe SET value='after!'; PRAGMA wal_checkpoint(RESTART)"); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", waitingDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE probe(value TEXT); INSERT INTO probe VALUES('before'); PRAGMA wal_checkpoint(RESTART)"); err != nil {
		t.Fatal(err)
	}
	w, err := openWatcher(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	before, err := w.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var infos []os.FileInfo
	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, fi)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatcherSameFileSignature$")
	cmd.Env = append(os.Environ(), "GFF_TEST_WATCHER_DB="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer: %v %s", err, out)
	}
	for i, p := range []string{path, path + "-wal"} {
		if err := os.Chtimes(p, infos[i].ModTime(), infos[i].ModTime()); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != infos[i].Size() || !fi.ModTime().Equal(infos[i].ModTime()) {
			t.Fatalf("signature changed for file %d", i)
		}
	}
	after, err := w.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("missed another process's WAL commit with unchanged file signature")
	}
}

// The change loop must notice the CLI's commit even when every file-stat field is unchanged.
func TestDialerUnchangedFileSignature(t *testing.T) {
	if path := os.Getenv("GFF_TEST_SAME_SIGNATURE_DB"); path != "" {
		st, err := logbook.Open(context.Background(), path, logbook.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.Write(context.Background(), logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "external writer"})}}); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", waitingDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("PRAGMA wal_checkpoint(RESTART)"); err != nil {
			t.Fatal(err)
		}
		return
	}
	var arm atomic.Bool
	var ticks atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	a := newHab(t, "a", func(h *hab, c *Config) {
		c.checked = func() {
			ticks.Add(1)
			if arm.CompareAndSwap(true, false) {
				close(entered)
				<-release
			}
		}
	})
	b := newHab(t, "b")
	dial(a, b)
	waitFor(t, "first exchange", func() bool { _, ok := a.svc.LinkStats()["b"]; return ok })
	path := a.svc.cfg.Path
	db, err := sql.Open("sqlite", waitingDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Free pages preserve DB size; a large checkpointed WAL provides space to reuse.
	if _, err := db.Exec("CREATE TABLE reserve(value BLOB); INSERT INTO reserve VALUES(zeroblob(1048576)); DROP TABLE reserve; PRAGMA wal_checkpoint(RESTART)"); err != nil {
		t.Fatal(err)
	}
	// Opening a Store itself commits metadata. Warm that path before fixing the
	// signature, so its WAL high-water size includes the complete writer path.
	for range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDialerUnchangedFileSignature$")
		cmd.Env = append(os.Environ(), "GFF_TEST_SAME_SIGNATURE_DB="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("warmup writer: %v %s", err, out)
		}
	}
	waitFor(t, "warmup ops copied", func() bool { return seqOf(t, b, a.id) == 2 })
	start := ticks.Load()
	waitFor(t, "settled watcher", func() bool { return ticks.Load() >= start+3 })
	arm.Store(true)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop at boundary")
	}
	var infos []os.FileInfo
	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, fi)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDialerUnchangedFileSignature$")
	cmd.Env = append(os.Environ(), "GFF_TEST_SAME_SIGNATURE_DB="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writer: %v %s", err, out)
	}
	for i, p := range []string{path, path + "-wal"} {
		if err := os.Chtimes(p, infos[i].ModTime(), infos[i].ModTime()); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != infos[i].Size() || !fi.ModTime().Equal(infos[i].ModTime()) {
			t.Fatalf("signature changed for file %d: size %d -> %d", i, infos[i].Size(), fi.Size())
		}
	}
	release <- struct{}{}
	waitFor(t, "b gets unchanged-signature commit", func() bool { return seqOf(t, b, a.id) == 3 })
}
