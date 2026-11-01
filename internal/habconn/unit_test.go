package habconn

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	common := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	share := func(dir string) []string {
		return []string{"-o", "ControlMaster=auto", "-o", "ControlPath=" + dir + "/%C", "-o", "ControlPersist=60"}
	}
	tail := []string{"--", "box", "gff-hab", "connect"}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	for _, tc := range []struct {
		name string
		tr   SSHTransport
		want []string
	}{
		{"options and dir", SSHTransport{Host: "box", Options: []string{"-p", "2222"}, ControlDir: "/run/x/"},
			slices.Concat([]string{"-p", "2222"}, common, share("/run/x"), tail)},
		{"default dir", SSHTransport{Host: "box"}, slices.Concat(common, share("/run/user/1000/gff/ssh"), tail)},
		{"no sharing", SSHTransport{Host: "box", NoSharing: true, ControlDir: "/run/x"}, slices.Concat(common, tail)},
	} {
		args, err := tc.tr.Args()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(args, tc.want) {
			t.Fatalf("%s: args\n got %q\nwant %q", tc.name, args, tc.want)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "a b"} {
		if _, err := (SSHTransport{Host: bad}).Args(); err == nil {
			t.Fatalf("host %q accepted", bad)
		}
	}
}

// TestDefaultControlDir: under $XDG_RUNTIME_DIR when it is set and short, else under /tmp.
func TestDefaultControlDir(t *testing.T) {
	uid := "/tmp/gff-ssh-" + strconv.Itoa(os.Getuid())
	for _, tc := range []struct{ rt, want string }{
		{"/run/user/1000", "/run/user/1000/gff/ssh"},
		{"", uid},
		{"relative", uid},
		{"/" + strings.Repeat("r", MaxControlDir), uid},
	} {
		t.Setenv("XDG_RUNTIME_DIR", tc.rt)
		if got := DefaultControlDir(); got != tc.want {
			t.Fatalf("XDG_RUNTIME_DIR=%q: %s, want %s", tc.rt, got, tc.want)
		}
	}
}

// TestControlDirChecks: a missing directory is made private; one others may enter, a link, a
// file, a relative path or one too long for socket paths is refused with a clear error.
func TestControlDirChecks(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "gffd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	prep := func(dir string) (string, error) {
		return SSHTransport{Host: "box", ControlDir: dir}.prepareControlDir()
	}

	made := filepath.Join(base, "a", "b")
	if got, err := prep(made + "/"); err != nil || got != made {
		t.Fatalf("missing dir: %q %v", got, err)
	}
	if fi, err := os.Stat(made); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("made dir: %v %v", fi.Mode(), err)
	}
	if _, err := prep(made); err != nil {
		t.Fatalf("existing private dir: %v", err)
	}

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(made, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	long := "/" + strings.Repeat("d", MaxControlDir)
	for _, tc := range []struct{ dir, want string }{
		{open, "has mode 0750"},
		{link, "is not a directory"},
		{file, "not a directory"},
		{"rel/dir", "not an absolute path"},
		{long, "104-byte limit"},
	} {
		if _, err := prep(tc.dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("dir %s: %v, want an error with %q", tc.dir, err, tc.want)
		}
	}
	// The longest allowed directory gives a master's temporary socket path of 103 bytes.
	fits := "/" + strings.Repeat("d", MaxControlDir-1)
	if got := len(fits + "/" + strings.Repeat("c", 40) + "." + strings.Repeat("t", 16)); got != 103 {
		t.Fatalf("longest socket path %d bytes", got)
	}
	if _, err := (SSHTransport{Host: "box", ControlDir: fits}).Args(); err != nil {
		t.Fatalf("dir of %d bytes: %v", len(fits), err)
	}
	// Dial checks the directory before it starts ssh.
	if _, err := (SSHTransport{Host: "box", ControlDir: open, SSH: "/nonexistent"}).Dial(t.Context(), KindControl); err == nil || !strings.Contains(err.Error(), "has mode") {
		t.Fatalf("dial with an open dir: %v", err)
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
