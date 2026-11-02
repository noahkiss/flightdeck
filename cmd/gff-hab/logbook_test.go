package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// realSSHWrapper writes an ssh into a new directory, to go first in PATH, that runs the real
// ssh to $GFF_TEST_SSH_HOST (else localhost) with every option habconn passes, and maps the
// destination to a hab's socket: `ssh <opts> -- hab-b gff-hab connect` becomes
// `ssh <opts> -- localhost <this test's gff-hab> connect -socket <b's socket>`. So the link
// crosses a real sshd, as `ssh localhost` does, while each hab keeps its own test socket.
// It skips the test when key-based BatchMode ssh to that host is not set up.
func realSSHWrapper(t *testing.T, socks map[string]string) string {
	t.Helper()
	host := os.Getenv("GFF_TEST_SSH_HOST")
	if host == "" {
		host = "localhost"
	}
	real, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	if err := exec.Command(real, "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "true").Run(); err != nil {
		t.Skipf("key-based ssh to %s is not set up: %v", host, err)
	}
	d := shortDir(t)
	var cases strings.Builder
	for h, sock := range socks {
		fmt.Fprintf(&cases, "%s) sock=%q ;;\n", h, sock)
	}
	script := `#!/bin/sh
opts=""
while [ $# -gt 0 ]; do
	case "$1" in
	--) shift; break ;;
	*) opts="$opts $1"; shift ;;
	esac
done
dest=$1
shift
case "$dest" in
` + cases.String() + `*) echo "ssh: Could not resolve hostname $dest: Name or service not known" >&2; exit 255 ;;
esac
[ $# -eq 0 ] && exec ` + real + ` $opts -- ` + host + `
[ "$1" = gff-hab ] || { echo "wrapper: unexpected remote command $*" >&2; exit 127; }
shift
exec ` + real + ` $opts -- ` + host + ` ` + bin + ` "$@" -socket "$sock"
`
	if err := os.WriteFile(filepath.Join(d, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// openReplicaFile opens a hab's logbook.db the way the CLI does while gff-hab runs (logbook.md
// 4.3): a second writer of the same file, with the origin the file records.
func openReplicaFile(t *testing.T, state string) *logbook.Store {
	t.Helper()
	s, err := logbook.Open(context.Background(), filepath.Join(state, logbookFile), logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func vectorOf(t *testing.T, s *logbook.Store) logbook.Vector {
	t.Helper()
	v, err := s.Vector(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sameVector(a, b logbook.Vector) bool {
	if len(a) != len(b) {
		return false
	}
	for o, n := range a {
		if b[o] != n {
			return false
		}
	}
	return true
}

// waitFor waits until cond holds, at most limit, and returns how long that took.
func waitFor(t *testing.T, what string, limit time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > limit {
			t.Fatalf("%s: not within %v", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// TestLogbookSyncTwoHabs: two real gff-hab processes over ssh (the stand-in everywhere, the real
// ssh to localhost where key-based ssh works). A reaches B; B cannot reach A, so B's ops reach
// A by the mesh.changed wake-up. 10,000 records written on B reach A when A comes up; then one
// write on B and one on A each reach the other in under 2 s (logbook.md 18, stage 3). Then B is
// rebuilt with a new key and an empty state folder: it gets its old ops back from A, as another
// origin's ops (7.4).
func TestLogbookSyncTwoHabs(t *testing.T) {
	for _, tr := range []string{"stand-in", "ssh"} {
		t.Run(tr, func(t *testing.T) { logbookTwoHabs(t, tr) })
	}
}

func logbookTwoHabs(t *testing.T, tr string) {
	ctx := context.Background()
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	socks := map[string]string{"hab-a": sockA, "hab-b": sockB}
	var path string
	if tr == "ssh" {
		path = realSSHWrapper(t, socks)
	} else {
		path = sshStandIn(t, socks)
	}
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	stateA, stateB := filepath.Join(d, "sa"), filepath.Join(d, "sb")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-nowhere") // b cannot reach a

	b := peerHab(t, sockB, stateB, filesB, path, "host-b")
	oldB := welcome(t, sockB).DeviceID
	if !strings.Contains(strings.Join(welcome(t, sockB).Capabilities, " "), "logbook") {
		t.Fatal("no logbook capability")
	}
	// The sync methods serve peer links only.
	for _, m := range []string{"logbook.pull", "logbook.push"} {
		out, err := exec.Command(bin, "api", "call", "-socket", sockB, m, `{"have":[],"ops":[]}`).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "needs a peer link") {
			t.Fatalf("%s from a client: %v %s", m, err, out)
		}
	}
	// 10,000 records on b, written through the file as the CLI does, 200 to a transaction. The
	// race build, about twenty times slower here, checks the same paths with 1,000.
	sb := openReplicaFile(t, stateB)
	records := 10000
	if slices.Contains(buildFlags, "-race") {
		records = 1000
	}
	for i := 0; i < records; i += 200 {
		var steps []logbook.Step
		for j := i; j < i+200; j++ {
			steps = append(steps, logbook.Create(logbook.TypeTask, map[string]any{"title": fmt.Sprintf("record %d", j)}))
		}
		if _, err := sb.Write(ctx, logbook.Batch{Steps: steps}); err != nil {
			t.Fatal(err)
		}
	}
	if v := vectorOf(t, sb); v[oldB] != uint64(records) || sb.Origin() != oldB {
		t.Fatalf("b's vector %v, origin %s", v, sb.Origin())
	}

	peerHab(t, sockA, stateA, filesA, path, "host-a")
	idA := welcome(t, sockA).DeviceID
	waitPeer(t, sockA, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	sa := openReplicaFile(t, stateA)
	bulk := waitFor(t, "a holds b's records", scaleTimeout(60*time.Second), func() bool { return sameVector(vectorOf(t, sa), vectorOf(t, sb)) })
	t.Logf("%s: %d records reached a %v after its link came up", tr, records, bulk)
	if st, err := sa.Status(ctx); err != nil || st.Records != records {
		t.Fatalf("a's status %+v %v", st, err)
	}

	// One write on b reaches a by the wake-up; one on a reaches b by a's push.
	if _, err := sb.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "one more on b"})}}); err != nil {
		t.Fatal(err)
	}
	one := waitFor(t, "b's write on a", scaleTimeout(2*time.Second), func() bool { return sameVector(vectorOf(t, sa), vectorOf(t, sb)) })
	if _, err := sa.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "one on a"})}}); err != nil {
		t.Fatal(err)
	}
	back := waitFor(t, "a's write on b", scaleTimeout(2*time.Second), func() bool { return vectorOf(t, sb)[idA] == 1 })
	t.Logf("%s: one write on b reached a in %v; one on a reached b in %v", tr, one, back)
	found, err := sa.Find(ctx, "one more on b", 5)
	if err != nil || len(found) != 1 {
		t.Fatalf("a finds b's write: %v %v", found, err)
	}

	// b's disk dies: a new state folder, so a new key and an empty replica. Now it can reach a.
	b.stop(t)
	_ = sb.Close()
	stateB2 := filepath.Join(d, "sb2")
	writePeers(t, filesB, "ay", "hab-a")
	peerHab(t, sockB, stateB2, filesB, path, "host-b")
	newB := welcome(t, sockB).DeviceID
	if newB == oldB {
		t.Fatal("the rebuilt hab kept its device id")
	}
	sb2 := openReplicaFile(t, stateB2)
	waitFor(t, "the rebuilt b gets its old ops back", scaleTimeout(60*time.Second), func() bool {
		v := vectorOf(t, sb2)
		return v[oldB] == uint64(records)+1 && v[idA] == 1
	})
	if sb2.Origin() != newB {
		t.Fatalf("rebuilt b writes as %s, want %s", sb2.Origin(), newB)
	}
	if got, err := sb2.Find(ctx, "one more on b", 5); err != nil || len(got) != 1 {
		t.Fatalf("rebuilt b finds its old write: %v %v", got, err)
	}
	// Its new writes are a new origin from seq 1, and reach a.
	if _, err := sb2.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "after the rebuild"})}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rebuilt b's write on a", scaleTimeout(5*time.Second), func() bool { return vectorOf(t, sa)[newB] == 1 })
}

// openLogbook sets a corrupt file aside and starts empty; a file of another origin (a new key
// with the old state folder) is set aside and its ops are copied into the new replica as the
// old origin's ops (logbook.md 14, Built (stage 3)).
func TestOpenLogbookSetAside(t *testing.T) {
	ctx := context.Background()
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, logbookFile), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	if v := vectorOf(t, s); len(v) != 0 || s.Origin() != "SHA256:new" {
		t.Fatalf("after a corrupt file: %v %s", v, s.Origin())
	}
	_ = s.Close()
	if m, _ := filepath.Glob(filepath.Join(d, logbookFile+".corrupt-*")); len(m) != 1 || !strings.Contains(strings.Join(logs, "\n"), "set aside") {
		t.Fatalf("set aside: %v; logs %q", m, logs)
	}

	d = t.TempDir()
	old, err := logbook.Open(ctx, filepath.Join(d, logbookFile), logbook.Options{Origin: "SHA256:old"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := old.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": fmt.Sprintf("old %d", i)})}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close()
	s, err = openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatalf("other origin: %v", err)
	}
	defer s.Close()
	if v := vectorOf(t, s); v["SHA256:old"] != 3 || s.Origin() != "SHA256:new" {
		t.Fatalf("after another origin's file: %v %s", v, s.Origin())
	}
	if _, err := s.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "new"})}}); err != nil {
		t.Fatal(err)
	}
	if v := vectorOf(t, s); v["SHA256:new"] != 1 {
		t.Fatalf("the new origin starts at seq 1: %v", v)
	}
	if m, _ := filepath.Glob(filepath.Join(d, logbookFile+".other-origin-*")); len(m) != 2 || !strings.HasSuffix(m[1], ".copied") {
		t.Fatalf("set aside, and marked copied: %v", m)
	}
}

// writeOld makes a replica file of origin SHA256:old at path with n ops.
func writeOld(t *testing.T, path string, n int) {
	t.Helper()
	ctx := context.Background()
	old, err := logbook.Open(ctx, path, logbook.Options{Origin: "SHA256:old"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := old.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": fmt.Sprintf("old %d", i)})}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close()
}

// A crash after the set-aside and the new file, before the copy ended: the next start copies
// the ops, page by page, and marks the file; the start after copies nothing again. A copy that
// fails is logged, shows in Status, and is tried again at the next start.
func TestOpenLogbookCopyAgain(t *testing.T) {
	ctx := context.Background()
	defer func(n int) { copyPageOps = n }(copyPageOps)
	copyPageOps = 2
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	d := t.TempDir()
	path := filepath.Join(d, logbookFile)
	writeOld(t, path, 5)
	aside, err := setAside(path, otherOrigin)
	if err != nil {
		t.Fatal(err)
	}
	s, err := logbook.Open(ctx, path, logbook.Options{Origin: "SHA256:new"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close() // the crash: no op copied yet
	s, err = openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatal(err)
	}
	if v := vectorOf(t, s); v["SHA256:old"] != 5 {
		t.Fatalf("after the start: %v; logs %q", v, logs)
	}
	if _, err := os.Stat(aside + copiedMark); err != nil {
		t.Fatalf("no mark: %v", err)
	}
	_ = s.Close()
	logs = nil
	s, err = openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if strings.Contains(strings.Join(logs, "\n"), "copied in") {
		t.Fatalf("copied again: %q", logs)
	}

	// A set-aside file that does not open: logged, in Status, tried again; then it copies.
	d = t.TempDir()
	path = filepath.Join(d, logbookFile)
	bad := path + "." + otherOrigin + "-100"
	if err := os.WriteFile(bad, []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	logs = nil
	s, err = openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || len(st.Uncopied) != 1 || !strings.HasPrefix(st.Uncopied[0], filepath.Base(bad)+": ") ||
		!strings.Contains(strings.Join(logs, "\n"), "tried again at the next start") {
		t.Fatalf("status %+v %v; logs %q", st.Uncopied, err, logs)
	}
	_ = s.Close()
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	writeOld(t, bad, 3)
	s, err = openLogbook(ctx, d, "SHA256:new", logf)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := s.Status(ctx); len(st.Uncopied) != 0 || st.Vector["SHA256:old"] != 3 {
		t.Fatalf("after the retry: %+v", st)
	}
}

// setAside moves the WAL and the shared-memory file with the file, so nothing of the old file
// stays beside the new one. A set-aside cut short after the WAL moved is finished at the next
// start: the file goes to the WAL's name before anything opens it.
func TestSetAsideWAL(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, logbookFile)
	for _, ext := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(path+ext, []byte("content"+ext), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aside, err := setAside(path, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + ext); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s%s stays: %v", path, ext, err)
		}
		if b, err := os.ReadFile(aside + ext); err != nil || string(b) != "content"+ext {
			t.Fatalf("%s%s: %q %v", aside, ext, b, err)
		}
	}

	// The crash: the WAL moved, the file did not.
	d = t.TempDir()
	path = filepath.Join(d, logbookFile)
	writeOld(t, path, 2)
	cut := path + "." + otherOrigin + "-7"
	if err := os.WriteFile(cut+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	s, err := openLogbook(context.Background(), d, "SHA256:new", func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := vectorOf(t, s); v["SHA256:old"] != 2 || v["SHA256:new"] != 0 {
		t.Fatalf("after the finished set-aside: %v; logs %q", v, logs)
	}
	if _, err := os.Stat(cut + copiedMark); err != nil {
		t.Fatalf("the finished set-aside was not copied: %v; logs %q", err, logs)
	}
}
