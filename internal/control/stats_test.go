package control

import (
	"github.com/noahkiss/flightdeck/api"
	"testing"
)

func TestStatsUnavailable(t *testing.T) {
	path := socketPath(t)
	startHab(t, "", path, nil)
	c := dial(t, path)
	if _, err := c.Hello(ctx(t), api.ClientInfo{Name: "app", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := (api.Client{Caller: c}).HabStats(ctx(t), api.HabStatsParams{}); err == nil {
		t.Fatal("stats answered without a reader")
	}
}
