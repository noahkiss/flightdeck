package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/noahkiss/flightdeck/internal/pairing"
)

// pairSettings is the [pairing] section of the hab's config file (transport.md 6.1.7): whether
// devices.pair may open a pairing window, and where.
type pairSettings struct {
	API            bool         // devices.pair and devices.confirm work; off by default
	Listen         []netip.Addr // the window's addresses; none: chosen as 6.1.3 says
	SSHPort        int          // the system sshd's port, told to the device; 0: 22
	SSHDKeys       string       // folder with the system sshd's host keys; "": /etc/ssh
	AuthorizedKeys string       // "": ~/.ssh/authorized_keys
}

// loadPairSettings reads the [pairing] section. A missing file or section gives the defaults:
// pairing over the API off.
func loadPairSettings(path string) (pairSettings, error) {
	var s pairSettings
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var fc struct {
		Pairing struct {
			API            bool     `toml:"api"`
			Listen         []string `toml:"listen"`
			SSHPort        int      `toml:"ssh_port"`
			SSHDKeys       string   `toml:"sshd_keys"`
			AuthorizedKeys string   `toml:"authorized_keys"`
		} `toml:"pairing"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	c := fc.Pairing
	if c.SSHPort < 0 || c.SSHPort > 65535 {
		return s, fmt.Errorf("%s: pairing: ssh_port %d is not a port", path, c.SSHPort)
	}
	if len(c.Listen) > pairing.MaxEndpoints {
		return s, fmt.Errorf("%s: pairing: listen names %d addresses; at most %d", path, len(c.Listen), pairing.MaxEndpoints)
	}
	for _, l := range c.Listen {
		a, err := netip.ParseAddr(l)
		if err != nil {
			return s, fmt.Errorf("%s: pairing: listen %q: %v", path, l, err)
		}
		if err := pairing.CheckListen(a); err != nil {
			return s, fmt.Errorf("%s: pairing: listen %s: %v", path, l, err)
		}
		s.Listen = append(s.Listen, a.Unmap())
	}
	keys, err := expandHome(c.AuthorizedKeys)
	if err != nil {
		return s, fmt.Errorf("%s: pairing: authorized_keys: %v", path, err)
	}
	if c.API {
		// The file turns pairing on and picks where the window listens and where keys land: as
		// stateTooOpen does for the state folder, refuse it when group or others can write it.
		fi, err := os.Stat(path)
		if err != nil {
			return s, err
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return s, fmt.Errorf("%s: group or others can write it (mode %04o), so pairing over the API stays off; run chmod go-w %s", path, fi.Mode().Perm(), path)
		}
	}
	s.API, s.SSHPort, s.SSHDKeys, s.AuthorizedKeys = c.API, c.SSHPort, c.SSHDKeys, keys
	return s, nil
}

// expandHome makes a configured path absolute: "~/" and "~" name the home folder; any other
// relative path is an error, since the hab's working folder is no place the owner chose ("/"
// under launchd). "" stays "", the default.
func expandHome(p string) (string, error) {
	switch {
	case p == "" || filepath.IsAbs(p):
		return p, nil
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return "", fmt.Errorf("%q is a relative path; name it from / or from ~/", p)
}
