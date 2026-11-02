package control

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultState picks gff-hab's state directory: $GFF_HAB_STATE, else gff/hab under the user
// config directory on macOS and under $XDG_STATE_HOME (or ~/.local/state) elsewhere. gff-hab
// keeps its files there, and the Logbook CLI finds logbook.db there when gff-hab is down
// (logbook.md 4.3).
func DefaultState() (string, error) {
	if d := os.Getenv("GFF_HAB_STATE"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "darwin" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "gff", "hab"), nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gff", "hab"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "gff", "hab"), nil
}
