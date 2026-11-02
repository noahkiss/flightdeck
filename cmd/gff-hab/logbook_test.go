package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// 10,000 records on b, written through the file as the CLI does, 200 to a transaction.
	sb := openReplicaFile(t, stateB)
	const records = 10000
	for i := 0; i < records; i += 200 {
		var steps []logbook.Step
		for j := i; j < i+200; j++ {
			steps = append(steps, logbook.Create(logbook.TypeTask, map[string]any{"title": fmt.Sprintf("record %d", j)}))
		}
		if _, err := sb.Write(ctx, logbook.Batch{Steps: steps}); err != nil {
			t.Fatal(err)
		}
	}
	if v := vectorOf(t, sb); v[oldB] != records || sb.Origin() != oldB {
		t.Fatalf("b's vector %v, origin %s", v, sb.Origin())
	}

	peerHab(t, sockA, stateA, filesA, path, "host-a")
	idA := welcome(t, sockA).DeviceID
	waitPeer(t, sockA, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	sa := openReplicaFile(t, stateA)
	bulk := waitFor(t, "a holds b's 10,000 records", 60*time.Second, func() bool { return sameVector(vectorOf(t, sa), vectorOf(t, sb)) })
	t.Logf("%s: 10,000 records reached a %v after its link came up", tr, bulk)
	if st, err := sa.Status(ctx); err != nil || st.Records != records {
		t.Fatalf("a's status %+v %v", st, err)
	}

	// One write on b reaches a by the wake-up; one on a reaches b by a's push.
	if _, err := sb.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "one more on b"})}}); err != nil {
		t.Fatal(err)
	}
	one := waitFor(t, "b's write on a", 2*time.Second, func() bool { return sameVector(vectorOf(t, sa), vectorOf(t, sb)) })
	if _, err := sa.Write(ctx, logbook.Batch{Steps: []logbook.Step{logbook.Create(logbook.TypeTask, map[string]any{"title": "one on a"})}}); err != nil {
		t.Fatal(err)
	}
	back := waitFor(t, "a's write on b", 2*time.Second, func() bool { return vectorOf(t, sb)[idA] == 1 })
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
	waitFor(t, "the rebuilt b gets its old ops back", 60*time.Second, func() bool {
		v := vectorOf(t, sb2)
		return v[oldB] == records+1 && v[idA] == 1
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
	waitFor(t, "the rebuilt b's write on a", 5*time.Second, func() bool { return vectorOf(t, sa)[newB] == 1 })
}
