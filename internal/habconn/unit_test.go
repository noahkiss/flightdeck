package habconn

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

func ev(t *testing.T, seq uint64, typ api.EventType, id string, rev uint64, data any) control.Notification {
	t.Helper()
	b, _ := json.Marshal(data)
	e := api.Event{Seq: seq, Type: typ, ID: id, Data: b}
	if rev > 0 {
		e.Revision = &rev
	}
	p, _ := json.Marshal(e)
	return control.Notification{Method: "event", Params: p}
}

func testConn() *Conn {
	return &Conn{updates: newQueue[Update](), state: newState(), panes: map[*PaneStream]struct{}{}}
}

// The seq rule and the revision rule (spec 3.5): events at or below the response's seq are in
// the snapshot; an event whose revision is not higher than the one held is ignored; a gap in
// seq ends the connection.
func TestMergeRules(t *testing.T) {
	c := testConn()
	c.state.reset(api.Structure{Sessions: []api.Session{{ID: "s", Revision: 5, Name: "five"}}})
	s := &session{floor: 2}
	steps := []struct {
		n    control.Notification
		name string // the session's name after the step
	}{
		{ev(t, 1, api.EventSessionUpdated, "s", 4, api.Session{ID: "s", Revision: 4, Name: "four"}), "five"},  // at or below the floor
		{ev(t, 2, api.EventSessionUpdated, "s", 9, api.Session{ID: "s", Revision: 9, Name: "nine?"}), "five"}, // at the floor
		{ev(t, 3, api.EventSessionUpdated, "s", 5, api.Session{ID: "s", Revision: 5, Name: "again"}), "five"}, // revision not higher
		{ev(t, 4, api.EventSessionUpdated, "s", 6, api.Session{ID: "s", Revision: 6, Name: "six"}), "six"},
		{ev(t, 5, api.EventSessionDeleted, "s", 7, api.Deleted{}), ""},
		{ev(t, 6, api.EventSessionUpdated, "s", 7, api.Session{ID: "s", Revision: 7, Name: "late"}), ""}, // after the delete
	}
	for i, st := range steps {
		if err := c.note(s, st.n); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := c.state.sessions["s"].Name; got != st.name {
			t.Fatalf("step %d: name %q, want %q", i, got, st.name)
		}
	}
	if err := c.note(s, control.Notification{Method: "heartbeat", Params: json.RawMessage(`{"seq":6}`)}); err != nil {
		t.Fatalf("heartbeat at the last seq: %v", err)
	}
	if err := c.note(s, control.Notification{Method: "heartbeat", Params: json.RawMessage(`{"seq":7}`)}); !errors.Is(err, errGap) {
		t.Fatalf("heartbeat past the last event: %v", err)
	}
	if err := c.note(s, ev(t, 8, api.EventPaneActivity, "p", 0, struct{}{})); !errors.Is(err, errGap) {
		t.Fatalf("event 8 after 6: %v", err)
	}
}

// A new epoch's snapshot replaces every object, whatever revision the client held.
func TestResetReplaces(t *testing.T) {
	st := newState()
	st.reset(api.Structure{Panes: []api.Pane{{ID: "p", Revision: 40}}, Sessions: []api.Session{{ID: "gone", Revision: 3}}})
	st.reset(api.Structure{Panes: []api.Pane{{ID: "p", Revision: 1, Cols: 100}}})
	if p := st.panes["p"]; p.Revision != 1 || p.Cols != 100 {
		t.Fatalf("pane after reset: %+v", p)
	}
	if _, ok := st.sessions["gone"]; ok {
		t.Fatal("a session absent from the snapshot survived")
	}
	rev := uint64(2)
	if !st.apply(api.Event{Type: api.EventPaneUpdated, ID: "p", Revision: &rev, Data: json.RawMessage(`{"id":"p","revision":2}`)}) {
		t.Fatal("an event above the new, lower revision was ignored")
	}
}

func TestSSHArgs(t *testing.T) {
	args, err := SSHTransport{Host: "box", Options: []string{"-p", "2222"}, ControlDir: "/run/x/"}.Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "2222", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto", "-o", "ControlPath=/run/x/%C", "-o", "ControlPersist=60",
		"--", "box", "gff-hab", "connect"}
	if !slices.Equal(args, want) {
		t.Fatalf("args\n got %q\nwant %q", args, want)
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "a b"} {
		if _, err := (SSHTransport{Host: bad}).Args(); err == nil {
			t.Fatalf("host %q accepted", bad)
		}
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := 100*time.Millisecond, time.Second
	for attempt := 1; attempt < 12; attempt++ {
		want := min(lo<<(attempt-1), hi)
		for range 50 {
			if d := backoff(lo, hi, attempt); d < want/2 || d > want {
				t.Fatalf("attempt %d: %v outside [%v, %v]", attempt, d, want/2, want)
			}
		}
	}
}
