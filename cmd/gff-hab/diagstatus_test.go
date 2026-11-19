package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/health"
)

// TestStatusLiveness: a marker's pid counts as gff-hab only when that process started before the
// marker did; a pid a later process took after a crash does not (4.8).
func TestStatusLiveness(t *testing.T) {
	t.Parallel()
	me := os.Getpid()
	if _, ok := procStarted(me); !ok {
		t.Fatal("ps gave no start time for this process")
	}
	if !markerAlive(health.Marker{Pid: me, Started: time.Now()}, procStarted) {
		t.Error("a process that started before its marker is not alive")
	}
	if markerAlive(health.Marker{Pid: me, Started: time.Now().Add(-time.Hour)}, procStarted) {
		t.Error("a process that started after the marker counts as gff-hab: the pid was reused")
	}
	if markerAlive(health.Marker{Pid: me, Started: time.Now(), Clean: true}, procStarted) {
		t.Error("a clean marker is alive")
	}
	for in, want := range map[string]time.Duration{"00:05": 5 * time.Second, "01:02:03": time.Hour + 2*time.Minute + 3*time.Second,
		"2-00:00:01": 48*time.Hour + time.Second} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %v %v", in, got, ok)
		}
	}
}

// TestStatusJSONToWriter: gff-hab status -json writes to the writer it was given.
func TestStatusJSONToWriter(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := statusCmd([]string{"-json", "-state", t.TempDir(), "-holders", t.TempDir(), "-socket", "/nonexistent.sock"}, &out); err != nil {
		t.Fatal(err)
	}
	var r statusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Hab.State != "never started" {
		t.Fatalf("status -json wrote %q: %v", out.String(), err)
	}
}
