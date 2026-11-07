// Package personal is Capcom's personal tools on the home hab (docs/specs/capcom-service.md 8):
// the device context the phone sends (8.1, 8.2), weather from WeatherKit (8.3), places from
// Apple Maps and, with the user's own key, Google (8.4). Pure Go: the network calls go through
// an http.Client the caller gives, so tests use a fake server and never the real services.
package personal

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// DeviceFile is the one device-context file in the Capcom folder: the latest time zone only
// (8.1). Location and calendar are never written.
const DeviceFile = "device.json"

// Reasons a read finds no location or calendar (8.2).
const (
	ReasonOff          = "off"
	ReasonNoPermission = "no_permission"
	ReasonNothingYet   = "nothing since the hab started"
)

// Fix is the latest location fix and the device that sent it.
type Fix struct {
	api.DeviceLocation
	Device string
	At     time.Time // the fix's own time, parsed
}

// Calendar is the latest calendar snapshot and the device that sent it.
type Calendar struct {
	api.DeviceCalendar
	Device string
	At     time.Time // when the phone read the calendars
}

// TZ is the latest time zone and the device that sent it.
type TZ struct {
	Zone   string    `json:"tz"`
	Device string    `json:"device,omitempty"`
	Time   time.Time `json:"time"`
}

// Store holds the device context: the latest value of each field, in memory only, except the
// time zone, which it keeps in DeviceFile so quiet hours survive a restart (8.1, E11). Safe for
// concurrent use.
type Store struct {
	path string // DeviceFile's path; "" keeps the zone in memory only
	now  func() time.Time

	mu     sync.Mutex
	fix    *Fix
	locOff map[string]string // device -> why it shares none
	cal    *Calendar
	calOff map[string]string
	tz     TZ
}

// OpenStore returns the store for the Capcom folder dir, with the time zone DeviceFile holds.
// A file that does not parse is ignored: the next zone replaces it. now may be nil.
func OpenStore(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{now: now, locOff: map[string]string{}, calOff: map[string]string{}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, DeviceFile)
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var tz TZ
		if json.Unmarshal(b, &tz) == nil && validZone(tz.Zone) {
			s.tz = tz
		}
	}
	return s, nil
}

func validZone(z string) bool {
	if z == "" {
		return false
	}
	_, err := time.LoadLocation(z)
	return err == nil
}

// ErrBadZone reports a time zone the hab does not know.
var ErrBadZone = errors.New("not an IANA time zone this hab knows")

// Put takes one device's context. The newest fix wins, whichever device sent it; a device that
// says it shares none drops the fix it sent. The calendar works the same way. A new time zone
// is saved before Put returns.
func (s *Store) Put(device string, p api.DeviceContextPutParams) error {
	var loc *Fix
	if p.Location != nil {
		t, err := time.Parse(time.RFC3339Nano, p.Location.Time)
		if err != nil {
			return errors.New("location.time is not RFC 3339")
		}
		if !(p.Location.Lat >= -90 && p.Location.Lat <= 90 && p.Location.Lon >= -180 && p.Location.Lon <= 180) {
			return errors.New("location: lat or lon out of range")
		}
		loc = &Fix{DeviceLocation: *p.Location, Device: device, At: t}
	}
	var cal *Calendar
	if p.Calendar != nil {
		t, err := time.Parse(time.RFC3339Nano, p.Calendar.Time)
		if err != nil {
			return errors.New("calendar.time is not RFC 3339")
		}
		c := *p.Calendar
		c.Events = append([]api.DeviceEvent{}, c.Events...)
		cal = &Calendar{DeviceCalendar: c, Device: device, At: t}
	}
	if p.TZ != "" && !validZone(p.TZ) {
		return ErrBadZone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.TZ != "" && p.TZ != s.tz.Zone {
		tz := TZ{Zone: p.TZ, Device: device, Time: s.now().UTC()}
		if err := s.saveZone(tz); err != nil {
			return err
		}
		s.tz = tz
	}
	if loc != nil {
		delete(s.locOff, device)
		if s.fix == nil || !loc.At.Before(s.fix.At) {
			s.fix = loc
		}
	} else if p.LocationOff != "" {
		s.locOff[device] = p.LocationOff
		if s.fix != nil && s.fix.Device == device {
			s.fix = nil
		}
	}
	if cal != nil {
		delete(s.calOff, device)
		if s.cal == nil || !cal.At.Before(s.cal.At) {
			s.cal = cal
		}
	} else if p.CalendarOff != "" {
		s.calOff[device] = p.CalendarOff
		if s.cal != nil && s.cal.Device == device {
			s.cal = nil
		}
	}
	return nil
}

// saveZone writes DeviceFile through a temporary file and a rename. The caller holds s.mu.
func (s *Store) saveZone(tz TZ) error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(tz)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Location returns the latest fix, or why there is none: a reason a device sent (off,
// no_permission), else ReasonNothingYet.
func (s *Store) Location() (Fix, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fix != nil {
		return *s.fix, ""
	}
	return Fix{}, reason(s.locOff)
}

// Calendar returns the latest snapshot, or why there is none.
func (s *Store) Calendar() (Calendar, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cal != nil {
		c := *s.cal
		c.Events = append([]api.DeviceEvent{}, c.Events...)
		return c, ""
	}
	return Calendar{}, reason(s.calOff)
}

// Zone returns the latest time zone; "" when no device has sent one.
func (s *Store) Zone() TZ {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tz
}

func reason(off map[string]string) string {
	for _, r := range []string{ReasonOff, ReasonNoPermission} {
		for _, v := range off {
			if v == r {
				return r
			}
		}
	}
	for _, v := range off {
		return v
	}
	return ReasonNothingYet
}
