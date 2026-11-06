package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/heavy"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// TestHeavySettings: the [heavy] keys in range are taken; a value outside its range, of the
// wrong type, an unknown key and a file that does not parse are each logged once and the
// default runs; the example file sets the defaults with nothing logged.
func TestHeavySettings(t *testing.T) {
	d := t.TempDir()
	load := func(body string) (heavySettings, []string) {
		p := filepath.Join(d, "hab.toml")
		os.WriteFile(p, []byte(body), 0o600)
		var logs []string
		return loadHeavySettings(p, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }), logs
	}
	def := heavySettings{RestartHold: heavy.DefaultRestartHold, ResumeMargin: heavy.DefaultResumeMargin}
	if got := loadHeavySettings(filepath.Join(d, "none.toml"), nil); got != def {
		t.Fatalf("no file: %+v", got)
	}
	got, logs := load("[heavy]\nrestart_hold = \"0s\"\ndisk_resume_margin = 0\n")
	if got.RestartHold != 0 || got.ResumeMargin != 0 || len(logs) != 0 {
		t.Fatalf("zeros: %+v %q", got, logs)
	}
	got, _ = load("[heavy]\nrestart_hold = \"45s\"\ndisk_resume_margin = 1.5\n")
	if got.RestartHold != 45*time.Second || got.ResumeMargin != 3*heavy.GiB/2 {
		t.Fatalf("in range: %+v", got)
	}
	for _, body := range []string{
		"[heavy]\nrestart_hold = \"61s\"\n",
		"[heavy]\nrestart_hold = 10\n",
		"[heavy]\nrestart_hold = \"-1s\"\n",
		"[heavy]\ndisk_resume_margin = 101\n",
		"[heavy]\ndisk_resume_margin = \"2\"\n",
		"[heavy]\ndisk_resume_margin = -1\n",
		"[heavy]\nslots = 2\n",
		"[heavy\n",
	} {
		got, logs := load(body)
		if got != def || len(logs) != 1 {
			t.Errorf("%q: %+v, logs %q", body, got, logs)
		}
	}
	var logs2 []string
	got = loadHeavySettings(filepath.Join("..", "..", "docs", "examples", "hab.toml"), func(f string, a ...any) { logs2 = append(logs2, fmt.Sprintf(f, a...)) })
	if got != def || len(logs2) != 0 {
		t.Fatalf("the example: %+v %q", got, logs2)
	}
}

// TestHeavyValues: the lock reads heavy.slots, heavy.disk_min_free and heavy.long_hold from the
// hab-side settings, in their units.
func TestHeavyValues(t *testing.T) {
	vals := map[string]string{settings.HeavySlots: "3", settings.HeavyDiskMinFree: "0", settings.HeavyLongHold: "5"}
	hv := heavyValues{value: func(id string) json.RawMessage { return json.RawMessage(vals[id]) }}
	if hv.slots() != 3 || hv.minFree() != 0 || hv.longHold() != 5*time.Minute {
		t.Fatalf("%d %d %v", hv.slots(), hv.minFree(), hv.longHold())
	}
	for _, d := range settings.Defs {
		if strings.HasPrefix(d.ID, "heavy.") {
			vals[d.ID] = string(d.Default)
		}
	}
	if hv.slots() != heavy.DefaultSlots || hv.minFree() != heavy.DefaultMinFree || hv.longHold() != heavy.DefaultLongHold {
		t.Fatalf("defaults: %d %d %v", hv.slots(), hv.minFree(), hv.longHold())
	}
}
