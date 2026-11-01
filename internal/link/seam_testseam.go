//go:build testseam

package link

import (
	"os"
	"time"
)

// SpawnDelayEnv names the delay a testseam build sleeps in Spawn between the holder's answer and
// the link following the pane (spawnedSeam). End-to-end tests set it to widen that window, so a
// program that calls back at once lands in it every time.
const SpawnDelayEnv = "GFF_TEST_SPAWN_DELAY"

func init() {
	d, err := time.ParseDuration(os.Getenv(SpawnDelayEnv))
	if err != nil || d <= 0 {
		return
	}
	spawnedSeam = func() { time.Sleep(d) }
}
