package push

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// TokensFile is the token store's file in the push directory of the hab's state directory.
const TokensFile = "tokens.json"

// MaxDevices bounds the devices one hab stores. The oldest registration goes first.
const MaxDevices = 64

// Device is one registered device: its APNs device token, the kinds it wants, and the Live
// Activity token of the session it tracks.
type Device struct {
	ID           string    `json:"id"`
	Token        string    `json:"token,omitempty"`
	Environment  string    `json:"environment,omitempty"`
	Kinds        []string  `json:"kinds,omitempty"`
	HideCommands bool      `json:"hide_commands,omitempty"`
	Activity     *Activity `json:"activity,omitempty"`
	Updated      time.Time `json:"updated"`
}

// Activity is a Live Activity push token for one session.
type Activity struct {
	Session string `json:"session"`
	Token   string `json:"token"`
}

// Wants reports whether the device asked for kind.
func (d Device) Wants(kind string) bool { return slices.Contains(d.Kinds, kind) }

// Store keeps the tokens in one owner-only JSON file. Safe for concurrent use.
type Store struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	devices map[string]*Device
}

// OpenStore opens the store in dir, creating dir with mode 0700. A missing file is an empty
// store; an unreadable one is an error.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, TokensFile), now: now, devices: map[string]*Device{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Devices []*Device `json:"devices"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	for _, d := range file.Devices {
		if d != nil && d.ID != "" {
			s.devices[d.ID] = d
		}
	}
	return s, nil
}

// ValidToken reports whether t looks like an APNs token: hex, 8 to 200 bytes.
func ValidToken(t string) bool {
	if len(t) < 16 || len(t) > 400 || len(t)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// Register stores the device token and kinds of a device, replacing its earlier ones. Its Live
// Activity token stays.
func (s *Store) Register(id, token, env string, kinds []string, hide bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(id)
	d.Token, d.Environment, d.Kinds, d.HideCommands = token, env, slices.Clone(kinds), hide
	return s.saveLocked()
}

// RegisterActivity stores the Live Activity token of the session the device tracks, replacing
// any earlier one: a device tracks one session.
func (s *Store) RegisterActivity(id, session, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deviceLocked(id)
	d.Activity = &Activity{Session: session, Token: token}
	return s.saveLocked()
}

// Unregister drops token from the device, as its device token or its activity token. A device
// left with no token is removed. It reports whether anything was dropped.
func (s *Store) Unregister(id, token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[id]
	if d == nil || !s.dropLocked(d, token) {
		return false, nil
	}
	return true, s.saveLocked()
}

// Drop removes token wherever it is stored, after APNs called it invalid. It reports whether
// anything was dropped.
func (s *Store) Drop(token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := false
	for _, d := range s.devices {
		dropped = s.dropLocked(d, token) || dropped
	}
	if !dropped {
		return false, nil
	}
	return true, s.saveLocked()
}

// EndActivity drops the device's Live Activity token once its activity ended.
func (s *Store) EndActivity(id, token string) error {
	_, err := s.Unregister(id, token)
	return err
}

// Devices returns a copy of every device, ordered by id.
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		c := *d
		c.Kinds = slices.Clone(d.Kinds)
		if d.Activity != nil {
			a := *d.Activity
			c.Activity = &a
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) dropLocked(d *Device, token string) bool {
	dropped := false
	if d.Token == token {
		d.Token, d.Environment, d.Kinds = "", "", nil
		dropped = true
	}
	if d.Activity != nil && d.Activity.Token == token {
		d.Activity = nil
		dropped = true
	}
	if d.Token == "" && d.Activity == nil {
		delete(s.devices, d.ID)
	}
	return dropped
}

func (s *Store) deviceLocked(id string) *Device {
	d := s.devices[id]
	if d == nil {
		if len(s.devices) >= MaxDevices {
			var oldest *Device
			for _, o := range s.devices {
				if oldest == nil || o.Updated.Before(oldest.Updated) {
					oldest = o
				}
			}
			delete(s.devices, oldest.ID)
		}
		d = &Device{ID: id}
		s.devices[id] = d
	}
	d.Updated = s.now().UTC()
	return d
}

// saveLocked writes the file whole: a temp file with mode 0600, renamed into place.
func (s *Store) saveLocked() error {
	devs := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].ID < devs[j].ID })
	b, err := json.MarshalIndent(struct {
		Devices []*Device `json:"devices"`
	}{devs}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), TokensFile+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(b); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
