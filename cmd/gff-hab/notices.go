package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// The notice store's bounds (capcom.md 8.5): notice.get reads back the last noticeKeep notices
// raised within noticeAge, title and body cut to noticeTitleMax and noticeBodyMax bytes.
const (
	noticeKeep     = 500
	noticeAge      = 7 * 24 * time.Hour
	noticeTitleMax = 1 << 10
	noticeBodyMax  = 4 << 10
)

// noticesFile is the notice store's file in the state directory.
const noticesFile = "notices.json"

// noticeLog keeps the recent notices this hab raised, and the agent-state banners it recorded
// as notices, for notice.get and Capcom's notification context. With a path it is saved there
// on every add, so a tap on a banner after a gff-hab restart still resolves.
type noticeLog struct {
	path string // "" keeps the log in memory only
	now  func() time.Time
	logf func(string, ...any)

	mu    sync.Mutex
	order []string
	byID  map[string]api.Notice
}

// noticeFile is the store's file format: the notices, oldest first.
type noticeFile struct {
	Notices []api.Notice `json:"notices"`
}

// openNoticeLog loads the store at <dir>/notices.json. A file that does not parse is kept
// aside as notices.json.bad and the log starts empty; that is logged, never fatal.
func openNoticeLog(dir string, now func() time.Time, logf func(string, ...any)) *noticeLog {
	l := &noticeLog{path: filepath.Join(dir, noticesFile), now: now, logf: logf}
	b, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return l
	case err != nil:
		l.log("notices: %v; starting empty", err)
		return l
	}
	var f noticeFile
	if err := json.Unmarshal(b, &f); err != nil {
		aside := l.path + ".bad"
		if rerr := os.Rename(l.path, aside); rerr != nil {
			l.log("notices: %s does not parse (%v) and could not be kept aside (%v); starting empty", l.path, err, rerr)
		} else {
			l.log("notices: %s does not parse (%v); kept as %s, starting empty", l.path, err, aside)
		}
		return l
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range f.Notices {
		if n.ID != "" {
			l.putLocked(n)
		}
	}
	l.pruneLocked()
	return l
}

func (l *noticeLog) log(format string, args ...any) {
	if l.logf != nil {
		l.logf(format, args...)
	}
}

func (l *noticeLog) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// add keeps n and saves the store. A failed save is logged: the notice still reads back until
// gff-hab stops.
func (l *noticeLog) add(n api.Notice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.putLocked(n)
	l.pruneLocked()
	if l.path == "" {
		return
	}
	if err := l.saveLocked(); err != nil {
		l.log("notices: save: %v", err)
	}
}

// putLocked keeps n, cut to the bounds, as the newest notice.
func (l *noticeLog) putLocked(n api.Notice) {
	if l.byID == nil {
		l.byID = map[string]api.Notice{}
	}
	n.Title = cutUTF8(n.Title, noticeTitleMax)
	n.Body = cutUTF8(n.Body, noticeBodyMax)
	if _, ok := l.byID[n.ID]; ok {
		for i, id := range l.order {
			if id == n.ID {
				l.order = append(l.order[:i], l.order[i+1:]...)
				break
			}
		}
	}
	l.byID[n.ID] = n
	l.order = append(l.order, n.ID)
}

// pruneLocked drops notices past the age, then the oldest past the count. A notice whose time
// does not parse goes by the count alone.
func (l *noticeLog) pruneLocked() {
	cut := l.clock().Add(-noticeAge)
	keep := l.order[:0]
	for _, id := range l.order {
		if noticeExpired(l.byID[id], cut) {
			delete(l.byID, id)
			continue
		}
		keep = append(keep, id)
	}
	l.order = keep
	for len(l.order) > noticeKeep {
		delete(l.byID, l.order[0])
		l.order = l.order[1:]
	}
}

func noticeExpired(n api.Notice, cut time.Time) bool {
	at, err := time.Parse(time.RFC3339, n.At)
	return err == nil && at.Before(cut)
}

// get returns a notice within the bounds.
func (l *noticeLog) get(id string) (api.Notice, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.byID[id]
	if !ok || noticeExpired(n, l.clock().Add(-noticeAge)) {
		return api.Notice{}, false
	}
	return n, true
}

// saveLocked writes the store through a temporary file and a rename, mode 0600.
func (l *noticeLog) saveLocked() error {
	f := noticeFile{Notices: make([]api.Notice, 0, len(l.order))}
	for _, id := range l.order {
		f.Notices = append(f.Notices, l.byID[id])
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeStateFile(l.path, append(b, '\n'))
}

// cutUTF8 cuts s to at most n bytes, on a character boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && s[i]&0xC0 == 0x80 {
		i--
	}
	return s[:i]
}

// writeStateFile writes p through a temporary file in its folder, synced, then renamed into
// place, mode 0600. A file or link left at the temporary name is removed first and the new one
// is made with O_EXCL, so a planted link is never written through.
func writeStateFile(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %w", p, err)
	}
	return os.Rename(tmp, p)
}
