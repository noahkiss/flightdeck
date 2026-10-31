package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/link"
)

// DefaultScrollbackMiB is each pane parser's scrollback cap: about 10,000 lines of 120 columns
// (spec 2.3). A cap of zero would leave libghostty's default of about 10 kB.
const DefaultScrollbackMiB = 10

// maxScrollbackMiB bounds the cap a flag or the environment may set.
const maxScrollbackMiB = 1 << 20

// scrollbackBytes returns the scrollback cap per pane in bytes: from -scrollback-mib when it is
// set, else $GFF_HAB_SCROLLBACK_MIB, else DefaultScrollbackMiB.
func scrollbackBytes(flagMiB int, getenv func(string) string) (int, error) {
	mib, from := flagMiB, "-scrollback-mib"
	if flagMiB == 0 {
		mib, from = DefaultScrollbackMiB, ""
		if v := getenv("GFF_HAB_SCROLLBACK_MIB"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, fmt.Errorf("GFF_HAB_SCROLLBACK_MIB=%q: want a whole number of MiB", v)
			}
			mib, from = n, "GFF_HAB_SCROLLBACK_MIB"
		}
	}
	if mib < 1 || mib > maxScrollbackMiB {
		return 0, fmt.Errorf("%s %d: the scrollback cap must be 1 to %d MiB", from, mib, maxScrollbackMiB)
	}
	return mib << 20, nil
}

// openLink starts the holder link (spec 2.3). The holder directory comes from -holders, else
// $GFF_HOLDER_DIR, else the directory the holder picks by default. Checkpoints go to
// <state>/checkpoints. Every pane parser, new or restored, gets scrollback bytes of scrollback.
// It returns nil, and gff-hab serves with no panes, when no holder directory can be chosen.
func openLink(holders, state string, scrollback int, logger *log.Logger) (*link.Link, error) {
	dir := holders
	if dir == "" {
		var err error
		if dir, err = holderreg.DefaultDir(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
			logger.Printf("holder link off: %v", err)
			return nil, nil
		}
	}
	p, name := parser()
	logger.Printf("holder link: holders in %s, parser %s, scrollback %d MiB per pane", dir, name, scrollback>>20)
	return link.Open(link.Config{
		HolderDir:       dir,
		StateDir:        filepath.Join(state, "checkpoints"),
		Build:           build,
		Parser:          p,
		ScrollbackBytes: scrollback,
		Logf:            logger.Printf,
	})
}
