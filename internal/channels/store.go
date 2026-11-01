package channels

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Files under <state>/channels (mesh.md 6.5): one outbox per target device id, the inbox, and
// the ledger of sent records and received ids. Each is written whole through a synced temporary
// file and a rename. A file that does not parse is set aside as .bad.
const (
	dirName    = "channels"
	inboxFile  = "inbox.json"
	ledgerFile = "ledger.json"
	outPrefix  = "outbox-"
)

type savedOutbox struct {
	Device string    `json:"device"`
	Items  []outItem `json:"items"`
}

type savedInbox struct {
	Items []*inItem `json:"items"`
}

type savedLedger struct {
	Records map[string]*record `json:"records"`
	Seen    map[string]seenID  `json:"seen"`
}

// fileSafe makes a device id usable in a file name.
var fileSafe = strings.NewReplacer("/", "_", "+", "-", ":", "_")

func (s *Service) dir() string {
	if s.cfg.State == "" {
		return ""
	}
	return filepath.Join(s.cfg.State, dirName)
}

func (s *Service) load() error {
	d := s.dir()
	if d == "" {
		return nil
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	var in savedInbox
	if s.read(filepath.Join(d, inboxFile), &in) {
		for _, it := range in.Items {
			if it != nil {
				s.inbox = append(s.inbox, it)
			}
		}
	}
	var lg savedLedger
	if s.read(filepath.Join(d, ledgerFile), &lg) {
		for id, r := range lg.Records {
			if r != nil {
				s.records[id] = r
			}
		}
		for id, v := range lg.Seen {
			s.seen[id] = v
		}
	}
	files, _ := filepath.Glob(filepath.Join(d, outPrefix+"*.json"))
	for _, f := range files {
		var o savedOutbox
		if s.read(f, &o) && o.Device != "" && len(o.Items) > 0 {
			s.outbox[o.Device] = append(s.outbox[o.Device], o.Items...)
		}
	}
	return nil
}

// read decodes a saved file. A missing file is false; one that does not parse is set aside.
func (s *Service) read(p string, v any) bool {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		s.cfg.Logf("channels: %s: %v; starting with it empty", p, err)
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		s.cfg.Logf("channels: %s: %v; set aside as .bad, starting with it empty", p, err)
		os.Rename(p, p+".bad")
		return false
	}
	return true
}

func (s *Service) write(name string, v any) {
	d := s.dir()
	if d == "" {
		return
	}
	b, err := json.Marshal(v)
	if err == nil {
		err = writeSynced(filepath.Join(d, name), b)
	}
	if err != nil {
		s.cfg.Logf("channels: save %s: %v", name, err)
	}
}

func (s *Service) saveInboxLocked() { s.write(inboxFile, savedInbox{Items: s.inbox}) }

func (s *Service) saveLedgerLocked() {
	s.write(ledgerFile, savedLedger{Records: s.records, Seen: s.seen})
}

func (s *Service) saveOutboxLocked(device string) {
	name := outPrefix + fileSafe.Replace(device) + ".json"
	q := s.outbox[device]
	if len(q) == 0 {
		if d := s.dir(); d != "" {
			if err := os.Remove(filepath.Join(d, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.cfg.Logf("channels: remove %s: %v", name, err)
			}
		}
		return
	}
	s.write(name, savedOutbox{Device: device, Items: q})
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
		d.Sync()
		d.Close()
	}
	return nil
}
