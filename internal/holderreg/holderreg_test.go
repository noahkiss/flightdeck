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

// TestRemoveRaceKeepsNewFile: the reader decided the old file was stale, and a restarted holder
// wrote its new file before the reader removed anything. Remove takes the new file aside, sees
// it is not the stale one, and puts it back. No temporary file is left.
func TestRemoveRaceKeepsNewFile(t *testing.T) {
	dir := shortDir(t)
	old := entry(dir, "inst", 100)
	if err := Write(dir, old); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Pid, newer.Started = 200, old.Started.Add(time.Minute)
	// The restart lands between the reader's decision and its remove.
	if err := Write(dir, newer); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, old); err != nil {
		t.Fatal(err)
	}
	if e, err := Read(Path(dir, "inst")); err != nil || e.Pid != 200 {
		t.Fatalf("new file after remove: %+v %v", e, err)
	}
	assertOnly(t, dir, "inst.json")
}

// TestRemoveRaceInsideAside: a holder writes yet another file while Remove holds the stale one
// aside. Remove removes the stale file and keeps the one written meanwhile.
func TestRemoveRaceInsideAside(t *testing.T) {
	dir := shortDir(t)
	old := entry(dir, "inst", 100)
	if err := Write(dir, old); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Pid, newer.Started = 300, old.Started.Add(time.Minute)
	hookAside = func(string) {
		if err := Write(dir, newer); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { hookAside = nil })
	if err := Remove(dir, old); err != nil {
		t.Fatal(err)
	}
	if e, err := Read(Path(dir, "inst")); err != nil || e.Pid != 300 {
		t.Fatalf("file written during remove: %+v %v", e, err)
	}
	assertOnly(t, dir, "inst.json")

	// And the case where the file taken aside is a newer holder's, and another is written
	// before it can go back: the one written last stays.
	newest := newer
	newest.Pid, newest.Started = 400, newer.Started.Add(time.Minute)
	hookAside = func(string) {
		if err := Write(dir, newest); err != nil {
			t.Error(err)
		}
	}
	if err := Remove(dir, old); err != nil {
		t.Fatal(err)
	}
	if e, err := Read(Path(dir, "inst")); err != nil || e.Pid != 400 {
		t.Fatalf("newest file: %+v %v", e, err)
	}
	assertOnly(t, dir, "inst.json")
}

// TestRemoveStaleRaceKeepsNewSocket: the reader found the old socket refusing, and a restarted
// holder then replaced it with a live one. RemoveStale probes the socket it took aside, finds
// it live, and puts it back; the holder still accepts at its path.
func TestRemoveStaleRaceKeepsNewSocket(t *testing.T) {
	dir := shortDir(t)
	e := entry(dir, "inst", deadPid(t))
	if err := Write(dir, e); err != nil {
		t.Fatal(err)
	}
	listen(t, e.Socket, true).Close() // the crashed holder's socket: present, refusing
	if stale, _ := Stale(e, true); !stale {
		t.Fatal("dead entry not stale")
	}
	// The restarted holder replaces the socket after the reader's decision.
	if err := os.Remove(e.Socket); err != nil {
		t.Fatal(err)
	}
	l := listen(t, e.Socket, false)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := RemoveStale(dir, e); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", e.Socket)
	if err != nil {
		t.Fatalf("live socket lost: %v", err)
	}
	c.Close()
	assertOnly(t, dir, "inst.sock")

	// The tightest window: the restarted holder binds its socket while RemoveStale holds the
	// old, refusing one aside. The old one goes; the new one stays.
	l.Close()
	if err := Write(dir, e); err != nil {
		t.Fatal(err)
	}
	listen(t, e.Socket, true).Close()
	var l2 *net.UnixListener
	hookAside = func(path string) {
		if path == e.Socket {
			l2 = listen(t, e.Socket, false)
		}
	}
	t.Cleanup(func() { hookAside = nil })
	if err := RemoveStale(dir, e); err != nil {
		t.Fatal(err)
	}
	if l2 == nil {
		t.Fatal("socket was not taken aside")
	}
	if err := CheckSocket(e.Socket); err != nil {
		t.Fatalf("socket bound during the remove: %v", err)
	}
	assertOnly(t, dir, "inst.sock")
}

// TestPruneJunk: temporary files and unreadable registry files go once they are older than
// TmpGrace; fresh ones and good files stay.
func TestPruneJunk(t *testing.T) {
	dir := shortDir(t)
	if err := Write(dir, entry(dir, "good", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{".tmp-x-1", ".tmp-rm-x-2", "bad.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneJunk(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, dir, "good.json", ".tmp-x-1", ".tmp-rm-x-2", "bad.json")
	if err := PruneJunk(dir, time.Now().Add(TmpGrace)); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, dir, "good.json")
}

// TestCheckDirAndSocket: the trust checks gff-hab runs before it reads the registry and dials.
func TestCheckDirAndSocket(t *testing.T) {
	dir := shortDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatalf("private dir: %v", err)
	}
	if err := CheckDir(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(open); err == nil {
		t.Fatal("open dir passed")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(link); err == nil {
		t.Fatal("symlinked dir passed")
	}

	sock := SocketPath(dir, "s")
	l := listen(t, sock, false)
	if err := CheckSocket(sock); err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := os.Symlink(sock, SocketPath(dir, "t")); err != nil {
		t.Fatal(err)
	}
	if err := CheckSocket(SocketPath(dir, "t")); err == nil {
		t.Fatal("symlink to a socket passed")
	}
	if err := CheckSocket(Path(dir, "nothing")); err == nil {
		t.Fatal("missing socket passed")
	}

	// The peer of a connection made by this process is this user.
	go func() {
		if c, err := l.Accept(); err == nil {
			defer c.Close()
			var b [1]byte
			_, _ = c.Read(b[:])
		}
	}()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if uid, err := PeerUID(c); err != nil || uid != os.Getuid() {
		t.Fatalf("peer uid %d %v", uid, err)
	}
}

// assertOnly fails unless dir holds exactly names.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, de := range des {
		got = append(got, de.Name())
	}
	if !sameSet(got, names) {
		t.Fatalf("dir holds %v, want %v", got, names)
	}
}
