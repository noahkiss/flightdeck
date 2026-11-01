package speech

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// HelperName is the speech helper's program name.
const HelperName = "gff-hab-speech"

// MinMemory is the host memory at which the engine is on by default: a host sold as 4 GB.
// Linux reports a little less than the installed memory as MemTotal, so the line sits below
// 4 GiB.
const MinMemory = 3584 << 20

// Settings is the [speech] section of the hab's config file.
type Settings struct {
	Enabled  *bool  // nil: on when the host has at least MinMemory
	Helper   string // the helper program; empty: look for it
	ModelURL string // a mirror of the model archive; the checksum is still checked
	Threads  int    // decode threads; 0: up to 4
}

type fileConfig struct {
	Speech struct {
		Enabled  *bool  `toml:"enabled"`
		Helper   string `toml:"helper"`
		ModelURL string `toml:"model_url"`
		Threads  int    `toml:"threads"`
	} `toml:"speech"`
}

// LoadSettings reads the [speech] section of the hab's config file. A missing file or section
// gives the defaults.
func LoadSettings(path string) (Settings, error) {
	var s Settings
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
	var fc fileConfig
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	c := fc.Speech
	if c.Threads < 0 {
		return s, fmt.Errorf("%s: speech: threads must not be negative", path)
	}
	return Settings{Enabled: c.Enabled, Helper: c.Helper, ModelURL: c.ModelURL, Threads: c.Threads}, nil
}

// On reports whether the engine is turned on for a host with mem bytes of memory (0: unknown,
// which counts as enough), and why, for the log.
func (s Settings) On(mem uint64) (bool, string) {
	if s.Enabled != nil {
		if *s.Enabled {
			return true, "on in the config file"
		}
		return false, "off in the config file"
	}
	if mem != 0 && mem < MinMemory {
		return false, fmt.Sprintf("off: the host has %d MiB of memory, under %d MiB", mem>>20, uint64(MinMemory)>>20)
	}
	return true, "on by default"
}

// ThreadCount returns the decode threads: the setting, else the CPU count up to 4.
func (s Settings) ThreadCount() int {
	if s.Threads > 0 {
		return s.Threads
	}
	return min(runtime.NumCPU(), 4)
}

// FindHelper returns the helper program: the setting, else $GFF_HAB_SPEECH_HELPER, else
// gff-hab-speech beside the running gff-hab, else on PATH. It returns "" when there is none.
func FindHelper(s Settings, getenv func(string) string, executable func() (string, error)) string {
	for _, p := range []string{s.Helper, getenv("GFF_HAB_SPEECH_HELPER")} {
		if p != "" {
			if isExec(p) {
				return p
			}
			return ""
		}
	}
	if exe, err := executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), HelperName); isExec(p) {
			return p
		}
	}
	if p, err := exec.LookPath(HelperName); err == nil {
		return p
	}
	return ""
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}
