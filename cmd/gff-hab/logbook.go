package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
func openLogbook(ctx context.Context, dir, origin string, logf func(string, ...any)) (*logbook.Store, error) {
	path := filepath.Join(dir, logbookFile)
	opts := logbook.Options{Origin: origin, Logf: logf}
	s, err := logbook.Open(ctx, path, opts)
	switch {
	case err == nil:
		return s, nil
	case errors.Is(err, logbook.ErrCorrupt):
		aside, merr := setAside(path, "corrupt")
		if merr != nil {
			return nil, fmt.Errorf("%v; and setting it aside: %v", err, merr)
		}
		logf("logbook: %v; set aside as %s. Starting empty: the peers send a full copy, and ops of this hab no peer held are lost", err, aside)
		return logbook.Open(ctx, path, opts)
	case errors.Is(err, logbook.ErrOtherOrigin):
		aside, merr := setAside(path, "other-origin")
		if merr != nil {
			return nil, fmt.Errorf("%v; and setting it aside: %v", err, merr)
		}
		s, err := logbook.Open(ctx, path, opts)
		if err != nil {
			return nil, err
		}
		n, cerr := copyOps(ctx, aside, s)
		if cerr != nil {
			logf("logbook: copying the ops of %s: %v", aside, cerr)
		}
		logf("logbook: %s belonged to another device id; set aside as %s, %d ops copied into a new replica", path, aside, n)
		return s, nil
	}
	return nil, err
}

// setAside renames the store's file, and its WAL and shared-memory files, to <path>.<why>-<unix>.
func setAside(path, why string) (string, error) {
	aside := path + "." + why + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if err := os.Rename(path+ext, aside+ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return aside, err
		}
	}
	return aside, nil
}

// copyOps applies every op of the replica file at from to s, page by page.
func copyOps(ctx context.Context, from string, s *logbook.Store) (int, error) {
	old, err := logbook.Open(ctx, from, logbook.Options{})
	if err != nil {
		return 0, err
	}
	defer old.Close()
	n := 0
	for {
		have, err := s.Vector(ctx)
		if err != nil {
			return n, err
		}
		ops, more, err := old.OpsAfter(ctx, have, logbook.PageOps, logbook.PageBytes)
		if err != nil || len(ops) == 0 {
			return n, err
		}
		res, err := s.Apply(ctx, ops)
		n += res.Stored
		if err != nil || !more || res.Stored == 0 {
			return n, err
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
