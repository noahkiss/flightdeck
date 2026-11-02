package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/lbsync"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/peers"
)

// logbookFile is the hab's Logbook replica in its state directory (logbook.md 4.1).
const logbookFile = "logbook.db"

// openLogbook opens the hab's replica at <state>/logbook.db with this hab's device id as its
// origin. Two cases set the file aside, under a name that keeps it, and start a new one
// (logbook.md 14, "Built (stage 3)"):
//
//   - A corrupt file: the new replica pulls a full copy from the peers. Its own ops that no peer
//     held are lost; the log says so.
//   - A file of another origin: the host got a new device id and kept its state folder. The old
//     file's ops are copied into the new replica as the old origin's ops, so nothing it held is
//     lost, and they reach the peers as any origin's ops do.
//
// A set-aside cut short by a crash is finished first (setAside). At every start, the ops of each
// other-origin file not yet marked copied are copied again, until a copy completes; then a
// marker file beside it says so. A copy that fails is logged, shows in the store's Status, and
// is tried again at the next start. Copying is idempotent: ops are keyed by op id.
func openLogbook(ctx context.Context, dir, origin string, logf func(string, ...any)) (*logbook.Store, error) {
	path := filepath.Join(dir, logbookFile)
	if err := finishSetAside(path, logf); err != nil {
		return nil, fmt.Errorf("finishing a set-aside: %w", err)
	}
	opts := logbook.Options{Origin: origin, Logf: logf}
	s, err := logbook.Open(ctx, path, opts)
	switch {
	case err == nil:
	case errors.Is(err, logbook.ErrCorrupt):
		aside, merr := setAside(path, "corrupt")
		if merr != nil {
			return nil, fmt.Errorf("%v; and setting it aside: %v", err, merr)
		}
		logf("logbook: %v; set aside as %s. Starting empty: the peers send a full copy, and ops of this hab no peer held are lost", err, aside)
		if s, err = logbook.Open(ctx, path, opts); err != nil {
			return nil, err
		}
	case errors.Is(err, logbook.ErrOtherOrigin):
		aside, merr := setAside(path, otherOrigin)
		if merr != nil {
			return nil, fmt.Errorf("%v; and setting it aside: %v", err, merr)
		}
		logf("logbook: %s belonged to another device id; set aside as %s, and its ops are copied into a new replica", path, aside)
		if s, err = logbook.Open(ctx, path, opts); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	copyAside(ctx, path, s, logf)
	return s, nil
}

// otherOrigin names a set-aside file of another origin; copiedMark the marker beside one whose
// ops are all copied in.
const (
	otherOrigin = "other-origin"
	copiedMark  = ".copied"
)

// copyAside copies the ops of each other-origin file beside path that no marker says is done.
func copyAside(ctx context.Context, path string, s *logbook.Store, logf func(string, ...any)) {
	files, _ := filepath.Glob(path + "." + otherOrigin + "-*")
	for _, f := range files {
		if strings.HasSuffix(f, "-wal") || strings.HasSuffix(f, "-shm") || strings.HasSuffix(f, copiedMark) {
			continue
		}
		if _, err := os.Stat(f + copiedMark); err == nil {
			continue
		}
		name := filepath.Base(f)
		n, err := copyOps(ctx, f, s)
		if err == nil {
			err = writeMark(f + copiedMark)
		}
		if err != nil {
			logf("logbook: copying the ops of %s: %v (%d ops copied); tried again at the next start", f, err, n)
			if nerr := s.NoteUncopied(ctx, name, err); nerr != nil {
				logf("logbook: noting %s: %v", name, nerr)
			}
			continue
		}
		if nerr := s.NoteUncopied(ctx, name, nil); nerr != nil {
			logf("logbook: noting %s: %v", name, nerr)
		}
		logf("logbook: the ops of %s are all copied in (%d new)", f, n)
	}
}

// writeMark writes the marker file that says a set-aside file's ops are all copied in.
func writeMark(p string) error {
	if err := os.WriteFile(p, []byte("Every op of this file is copied into logbook.db.\n"), 0o600); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// setAside renames the store's file, and its WAL and shared-memory files, to <path>.<why>-<unix>.
//
// Order, for a crash at any point: the WAL first, then the shared-memory file, then the file
// itself, with the folder synced after each rename. The WAL belongs to the file: SQLite does not
// check that a WAL matches its database, so the file renamed first would leave its WAL beside
// path, and the new replica made at path would replay the old file's pages. Renamed first, the
// WAL waits under the aside name while the file still sits at path; finishSetAside sees that at
// the next start and renames the file to the WAL's name before anything opens it.
func setAside(path, why string) (string, error) {
	aside := path + "." + why + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	dir := filepath.Dir(path)
	for _, ext := range []string{"-wal", "-shm"} {
		if err := os.Rename(path+ext, aside+ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return aside, err
		}
		if err := syncDir(dir); err != nil {
			return aside, err
		}
	}
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	return aside, syncDir(dir)
}

// finishSetAside completes a set-aside a crash cut short: a WAL or shared-memory file under an
// aside name whose file is not there yet, while path still holds the file. It renames the file
// to that name.
func finishSetAside(path string, logf func(string, ...any)) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	for _, why := range []string{"corrupt", otherOrigin} {
		for _, ext := range []string{"-wal", "-shm"} {
			files, _ := filepath.Glob(path + "." + why + "-*" + ext)
			for _, f := range files {
				aside := strings.TrimSuffix(f, ext)
				if _, err := os.Stat(aside); err == nil {
					continue
				}
				if err := os.Rename(path, aside); err != nil {
					return err
				}
				logf("logbook: a set-aside of %s was cut short; finished it as %s", path, aside)
				return syncDir(filepath.Dir(path))
			}
		}
	}
	return nil
}

// syncDir makes the renames in dir durable. Some file systems refuse a folder sync; the rename
// is done either way.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	_ = d.Sync()
	return d.Close()
}

// copyPageOps is the most ops copyOps moves at once; tests lower it.
var copyPageOps = logbook.PageOps

// copyOps applies every op of the replica file at from to s, page by page. An origin whose next
// op s refuses is passed over, so the others still copy; the error then names it.
func copyOps(ctx context.Context, from string, s *logbook.Store) (int, error) {
	old, err := logbook.Open(ctx, from, logbook.Options{})
	if err != nil {
		return 0, err
	}
	defer old.Close()
	n := 0
	passed := map[string]bool{}
	for {
		have, err := s.Vector(ctx)
		if err != nil {
			return n, err
		}
		want := maps.Clone(have)
		for o := range passed {
			want[o] = math.MaxUint64
		}
		ops, more, err := old.OpsAfter(ctx, want, copyPageOps, logbook.PageBytes)
		if err != nil {
			return n, err
		}
		if len(ops) > 0 {
			res, err := s.Apply(ctx, ops)
			n += res.Stored
			if err != nil {
				return n, err
			}
			after, err := s.Vector(ctx)
			if err != nil {
				return n, err
			}
			for o := range logbook.PageVector(ops) {
				if after[o] <= have[o] {
					passed[o] = true
				}
			}
		}
		if len(ops) == 0 || !more {
			if len(passed) > 0 {
				return n, fmt.Errorf("the ops of %s were refused", strings.Join(slices.Sorted(maps.Keys(passed)), ", "))
			}
			return n, nil
		}
	}
}

// logbookPeers is the peer list as Logbook sync reads it.
type logbookPeers struct{ m *peers.Manager }

func (c logbookPeers) Links() []lbsync.Link {
	ls := c.m.Links()
	out := make([]lbsync.Link, 0, len(ls))
	for _, l := range ls {
		ll := lbsync.Link{Label: l.Label, DeviceID: l.DeviceID, Online: l.Online, Caps: l.Caps}
		if l.Conn != nil {
			ll.Caller = l.Conn
		}
		out = append(out, ll)
	}
	return out
}
