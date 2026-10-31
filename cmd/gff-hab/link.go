package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/link"
)

// openLink starts the holder link (spec 2.3). The holder directory comes from -holders, else
// $GFF_HOLDER_DIR, else the directory the holder picks by default. Checkpoints go to
// <state>/checkpoints. It returns nil, and gff-hab serves with no panes, when no holder
// directory can be chosen.
func openLink(holders, state string, logger *log.Logger) (*link.Link, error) {
	dir := holders
	if dir == "" {
		var err error
		if dir, err = holderreg.DefaultDir(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
			logger.Printf("holder link off: %v", err)
			return nil, nil
		}
	}
	p, name := parser()
	logger.Printf("holder link: holders in %s, parser %s", dir, name)
	return link.Open(link.Config{
		HolderDir: dir,
		StateDir:  filepath.Join(state, "checkpoints"),
		Build:     build,
		Parser:    p,
		Logf:      logger.Printf,
	})
}
