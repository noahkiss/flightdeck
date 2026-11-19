package control

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

type fakeDebug struct {
	mu  sync.Mutex
	got []time.Time
}

func (f *fakeDebug) calls() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.got...)
}

func (f *fakeDebug) Set(until time.Time) (api.DebugState, error) {
	f.mu.Lock()
	f.got = append(f.got, until)
	f.mu.Unlock()
	if until.IsZero() {
		return api.DebugState{}, nil
	}
	return api.DebugState{Until: until.UTC().Format(time.RFC3339)}, nil
}

// diag.debug passes a time within 24 hours, turns debug off for an empty or past time, and
// refuses a malformed time or one more than 24 hours ahead (diagnostics.md 4.5).
func TestDiagDebug(t *testing.T) {
	path := socketPath(t)
	fd := &fakeDebug{}
	startHab(t, "", path, func(c *Config) { c.Debug = fd })
	c := dial(t, path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	until := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	res, err := cl.DiagDebug(ctx(t), api.DebugParams{Until: until.Format(time.RFC3339)})
	if err != nil || res.Until != until.Format(time.RFC3339) {
		t.Fatalf("30 minutes: %+v %v", res, err)
	}
	for _, p := range []string{"", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)} {
		if _, err := cl.DiagDebug(ctx(t), api.DebugParams{Until: p}); err != nil {
			t.Fatalf("until %q: %v", p, err)
		}
	}
	if got := fd.calls(); len(got) != 3 || !got[0].Equal(until) || !got[1].IsZero() || !got[2].IsZero() {
		t.Fatalf("Set got %v", got)
	}
	for _, p := range []string{"soon", time.Now().Add(25 * time.Hour).UTC().Format(time.RFC3339)} {
		_, err := cl.DiagDebug(ctx(t), api.DebugParams{Until: p})
		var ae *Error
		if !errors.As(err, &ae) || ae.Data.Kind != api.ErrorKindInvalid {
			t.Fatalf("until %q: %v", p, err)
		}
	}
	if got := fd.calls(); len(got) != 3 {
		t.Fatalf("a refused call reached Set: %v", got)
	}
}

func TestDiagDebugUnavailable(t *testing.T) {
	path := socketPath(t)
	startHab(t, "", path, nil)
	c := dial(t, path)
	hello(t, c, "")
	_, err := (api.Client{Caller: c}).DiagDebug(ctx(t), api.DebugParams{})
	var ae *Error
	if !errors.As(err, &ae) || ae.Data.Kind != api.ErrorKindUnavailable {
		t.Fatalf("diag.debug with no Debug: %v", err)
	}
}
