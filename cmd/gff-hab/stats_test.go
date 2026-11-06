package main

import (
	"context"
	"github.com/noahkiss/flightdeck/api"
	"slices"
	"testing"
	"time"
)

// TestHabStats asks the real hab for its resources without a holder or client-side reader.
func TestHabStats(t *testing.T) {
	e := newHabEnv(t, 0)
	if w := welcome(t, e.sock); !slices.Contains(w.Capabilities, "host-stats") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(5*time.Second))
	defer cancel()
	var got, again api.HostStats
	if err := c.Call(ctx, "hab.stats", api.HabStatsParams{}, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, got.At); err != nil {
		t.Fatal(err)
	}
	if got.MemTotal == 0 || got.MemUsed > got.MemTotal || got.DiskTotal == 0 || got.DiskUsed > got.DiskTotal || got.UptimeS == 0 {
		t.Fatalf("stats %+v", got)
	}
	if err := c.Call(ctx, "hab.stats", api.HabStatsParams{}, &again); err != nil {
		t.Fatal(err)
	}
	if again.At != got.At {
		t.Fatalf("readings not cached: %q, %q", got.At, again.At)
	}
}
