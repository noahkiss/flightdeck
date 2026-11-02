package devicestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// FileName is the device list's name in the state directory.
const FileName = "devices.json"

// Device states.
const (
	StatePaired  = "paired"
	StateRevoked = "revoked"
)

// Record is one device in the hab's device list.
type Record struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Kind           string `json:"kind"`
	PairedAt       string `json:"paired_at"`
	Key            string `json:"key"`
	AuthorizedKeys string `json:"authorized_keys,omitempty"`
	State          string `json:"state"`
	RevokedAt      string `json:"revoked_at,omitempty"`
}

// PublicKey parses the record's key.
func (r Record) PublicKey() (ssh.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.Key))
	return pub, err
}

type file struct {
	Version int      `json:"version"`
	Devices []Record `json:"devices"`
}

// Store is the device list at <state>/devices.json.
type Store struct {
	Path string
}

// Open names the device list in the state directory dir. It reads nothing yet.
func Open(dir string) *Store { return &Store{Path: filepath.Join(dir, FileName)} }

// List returns the devices, paired and revoked, in the order they were added.
func (s *Store) List() ([]Record, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: version %d, this gff-hab reads version 1", s.Path, f.Version)
	}
	return f.Devices, nil
}

func (s *Store) save(devs []Record) error {
	b, err := json.MarshalIndent(file{Version: 1, Devices: devs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	old, err := readSmall(s.Path)
	if err != nil {
		return err
	}
	return writeAtomic(s.Path, old, append(b, '\n'))
}

// Add stores r, replacing a record with the same id (a device paired again).
func (s *Store) Add(r Record) error {
	devs, err := s.List()
	if err != nil {
		return err
	}
	out := devs[:0:0]
	for _, d := range devs {
		if d.ID != r.ID {
			out = append(out, d)
		}
	}
	return s.save(append(out, r))
}

// Find returns the one paired device that query names: its full id, an id prefix (with or
// without "SHA256:", at least 6 characters), or its exact label.
func (s *Store) Find(query string) (Record, error) {
	devs, err := s.List()
	if err != nil {
		return Record{}, err
	}
	q := strings.TrimPrefix(query, "SHA256:")
	var hits []Record
	for _, d := range devs {
		if d.State != StatePaired {
			continue
		}
		id := strings.TrimPrefix(d.ID, "SHA256:")
		if d.ID == query || (len(q) >= 6 && strings.HasPrefix(id, q)) || d.Label == query {
			hits = append(hits, d)
		}
	}
	switch len(hits) {
	case 0:
		return Record{}, fmt.Errorf("no paired device matches %q", query)
	case 1:
		return hits[0], nil
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID+" ("+h.Label+")")
	}
	return Record{}, fmt.Errorf("%q matches %d devices: %s", query, len(hits), strings.Join(ids, ", "))
}

// Revoke removes the device's line from the authorized_keys file it was written to and keeps the
// record as a revoked tombstone. It returns the record and how many lines it removed.
func (s *Store) Revoke(query string, now time.Time) (Record, int, error) {
	r, err := s.Find(query)
	if err != nil {
		return Record{}, 0, err
	}
	removed := 0
	if r.AuthorizedKeys != "" {
		pub, err := r.PublicKey()
		if err != nil {
			return Record{}, 0, fmt.Errorf("device %s: %w", r.ID, err)
		}
		if removed, err = RemoveAuthorizedKey(r.AuthorizedKeys, pub); err != nil {
			return Record{}, 0, err
		}
	}
	r.State = StateRevoked
	r.RevokedAt = now.UTC().Format(time.RFC3339)
	devs, err := s.List()
	if err != nil {
		return Record{}, 0, err
	}
	for i := range devs {
		if devs[i].ID == r.ID {
			devs[i] = r
		}
	}
	return r, removed, s.save(devs)
}
