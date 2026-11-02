package rounds

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LoadZone loads an IANA zone by name. "Local" and the empty name are refused: a round stores
// a real zone name, so it keeps the time the user meant on a laptop that travels (rounds.md 4.1).
func LoadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("zone %q: want an IANA zone name such as Europe/Paris", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("zone %q: %v", name, err)
	}
	return loc, nil
}

// HostZone returns this host's zone as an IANA name: $TZ when it names a zone, else the target
// of /etc/localtime (Linux and macOS both link it into a zoneinfo tree), else "UTC".
func HostZone() string {
	return hostZone(os.Getenv("TZ"), func() (string, error) { return os.Readlink("/etc/localtime") })
}

func hostZone(tz string, readlink func() (string, error)) string {
	if name, ok := zoneName(strings.TrimPrefix(tz, ":")); ok {
		return name
	}
	if target, err := readlink(); err == nil {
		if name, ok := zoneName(target); ok {
			return name
		}
	}
	return "UTC"
}

// zoneName turns a zone name or a path into a zoneinfo tree into an IANA name that loads. A
// path outside a zoneinfo tree, absolute or relative ("./UTC", which the time package would
// load), is not a zone name.
func zoneName(s string) (string, bool) {
	if i := strings.LastIndex(s, "zoneinfo/"); i >= 0 {
		s = s[i+len("zoneinfo/"):]
	}
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		return "", false
	}
	if _, err := LoadZone(s); err != nil {
		return "", false
	}
	return s, true
}
