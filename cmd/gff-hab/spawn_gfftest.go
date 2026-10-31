//go:build gfftest

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// testSpawn is one entry of $GFF_HAB_TEST_SPAWN, a JSON array.
type testSpawn struct {
	UUID string   `json:"uuid"` // 32 hex digits
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
}

// In the gfftest build, gff-hab serve spawns each pane listed in $GFF_HAB_TEST_SPAWN at start.
func init() {
	startupSpawns = func(s *spawner, logf func(string, ...any)) {
		v := os.Getenv("GFF_HAB_TEST_SPAWN")
		if v == "" {
			return
		}
		var list []testSpawn
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			logf("test spawn: %v", err)
			return
		}
		for _, ts := range list {
			var u hp.UUID
			b, err := hex.DecodeString(ts.UUID)
			if err != nil || len(b) != len(u) {
				logf("test spawn: bad uuid %q", ts.UUID)
				continue
			}
			copy(u[:], b)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err = s.spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/", Argv: ts.Argv, Env: ts.Env})
			cancel()
			if err != nil {
				logf("test spawn %s: %v", u, err)
				continue
			}
			logf("test spawn: pane %s started", u)
		}
	}
}
