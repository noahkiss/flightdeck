package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// sshStandIn writes a stand-in for ssh into a new directory and returns that directory, to go
// first in PATH. It reads the destination after "--", maps it to a hab's socket, and runs the
// remote command with this test's gff-hab in place of the remote one: the same stdio bridge a
// real `ssh <host> gff-hab connect` gives. `ssh -O exit` (the transport's reset) fails as it
// does with no master. An unknown destination fails as ssh does.
func sshStandIn(t *testing.T, socks map[string]string) string {
	t.Helper()
	d := shortDir(t)
	var cases strings.Builder
	for host, sock := range socks {
		fmt.Fprintf(&cases, "%s) sock=%q ;;\n", host, sock)
	}
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	-O) exit 255 ;;
	--) shift; break ;;
	*) shift ;;
	esac
done
host=$1
shift
case "$host" in
` + cases.String() + `*) echo "ssh: Could not resolve hostname $host: Name or service not known" >&2; exit 255 ;;
esac
[ "$1" = gff-hab ] || { echo "stand-in: unexpected remote command $*" >&2; exit 127; }
shift
exec ` + bin + ` "$@" -socket "$sock"
`
	if err := os.WriteFile(filepath.Join(d, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// peerHab is a gff-hab serve with its own peer file, reaching its peers through the stand-in.
func peerHab(t *testing.T, sock, state, peersFile, standIn, hostID string, args ...string) *served {
	t.Helper()
	rt := shortDir(t)
	return startServeWith(t, sock, state, []string{
		"PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GFF_HAB_PEERS=" + peersFile,
		"XDG_RUNTIME_DIR=" + rt, // the ssh control directory goes here, not the user's
	}, append([]string{"-host-id", hostID, "-hook-socket", strings.TrimSuffix(sock, ".sock") + ".hook"}, args...)...)
}

func peerList(t *testing.T, sock string) api.PeerList {
	t.Helper()
	var l api.PeerList
	if err := json.Unmarshal([]byte(run(t, "peers", "-socket", sock, "-json")), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func peerRow(l api.PeerList, label string) api.Peer {
	for _, p := range l.Peers {
		if p.Label == label {
			return p
		}
	}
	return api.Peer{}
}

func waitPeer(t *testing.T, sock, label, what string, ok func(api.Peer) bool) api.Peer {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		p := peerRow(peerList(t, sock), label)
		if ok(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: peer %s is %+v", what, label, p)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitPin waits for id to be pinned in the peer file and returns the file. A peer's row is
// online, with its device id from the welcome, before the hab has written the pin.
func waitPin(t *testing.T, path, id string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), `device_id = "`+id+`"`) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pin of %s in %s:\n%s", id, path, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writePeers(t *testing.T, path, label, endpoint string) {
	t.Helper()
	s := fmt.Sprintf("# test peers\n[[peers]]\nlabel = %q\nendpoints = [%q]\n", label, endpoint)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPeersOverSSH: two habs on one machine with two peer files reach each other over the ssh
// stand-in. Each lists the other as online, pinned, at its ssh endpoint; when one stops, the
// other lists it as offline (task acceptance 1).
func TestPeersOverSSH(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	standIn := sshStandIn(t, map[string]string{"hab-a": sockA, "hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	peerHab(t, sockA, filepath.Join(d, "sa"), filesA, standIn, "host-a")
	b := peerHab(t, sockB, filepath.Join(d, "sb"), filesB, standIn, "host-b")
	idA, idB := welcome(t, sockA).DeviceID, welcome(t, sockB).DeviceID
	if !strings.Contains(strings.Join(welcome(t, sockA).Capabilities, " "), "mesh") {
		t.Fatal("no mesh capability")
	}

	pb := waitPeer(t, sockA, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.Endpoint == "hab-b" })
	pa := waitPeer(t, sockB, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.Endpoint == "hab-a" })
	if pb.DeviceID != idB || pa.DeviceID != idA || pb.Host != "host-b" || pa.Host != "host-a" || pb.LastSeen == "" {
		t.Fatalf("rows: a has %+v, b has %+v", pb, pa)
	}
	for f, id := range map[string]string{filesA: idB, filesB: idA} {
		if b := waitPin(t, f, id); !strings.HasPrefix(b, "# test peers\n") {
			t.Fatalf("%s after the first link:\n%s", f, b)
		}
	}
	table := run(t, "peers", "-socket", sockA)
	if !strings.Contains(table, "bee") || !strings.Contains(table, "online") || !strings.Contains(table, shortID(idB)) {
		t.Fatalf("peers table:\n%s", table)
	}

	b.stop(t)
	waitPeer(t, sockA, "bee", "a sees b offline", func(p api.Peer) bool { return p.State == api.PeerStateOffline && p.LastSeen != "" })
}

// TestPeerRefusedKey: the endpoint a hab pinned now answers with another host key (the peer's
// state directory was lost). The link is refused with both ids; `gff-hab peers accept` shows
// both and admits the new id (task acceptance 2).
func TestPeerRefusedKey(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	standIn := sshStandIn(t, map[string]string{"hab-b": sockB})
	filesA := filepath.Join(d, "a.toml")
	writePeers(t, filesA, "bee", "hab-b")
	emptyB := filepath.Join(d, "b.toml") // b lists no peers
	peerHab(t, sockA, filepath.Join(d, "sa"), filesA, standIn, "host-a")
	b := peerHab(t, sockB, filepath.Join(d, "sb1"), emptyB, standIn, "host-b")
	oldID := welcome(t, sockB).DeviceID
	waitPeer(t, sockA, "bee", "b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.DeviceID == oldID })
	waitPin(t, filesA, oldID)

	b.stop(t)
	peerHab(t, sockB, filepath.Join(d, "sb2"), emptyB, standIn, "host-b") // a new host key
	newID := welcome(t, sockB).DeviceID
	if newID == oldID {
		t.Fatal("the new state directory kept the key")
	}
	p := waitPeer(t, sockA, "bee", "b refused", func(p api.Peer) bool { return p.State == api.PeerStateRefused })
	if p.DeviceID != oldID || p.OfferedID != newID || !strings.Contains(p.Reason, "different hab at hab-b") ||
		!strings.Contains(p.Reason, oldID) || !strings.Contains(p.Reason, newID) {
		t.Fatalf("refused row %+v", p)
	}

	out, err := exec.Command(bin, "peers", "accept", "-socket", sockA, "nobody").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not_found") {
		t.Fatalf("accept of an unknown peer: %v\n%s", err, out)
	}
	acc := run(t, "peers", "accept", "-socket", sockA, "bee")
	if !strings.Contains(acc, "was: "+oldID) || !strings.Contains(acc, "now: "+newID) {
		t.Fatalf("accept output:\n%s", acc)
	}
	waitPeer(t, sockA, "bee", "b online under the new id", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.DeviceID == newID })
	b2, _ := os.ReadFile(filesA)
	if !strings.Contains(string(b2), `device_id = "`+newID+`"`) {
		t.Fatalf("peer file after accept:\n%s", b2)
	}
}
