package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/heavy"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// heavySettings is the [heavy] section of the hab's config file (groups.md 15.3).
type heavySettings struct {
	RestartHold  time.Duration // [heavy] restart_hold, 0 to 60 s
	ResumeMargin uint64        // [heavy] disk_resume_margin, 0 to 100 GiB, in bytes
}

// loadHeavySettings reads [heavy] from the hab's config file: restart_hold, a duration such as
// "10s" from 0 to 60 s, and disk_resume_margin, a number of GiB from 0 to 100. A value outside
// its range, or of the wrong type, is logged once and the default runs; so does a file that
// does not parse. A missing file or section takes the defaults.
func loadHeavySettings(path string, logf func(string, ...any)) heavySettings {
	out := heavySettings{RestartHold: heavy.DefaultRestartHold, ResumeMargin: heavy.DefaultResumeMargin}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		logf("heavy: %v; the [heavy] defaults run", err)
		return out
	}
	var fc struct {
		Heavy map[string]any `toml:"heavy"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		logf("heavy: %s does not parse (%v); the [heavy] defaults run", path, err)
		return out
	}
	for k, v := range fc.Heavy {
		switch k {
		case "restart_hold":
			s, ok := v.(string)
			d, err := time.ParseDuration(s)
			if !ok || err != nil || d < 0 || d > time.Minute {
				logf("heavy: %s: [heavy] restart_hold %v is not a duration from \"0s\" to \"60s\"; %s runs", path, v, heavy.DefaultRestartHold)
				continue
			}
			out.RestartHold = d
		case "disk_resume_margin":
			var g float64
			switch n := v.(type) {
			case int64:
				g = float64(n)
			case float64:
				g = n
			default:
				g = -1
			}
			if g < 0 || g > 100 || math.IsNaN(g) {
				logf("heavy: %s: [heavy] disk_resume_margin %v is not a number of GiB from 0 to 100; 2 runs", path, v)
				continue
			}
			out.ResumeMargin = uint64(g * float64(heavy.GiB))
		default:
			logf("heavy: %s: [heavy] has no key %q; it is left out", path, k)
		}
	}
	return out
}

// heavyValues reads the hab-side settings the lock uses at each use, so a change applies at
// once.
type heavyValues struct {
	value func(id string) json.RawMessage
}

func (h heavyValues) int(id string, def int64) int64 {
	n, err := strconv.ParseInt(string(h.value(id)), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func (h heavyValues) slots() int { return int(h.int(settings.HeavySlots, heavy.DefaultSlots)) }

func (h heavyValues) minFree() uint64 {
	return uint64(h.int(settings.HeavyDiskMinFree, int64(heavy.DefaultMinFree/heavy.GiB))) * heavy.GiB
}

func (h heavyValues) longHold() time.Duration {
	return time.Duration(h.int(settings.HeavyLongHold, int64(heavy.DefaultLongHold/time.Minute))) * time.Minute
}

// heavyMain is `gff-hab heavy`, the same command as `gff heavy`.
func heavyMain(args []string) int {
	return heavy.Main(heavy.Env{Args: args, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Client: cliClient, Name: "gff-hab heavy"})
}
