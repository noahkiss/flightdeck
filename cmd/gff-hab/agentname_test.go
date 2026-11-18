package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// agentFromPane runs `gff-hab agent <args>` as a program in the pane with GFF_HAB addr would.
func agentFromPane(addr string, args ...string) (string, int) {
	cmd := exec.Command(bin, append([]string{"agent"}, args...)...)
	cmd.Env = append(os.Environ(), "GFF_HAB="+addr)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

// TestAgentNameFromPane: an agent names its own session from its pane (harness-adapters.md,
// Agent kit). The new name is its channel address: a message it sends to an idle agent arrives
// at once, in the envelope, from the new name. A taken name or one outside the charset is
// refused and changes nothing.
func TestAgentNameFromPane(t *testing.T) {
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	addr := startSender(t, e)
	m := pasted("reviewer", "named and sending")
	startReceiver(t, e, "idle", m)

	out, code := agentFromPane(addr, "name", "-json", "reviewer")
	var s api.Session
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil || s.Name != "reviewer" {
		t.Fatalf("agent name reviewer: exit %d, %s", code, out)
	}
	if out, code := agentFromPane(addr, "name", "reviewer"); code != 0 || !strings.Contains(out, "now named reviewer") {
		t.Fatalf("the same name again: exit %d, %s", code, out)
	}
	for _, bad := range []struct{ name, want string }{{"bob", "bob"}, {"a b", "a b"}} {
		if out, code := agentFromPane(addr, "name", bad.name); code != 3 || !strings.Contains(out, bad.want) {
			t.Errorf("agent name %q: exit %d, %s", bad.name, code, out)
		}
	}

	id, st := sendFromPane(t, addr, "bob", "named and sending")
	if st != api.ChannelStateHanded {
		t.Fatalf("state %s", st)
	}
	waitUntil(t, "the message in bob's pane", fileIs(filepath.Join(e.work, "msg-1"), withID(m, id)))
	if r := channelStatus(t, e.sock, id, 20000); r.State != api.ChannelStateDelivered || r.From != "reviewer" {
		t.Fatalf("record %+v", r)
	}

	// Outside a pane the command is refused before any call.
	if out, err := exec.Command(bin, "agent", "name", "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "GFF_HAB is not set") {
		t.Fatalf("outside a pane: %v %s", err, out)
	}
}
