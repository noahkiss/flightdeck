package merge

import (
	"errors"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// TestEventStatusFromStream: each event's Status follows the updates, not the model, which may
// run ahead. Here the Set already shows the host online while the merger reads the updates
// before that: the offline event must still say offline (merge review, note on Event.Status).
func TestEventStatusFromStream(t *testing.T) {
	m := &Merger{events: newQueue[Event](), connected: map[string]bool{}}
	defer m.events.stop()
	ahead := func() []hosts.Host { return []hosts.Host{{Label: "one", Status: hosts.Online}} }
	drop := errors.New("dropped")
	up := func(k habconn.UpdateKind, err error) hosts.Update {
		return hosts.Update{Host: "one", Update: habconn.Update{Kind: k, Err: err}}
	}
	steps := []struct {
		u    hosts.Update
		want hosts.Status
	}{
		{up(habconn.UpdateRetrying, drop), hosts.Connecting},
		{up(habconn.UpdateOnline, nil), hosts.Online},
		{up(habconn.UpdateOffline, drop), hosts.Offline},
		{up(habconn.UpdateRetrying, drop), hosts.Offline},
		{up(habconn.UpdateOnline, nil), hosts.Online},
		{up(habconn.UpdateOffline, hosts.ErrSpecChanged), hosts.Connecting},
		{up(habconn.UpdateRetrying, drop), hosts.Connecting},
		{up(habconn.UpdateFailed, drop), hosts.Failed},
		{hosts.Update{Host: "one", Removed: true}, 0},
	}
	for i, st := range steps {
		m.apply(st.u, ahead)
		select {
		case ev := <-m.events.out:
			if ev.Seq != uint64(i+1) || ev.Status != st.want || ev.Err != st.u.Err {
				t.Fatalf("step %d (%v): seq %d, status %v, err %v; want %d, %v, %v",
					i, ev.Kind, ev.Seq, ev.Status, ev.Err, i+1, st.want, st.u.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("step %d: no event", i)
		}
	}
}
