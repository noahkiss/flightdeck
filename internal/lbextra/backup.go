package lbextra

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// BackupHour is the local hour of the nightly backup's slot (13).
const BackupHour = 3

const (
	backupPrefix = "logbook-"
	backupStamp  = "20060102T150405Z"
)

// Backup keeps nightly copies of the replica in Dir: logbook-<UTC time>.db, a SQLite copy that
// opens as a working replica, and logbook-<UTC time>.jsonl, the export (13).
type Backup struct {
	Dir  string
	Keep int
}

// LastSlot is the latest nightly slot at or before now: BackupHour in now's zone.
func LastSlot(now time.Time) time.Time {
	s := time.Date(now.Year(), now.Month(), now.Day(), BackupHour, 0, 0, 0, now.Location())
	if s.After(now) {
		s = time.Date(now.Year(), now.Month(), now.Day()-1, BackupHour, 0, 0, 0, now.Location())
	}
	return s
}

// Due reports whether no backup was made since the last slot. A hab that slept through several
// slots is due once: the backup it makes is after every one of them.
func (b Backup) Due(now time.Time) (bool, error) {
	stems, err := b.stems()
	if err != nil {
		return false, err
	}
	if len(stems) == 0 {
		return true, nil
	}
	last, err := time.Parse(backupStamp, stems[0])
	if err != nil {
		return true, nil
	}
	return last.Before(LastSlot(now)), nil
}

// Run makes one backup and removes the oldest past Keep. It returns the copy's path.
func (b Backup) Run(ctx context.Context, st *logbook.Store, now time.Time) (string, error) {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(b.Dir, 0o700); err != nil {
		return "", err
	}
	stem := backupPrefix + now.UTC().Format(backupStamp)
	db := filepath.Join(b.Dir, stem+".db")
	if err := st.Backup(ctx, db); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(b.Dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := st.Export(ctx, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(b.Dir, stem+".jsonl")); err != nil {
		return "", err
	}
	return db, b.prune()
}

// stems lists the backups' time stamps, newest first.
func (b Backup) stems() ([]string, error) {
	ents, err := os.ReadDir(b.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range ents {
		name := e.Name()
		rest, ok := strings.CutPrefix(name, backupPrefix)
		if !ok {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(rest, ".db"), ".jsonl")
		if stem == rest {
			continue
		}
		if _, err := time.Parse(backupStamp, stem); err != nil || seen[stem] {
			continue
		}
		seen[stem] = true
		out = append(out, stem)
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out, nil
}

func (b Backup) prune() error {
	stems, err := b.stems()
	if err != nil {
		return err
	}
	keep := b.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	for _, s := range stems[min(keep, len(stems)):] {
		for _, ext := range []string{".db", ".jsonl"} {
			if err := os.Remove(filepath.Join(b.Dir, backupPrefix+s+ext)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
