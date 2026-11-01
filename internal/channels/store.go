package channels

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// The state file (mesh.md 6.5): <state>/channels/state.json holds every outbox, the inbox, the
// sent records and the received ids. Every change is saved whole through a synced temporary
// file and one rename, so a crash leaves the old state or the new one, never half of each. A
// file that does not parse is set aside as .bad.
const (
	dirName   = "channels"
	stateFile = "state.json"
)

func (s *Service) path() string {
	if s.cfg.State == "" {
		return ""
	}
	return filepath.Join(s.cfg.State, dirName, stateFile)
}

func (s *Service) load() error {
	p := s.path()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err // unreadable, not corrupt: refuse to start rather than lose mail
	default:
		st, err := decodeState(b)
		if err != nil {
			s.cfg.Logf("channels: %s: %v; set aside as .bad, starting empty", p, err)
			if err := os.Rename(p, p+".bad"); err != nil {
				return err
			}
		} else {
			s.st = st
			// An item marked here was being written, or returned by a wait, when the hab
			// stopped: it finishes not_delivered and is never written again.
			for _, it := range st.Inbox {
				if it.Attempting {
					s.settle[it.Message.ID] = stoppedWriting
				}
			}
		}
	}
	b, err = json.Marshal(s.st)
	if err != nil {
		return err
	}
	s.lastGood = b
	return nil
}

func decodeState(b []byte) (state, error) {
	st := newState()
	if err := json.Unmarshal(b, &st); err != nil {
		return newState(), err
	}
	if st.Outbox == nil {
		st.Outbox = map[string][]outItem{}
	}
	if st.Records == nil {
		st.Records = map[string]*record{}
	}
	if st.Seen == nil {
		st.Seen = map[string]seenID{}
	}
	st.Inbox = compactItems(st.Inbox)
	return st, nil
}

func compactItems(in []*inItem) []*inItem {
	out := in[:0]
	for _, it := range in {
		if it != nil {
			out = append(out, it)
		}
	}
	return out
}

// commitLocked saves the state, then publishes the record changes made since the last save.
// When the save fails, the state returns to the last saved one, the changes are dropped, and
// the error is returned: no caller answers as if it were on disk.
func (s *Service) commitLocked() error {
	b, err := json.Marshal(s.st)
	if err == nil && s.path() != "" {
		err = s.cfg.write(s.path(), b)
	}
	if err != nil {
		busy := map[string]bool{}
		for _, it := range s.st.Inbox {
			if it.busy {
				busy[it.Message.ID] = true
			}
		}
		if st, derr := decodeState(s.lastGood); derr == nil {
			s.st = st
		}
		for _, it := range s.st.Inbox {
			it.busy = busy[it.Message.ID]
		}
		clear(s.fresh) // the items are gone with the rollback
		s.pending = nil
		s.broadcastLocked()
		return err
	}
	s.lastGood = b
	if s.cfg.Publish != nil {
		for _, r := range s.pending {
			s.cfg.Publish(r)
		}
	}
	s.wakeFreshLocked()
	s.pending = nil
	s.broadcastLocked()
	return nil
}

// writeSynced writes a file through a temporary file that is synced before the rename, then
// syncs the folder so the rename itself is on disk.
func writeSynced(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync() // some file systems refuse a folder sync; the rename is done either way
		d.Close()
	}
	return nil
}
