package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// failFrom takes the writes before index from and fails the rest.
type failFrom struct {
	from   int
	writes int
}

func (f *failFrom) QueueInputs(_ context.Context, _ string, data ...[]byte) []func() error {
	waits := make([]func() error, len(data))
	for i := range waits {
		if i < f.from {
			f.writes++
			waits[i] = func() error { return nil }
		} else {
			waits[i] = func() error { return control.Errorf(api.ErrorKindUnavailable, "write %d failed", i) }
		}
	}
	return waits
}

// TestSendDeliveredInPart: a send whose paste was taken but whose Enter failed answers a
// Partial error, so an idempotency key keeps it and a retry pastes nothing (hab-logic-api.md
// 3.9). A send whose first write failed answers a plain error, which a retry may run again.
func TestSendDeliveredInPart(t *testing.T) {
	st, err := structure.Open(structure.Config{Dir: filepath.Join(t.TempDir(), "structure"), Holders: structure.NewFakeHolder(structure.HolderPane{ID: "p"})})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ss, err := st.CreateSession(structure.SessionSpec{Name: "s", Pane: "p", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from    int
		partial bool
	}{{1, true}, {0, false}} {
		in := &failFrom{from: tc.from}
		a := &agentService{store: st, agents: harness.NewStore(harness.Config{}), gate: harness.NewGate(time.Minute), input: in, logf: t.Logf}
		a.agents.Hook("p", claudeEnv("SessionStart", `{"session_id":"a","hook_event_name":"SessionStart"}`, 1))
		err := a.Send(context.Background(), "c", api.SessionSendParams{Session: ss.ID, Text: "hello", Origin: api.SendOriginTyped})
		var ce *control.Error
		if !errors.As(err, &ce) || ce.IsPartial() != tc.partial {
			t.Errorf("writes taken before the failure %d: %v, want partial %v", tc.from, err, tc.partial)
		}
		if in.writes != tc.from {
			t.Errorf("writes taken %d, want %d", in.writes, tc.from)
		}
	}
}
