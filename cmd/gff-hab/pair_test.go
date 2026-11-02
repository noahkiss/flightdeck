package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
)

// pairRun runs `gff-hab pair` on loopback with temporary files, plays the phone, and answers
// the prompt with reply. It returns pair's error, the phone's last answer and the files.
func pairRun(t *testing.T, reply string) (error, pairing.Answer, string, string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	keys := filepath.Join(dir, "home", "user", ".ssh", "authorized_keys")
	os.MkdirAll(filepath.Dir(keys), 0o700)
	before := "# mine\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJOC+phHPRbstZI8r/1OhXthhMlhAMU1TiRPPAyXm4+h laptop"
	os.WriteFile(keys, []byte(before), 0o600)
	sshd := filepath.Join(dir, "sshd")
	os.MkdirAll(sshd, 0o700)
	hpk, _, _ := ed25519.GenerateKey(rand.Reader)
	hostPub, _ := ssh.NewPublicKey(hpk)
	os.WriteFile(filepath.Join(sshd, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(hostPub), 0o644)
	os.WriteFile(filepath.Join(sshd, "ssh_host_rsa_key.pub"), []byte("ssh-rsa AAAA bad\n"), 0o644)

	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- pairCmd([]string{"-state", state, "-listen", "127.0.0.1", "-timeout", "30s", "-authorized-keys", keys,
			"-command", "/usr/local/bin/gff-hab connect", "-sshd-keys", sshd, "-label", "devbox"}, inR, outW)
		outW.Close()
	}()
	var out bytes.Buffer
	links := make(chan string, 1)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(io.TeeReader(outR, &out))
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "flightdeck://pair?") {
				links <- l
			}
		}
	}()
	var link string
	select {
	case link = <-links:
	case err := <-done:
		t.Fatalf("pair ended first: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no link")
	}
	u, _ := url.Parse(link)
	q := u.Query()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := ssh.NewSignerFromKey(k)
	c, err := ssh.Dial("tcp", q.Get("ep"), &ssh.ClientConfig{
		User: "u", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != q.Get("id") {
				return fmt.Errorf("host key %s, code says %s", ssh.FingerprintSHA256(key), q.Get("id"))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{pairing.Subsystem})); !ok || err != nil {
		t.Fatalf("subsystem %v %v", ok, err)
	}
	b, _ := json.Marshal(map[string]any{"v": 1, "token": q.Get("t"), "name": "Owner's iPhone", "kind": "phone"})
	fmt.Fprintf(ch, "%s\n", b)
	var last pairing.Answer
	sc := bufio.NewScanner(ch)
	for sc.Scan() {
		json.Unmarshal(sc.Bytes(), &last)
		if last.State == "waiting" {
			fmt.Fprintln(inW, reply)
		}
	}
	var perr error
	select {
	case perr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pair did not end")
	}
	<-scanned
	if strings.Contains(out.String(), q.Get("t")+" ") || !strings.Contains(out.String(), "Pair \"Owner's iPhone\" (phone)?") {
		t.Fatalf("terminal:\n%s", out.String())
	}
	got, _ := os.ReadFile(keys)
	if !strings.HasPrefix(string(got), before) {
		t.Fatalf("the user's lines changed:\n%s", got)
	}
	return perr, last, string(got)[len(before):], state
}

func TestPairCommand(t *testing.T) {
	err, last, added, state := pairRun(t, "y")
	if err != nil {
		t.Fatal(err)
	}
	if last.State != "paired" || last.User == "" || len(last.SSH.HostKeys) != 1 || !strings.HasPrefix(last.SSH.HostKeys[0], "ssh-ed25519 ") ||
		len(last.SSH.Endpoints) != 1 || last.SSH.Endpoints[0] != "127.0.0.1:22" || !last.ForcedCommand || last.Hab.Label != "devbox" {
		t.Fatalf("answer %+v", last)
	}
	if !strings.HasPrefix(added, "\nrestrict,command=\"/usr/local/bin/gff-hab connect\" ecdsa-sha2-nistp256 ") ||
		!strings.Contains(added, " flightdeck: Owner's iPhone (paired ") || strings.Count(added, "\n") != 2 {
		t.Fatalf("added %q", added)
	}
	var out bytes.Buffer
	if err := devicesCmd([]string{"list", "-state", state}, &out); err != nil || !strings.Contains(out.String(), "Owner's iPhone  phone  paired") {
		t.Fatalf("devices list: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := devicesCmd([]string{"revoke", "-state", state, "Owner's iPhone"}, &out); err != nil || !strings.Contains(out.String(), "removed 1 line(s)") {
		t.Fatalf("devices revoke: %v\n%s", err, out.String())
	}
}

func TestPairCommandDeclined(t *testing.T) {
	err, last, added, _ := pairRun(t, "n")
	if err == nil || !strings.Contains(err.Error(), "declined") || last.Reason != pairing.ReasonDeclined || added != "" {
		t.Fatalf("err %v, answer %+v, added %q", err, last, added)
	}
}

func TestPairAddrsAndCommand(t *testing.T) {
	for _, bad := range []string{"0.0.0.0", "::", "203.0.113.5", "nonsense"} {
		if _, err := pairAddrs([]string{bad}); err == nil {
			t.Errorf("-listen %s accepted", bad)
		}
	}
	if a, err := pairAddrs([]string{"127.0.0.1", "127.0.0.2"}); err != nil || len(a) != 2 {
		t.Fatalf("%v %v", a, err)
	}
	for in, want := range map[string]string{
		"/usr/local/bin/gff-hab":                        "/usr/local/bin/gff-hab",
		"/Users/u/Library/GFF Hab.app/Contents/gff-hab": "'/Users/u/Library/GFF Hab.app/Contents/gff-hab'",
	} {
		if got, err := shellWord(in); err != nil || got != want {
			t.Errorf("shellWord(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := shellWord("/x/it's"); err == nil {
		t.Error("a single quote was accepted")
	}
	cmd, err := connectCommand("/tmp/s p/hab.sock")
	if err != nil || !strings.HasSuffix(cmd, " connect -socket '/tmp/s p/hab.sock'") {
		t.Fatalf("%q %v", cmd, err)
	}
}
