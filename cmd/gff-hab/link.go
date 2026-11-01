package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/noahkiss/flightdeck/internal/gitstatus"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/service"
)

// newInstanceID returns a random holder instance id, in the holder's own format.
func newInstanceID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DefaultScrollbackMiB is each pane parser's scrollback cap: about 10,000 lines of 120 columns
// (spec 2.3). A cap of zero would leave libghostty's default of about 10 kB.
const DefaultScrollbackMiB = 10

// DefaultScrollbackBudgetMiB caps the scrollback of all pane parsers together: 25 panes at the
// full per-pane cap. Past that each parser gets an equal share, at least 1 MiB (spec 2.3).
const DefaultScrollbackBudgetMiB = 256

// maxScrollbackMiB bounds the cap a flag or the environment may set.
const maxScrollbackMiB = 1 << 20

// scrollbackBytes returns the scrollback cap per pane in bytes: from -scrollback-mib when it is
// set, else $GFF_HAB_SCROLLBACK_MIB, else DefaultScrollbackMiB.
func scrollbackBytes(flagMiB int, getenv func(string) string) (int, error) {
	return mibSetting(flagMiB, "-scrollback-mib", "GFF_HAB_SCROLLBACK_MIB", DefaultScrollbackMiB, getenv)
}

// scrollbackBudget returns the scrollback budget across panes in bytes: from
// -scrollback-budget-mib when it is set, else $GFF_HAB_SCROLLBACK_BUDGET_MIB, else
// DefaultScrollbackBudgetMiB.
func scrollbackBudget(flagMiB int, getenv func(string) string) (int, error) {
	return mibSetting(flagMiB, "-scrollback-budget-mib", "GFF_HAB_SCROLLBACK_BUDGET_MIB", DefaultScrollbackBudgetMiB, getenv)
}

// mibSetting reads a size in MiB from a flag (zero means unset), else the environment variable
// env, else def, and returns it in bytes.
func mibSetting(flagMiB int, flagName, env string, def int, getenv func(string) string) (int, error) {
	mib, from := flagMiB, flagName
	if flagMiB == 0 {
		mib, from = def, ""
		if v := getenv(env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, fmt.Errorf("%s=%q: want a whole number of MiB", env, v)
			}
			mib, from = n, env
		}
	}
	if mib < 1 || mib > maxScrollbackMiB {
		return 0, fmt.Errorf("%s %d: must be 1 to %d MiB", from, mib, maxScrollbackMiB)
	}
	return mib << 20, nil
}

// DefaultRetiredDays is how long gff-hab refuses the uuid of a pane that is gone, across
// restarts (spec 2.3).
const DefaultRetiredDays = 30

// retiredTTL returns how long a retired pane uuid stays refused: from -retired-days when it is
// set, else $GFF_HAB_RETIRED_DAYS, else DefaultRetiredDays.
func retiredTTL(flagDays int, getenv func(string) string) (time.Duration, error) {
	days, from := flagDays, "-retired-days"
	if flagDays == 0 {
		days, from = DefaultRetiredDays, ""
		if v := getenv("GFF_HAB_RETIRED_DAYS"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, fmt.Errorf("GFF_HAB_RETIRED_DAYS=%q: want a whole number of days", v)
			}
			days, from = n, "GFF_HAB_RETIRED_DAYS"
		}
	}
	if days < 1 || days > 3650 {
		return 0, fmt.Errorf("%s %d: must be 1 to 3650 days", from, days)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// gitInterval picks how often each project's repository state is read: -git-interval, else
// $GFF_HAB_GIT_INTERVAL (a Go duration), else gitstatus.DefaultInterval.
func gitInterval(flagValue time.Duration, getenv func(string) string) (time.Duration, error) {
	d, from := flagValue, "-git-interval"
	if d == 0 {
		d, from = gitstatus.DefaultInterval, ""
		if v := getenv("GFF_HAB_GIT_INTERVAL"); v != "" {
			var err error
			if d, err = time.ParseDuration(v); err != nil {
				return 0, fmt.Errorf("GFF_HAB_GIT_INTERVAL=%q: want a duration such as 60s", v)
			}
			from = "GFF_HAB_GIT_INTERVAL"
		}
	}
	if d < time.Second || d > 24*time.Hour {
		return 0, fmt.Errorf("%s %v: must be 1s to 24h", from, d)
	}
	return d, nil
}

// linkOptions are the holder link settings that come from flags or the environment.
type linkOptions struct {
	scrollback int             // scrollback cap per pane, bytes
	budget     int             // scrollback cap for all panes, bytes
	retiredTTL time.Duration   // how long a retired pane uuid stays refused
	starter    service.Starter // starts holders as init system jobs; nil starts none
}

// openLink starts the holder link (spec 2.3). The holder directory comes from -holders, else
// $GFF_HOLDER_DIR, else the directory the holder picks by default. Checkpoints go to
// <state>/checkpoints, with the retired pane uuids. Every pane parser, new or restored, gets
// opt.scrollback bytes of scrollback, or its share of opt.budget when that is less. It returns
// nil, and gff-hab serves with no panes, when no holder directory can be chosen.
func openLink(holders, state string, opt linkOptions, logger *log.Logger) (*link.Link, error) {
	dir := holders
	if dir == "" {
		var err error
		if dir, err = holderreg.DefaultDir(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
			logger.Printf("holder link off: %v", err)
			return nil, nil
		}
	}
	p, name := parser()
	var start func(context.Context) error
	var retired func(string, int)
	if opt.starter != nil {
		start, retired = opt.starter.Start, opt.starter.Retire
	}
	logger.Printf("holder link: holders in %s, parser %s, scrollback %d MiB per pane, %d MiB for all panes",
		dir, name, opt.scrollback>>20, opt.budget>>20)
	return link.Open(link.Config{
		HolderDir:        dir,
		StateDir:         filepath.Join(state, "checkpoints"),
		Build:            build,
		Parser:           p,
		ScrollbackBytes:  opt.scrollback,
		ScrollbackBudget: opt.budget,
		RetiredTTL:       opt.retiredTTL,
		Start:            start,
		Retired:          retired,
		Logf:             logger.Printf,
	})
}
