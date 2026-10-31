package holderreg

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// shortDir returns a fresh directory with a short path (macOS caps socket paths at 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// deadPid returns the pid of a process that has exited and been reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// listen opens a Unix socket at path. With keep, closing it leaves the socket file behind, as
// a crashed process does.
func listen(t *testing.T, path string, keep bool) *net.UnixListener {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(!keep)
	t.Cleanup(func() { l.Close() })
	return l
}

func entry(dir, id string, pid int) Entry {
	return Entry{Instance: id, Build: "b-" + id, Protocol: Protocol{1, 2}, Pid: pid,
		Socket: SocketPath(dir, id), Started: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), Draining: true}
}

func TestWriteReadList(t *testing.T) {
	dir := shortDir(t)
	a := entry(dir, "aaa", os.Getpid())
	b := entry(dir, "bbb", os.Getpid())
	b.Started = a.Started.Add(-time.Second) // older, so it lists first
	for _, e := range []Entry{a, b} {
		if err := Write(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := List(dir)
	if len(errs) != 0 || !reflect.DeepEqual(got, []Entry{b, a}) {
		t.Fatalf("list: %+v %v", got, errs)
	}
	// Rewriting replaces the file in place; no temporary file is left.
	a.Draining = false
	if err := Write(dir, a); err != nil {
		t.Fatal(err)
	}
	if e, err := Read(Path(dir, "aaa")); err != nil || e.Draining {
		t.Fatalf("after rewrite: %+v %v", e, err)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 2 {
		t.Fatalf("files: %v", des)
	}
	if err := Write(dir, Entry{Instance: "../x"}); err == nil {
		t.Fatal("wrote a bad instance id")
	}
}

func TestListSkipsJunk(t *testing.T) {
	dir := shortDir(t)
	if err := Write(dir, entry(dir, "good", 1)); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"broken.json":    "{",
		"renamed.json":   `{"instance":"other"}`,
		".tmp-x-1":       "{",
		"notes.txt":      "x",
		"good.sock.json": "", // empty
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := List(dir)
	if len(got) != 1 || got[0].Instance != "good" || len(errs) != 3 {
		t.Fatalf("list: %+v %v", got, errs)
	}
	if got, errs := List(filepath.Join(dir, "missing")); got != nil || errs != nil {
		t.Fatalf("missing dir: %v %v", got, errs)
	}
}

// TestRemoveOnlyOwn: Remove leaves a file a newer holder of the same instance wrote since.
func TestRemoveOnlyOwn(t *testing.T) {
	dir := shortDir(t)
	old := entry(dir, "inst", 100)
	newer := old
	newer.Pid, newer.Started = 200, old.Started.Add(time.Minute)
	if err := Write(dir, newer); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(Path(dir, "inst")); err != nil {
		t.Fatalf("newer file removed: %v", err)
	}
	if err := Remove(dir, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(dir, "inst")); !os.IsNotExist(err) {
		t.Fatalf("own file kept: %v", err)
	}
	if err := Remove(dir, newer); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// TestStaleAndPrune covers the stale rule: a dead pid, or a live pid whose socket refuses
// (file present with no listener, or file gone). A live holder and fresh temporary files stay.
func TestStaleAndPrune(t *testing.T) {
	dir := shortDir(t)
	me := os.Getpid()

	live := entry(dir, "live", me)
	listen(t, live.Socket, false)

	deadPidE := entry(dir, "deadpid", deadPid(t))
	listen(t, deadPidE.Socket, true).Close() // socket file left, as after a crash

	refused := entry(dir, "refused", me) // pid reused by a live process; socket left, nobody listens
	listen(t, refused.Socket, true).Close()

	nosock := entry(dir, "nosock", me) // socket file gone
	elsewhere := entry(dir, "elsewhere", deadPid(t))
	elsewhere.Socket = filepath.Join(t.TempDir(), "x.sock") // not in dir: never removed

	for _, e := range []Entry{live, deadPidE, refused, nosock, elsewhere} {
		if err := Write(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		e           Entry
		probe, want bool
	}{
		{live, true, false},
		{deadPidE, false, true},
		{refused, false, false}, // without a probe only the pid counts
		{refused, true, true},
		{nosock, true, true},
	} {
		if got, why := Stale(tc.e, tc.probe); got != tc.want {
			t.Errorf("%s probe=%v: stale=%v (%s)", tc.e.Instance, tc.probe, got, why)
		}
	}

	now := time.Now()
	oldTmp := filepath.Join(dir, ".tmp-gone-1")
	newTmp := filepath.Join(dir, ".tmp-busy-2")
	oldJunk := filepath.Join(dir, "junk.json")
	for _, p := range []string{oldTmp, newTmp, oldJunk} {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := now.Add(-2 * TmpGrace)
	for _, p := range []string{oldTmp, oldJunk} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := Prune(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range removed {
		ids = append(ids, e.Instance)
	}
	if want := []string{"deadpid", "elsewhere", "nosock", "refused"}; !sameSet(ids, want) {
		t.Fatalf("pruned %v, want %v", ids, want)
	}
	var left []string
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		left = append(left, de.Name())
	}
	if want := []string{".tmp-busy-2", "live.json", "live.sock"}; !sameSet(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

// TestRemoveStaleKeepsLiveSocket: a stale file whose socket a new holder now listens on loses
// its file but keeps the socket.
func TestRemoveStaleKeepsLiveSocket(t *testing.T) {
	dir := shortDir(t)
	e := entry(dir, "inst", deadPid(t))
	listen(t, e.Socket, false)
	if err := Write(dir, e); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(dir, e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(dir, "inst")); !os.IsNotExist(err) {
		t.Fatalf("file kept: %v", err)
	}
	if _, err := os.Stat(e.Socket); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}
