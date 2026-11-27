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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
)

// pairOpts says how pairRun plays the owner.
type pairOpts struct {
	// replies are typed at the prompt, one per question, given the code the phone shows.
	replies func(code string) []string
	// early is typed before the phone connects, when not empty.
	early func(code string) string
	// existing, when not nil, makes a line of authorized_keys from the phone's key, written
	// before pair runs.
	existing func(authorizedKey string) string
}

// pairResult is what pairRun saw.
type pairResult struct {
	err   error
	last  pairing.Answer
	added string // what pairing appended to authorized_keys
	state string
	out   string // the terminal
}

// pairRun runs `gff-hab pair` on loopback with temporary files, plays the phone, and answers
// the prompt as o says.
func pairRun(t *testing.T, o pairOpts) pairResult {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	keys := filepath.Join(dir, "home", "user", ".ssh", "authorized_keys")
	os.MkdirAll(filepath.Dir(keys), 0o700)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := ssh.NewSignerFromKey(k)
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	code := fp[len(fp)-4:]
	before := "# mine\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJOC+phHPRbstZI8r/1OhXthhMlhAMU1TiRPPAyXm4+h laptop"
	if o.existing != nil {
		before += "\n" + o.existing(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	}
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
			"-command", "/usr/local/bin/gff-hab connect", "-sshd-keys", sshd, "-label", "devbox", "-no-tty", "-link"}, inR, outW)
		outW.Close()
	}()
	// The terminal: a link line, and each prompt (which ends with no newline).
	var out bytes.Buffer
	links := make(chan string, 1)
	prompts := make(chan struct{}, 8)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		buf := make([]byte, 4096)
		var pending string
		for {
			n, err := outR.Read(buf)
			out.Write(buf[:n])
			pending += string(buf[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				if l := pending[:i]; strings.HasPrefix(l, "flightdeck://pair?") {
					links <- l
				}
				pending = pending[i+1:]
			}
			for _, p := range []string{"(Enter alone declines): ", "Type the last 4 characters the phone shows: "} {
				if strings.HasSuffix(pending, p) {
					prompts <- struct{}{}
					pending = ""
				}
			}
			if err != nil {
				return
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
	if o.early != nil {
		fmt.Fprintln(inW, o.early(code))
		time.Sleep(50 * time.Millisecond) // read and stamped before the question
	}
	u, _ := url.Parse(link)
	q := u.Query()
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
	go func() {
		for _, r := range o.replies(code) {
			select {
			case <-prompts:
			case <-time.After(10 * time.Second):
				return
			}
			fmt.Fprintln(inW, r)
		}
	}()
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
	var r pairResult
	sc := bufio.NewScanner(ch)
	for sc.Scan() {
		json.Unmarshal(sc.Bytes(), &r.last)
	}
	select {
	case r.err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pair did not end")
	}
	<-scanned
	r.out = out.String()
	if strings.Contains(r.out, q.Get("t")+" ") || !strings.Contains(r.out, "Pair \"Owner's iPhone\" (phone)?") {
		t.Fatalf("terminal:\n%s", r.out)
	}
	if strings.Contains(r.out, fp) || !strings.Contains(r.out, fp[:len(fp)-4]+"????") {
		t.Fatalf("the terminal printed the code the owner must read off the phone:\n%s", r.out)
	}
	got, _ := os.ReadFile(keys)
	if !strings.HasPrefix(string(got), before) {
		t.Fatalf("the user's lines changed:\n%s", got)
	}
	r.added, r.state = string(got)[len(before):], state
	return r
}

func typed(lines ...string) func(string) []string { return func(string) []string { return lines } }

func theCode(code string) []string { return []string{code} }

func TestPairCommand(t *testing.T) {
	t.Parallel()
	r := pairRun(t, pairOpts{replies: theCode})
	if r.err != nil {
		t.Fatal(r.err)
	}
	last := r.last
	if last.State != "paired" || last.User == "" || len(last.SSH.HostKeys) != 1 || !strings.HasPrefix(last.SSH.HostKeys[0], "ssh-ed25519 ") ||
		len(last.SSH.Endpoints) != 1 || last.SSH.Endpoints[0] != "127.0.0.1:22" || !last.ForcedCommand || last.Hab.Label != "devbox" {
		t.Fatalf("answer %+v", last)
	}
	if !strings.HasPrefix(r.added, "\nrestrict,command=\"/usr/local/bin/gff-hab connect\" ecdsa-sha2-nistp256 ") ||
		!strings.Contains(r.added, " flightdeck: Owner's iPhone (paired ") || strings.Count(r.added, "\n") != 2 {
		t.Fatalf("added %q", r.added)
	}
	noEnv := func(string) string { return "" }
	inPane := func(k string) string {
		if k == hook.Env {
			return "/run/hab.sock"
		}
		return ""
	}
	var out bytes.Buffer
	if err := devicesCmd([]string{"list", "-state", r.state}, &out, noEnv); err != nil || !strings.Contains(out.String(), "Owner's iPhone  phone  paired") {
		t.Fatalf("devices list: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := devicesCmd([]string{"revoke", "-state", r.state, "Owner's iPhone"}, &out, inPane); err == nil || !strings.Contains(err.Error(), "agent's pane") {
		t.Fatalf("devices revoke in an agent's pane: %v", err)
	}
	if err := devicesCmd([]string{"revoke", "-state", r.state, "Owner's iPhone"}, &out, noEnv); err != nil || !strings.Contains(out.String(), "removed 1 line(s)") || !strings.Contains(out.String(), "push registration is kept") {
		t.Fatalf("devices revoke: %v\n%s", err, out.String())
	}
}

func TestPairCommandDeclined(t *testing.T) {
	t.Parallel()
	r := pairRun(t, pairOpts{replies: typed("")})
	if r.err == nil || !strings.Contains(r.err.Error(), "declined") || r.last.Reason != pairing.ReasonDeclined || r.added != "" {
		t.Fatalf("err %v, answer %+v, added %q", r.err, r.last, r.added)
	}
}

// TestPairCommandNeedsTheCode: y is not an answer; three wrong codes decline, and a right code
// after a wrong one pairs.
func TestPairCommandNeedsTheCode(t *testing.T) {
	t.Parallel()
	r := pairRun(t, pairOpts{replies: typed("y", "yes", "zzzz")})
	if r.err == nil || r.last.Reason != pairing.ReasonDeclined || r.added != "" || !strings.Contains(r.out, "does not match the phone's key; declined") {
		t.Fatalf("err %v, answer %+v, added %q\n%s", r.err, r.last, r.added, r.out)
	}
	r = pairRun(t, pairOpts{replies: func(code string) []string { return []string{"y", code} }})
	if r.err != nil || r.last.State != "paired" {
		t.Fatalf("err %v, answer %+v\n%s", r.err, r.last, r.out)
	}
}

// TestPairCommandIgnoresEarlyInput: the right code typed before the question is drained, never
// taken as the answer.
func TestPairCommandIgnoresEarlyInput(t *testing.T) {
	t.Parallel()
	r := pairRun(t, pairOpts{early: func(code string) string { return code }, replies: typed("")})
	if r.err == nil || r.last.Reason != pairing.ReasonDeclined || r.added != "" {
		t.Fatalf("err %v, answer %+v, added %q", r.err, r.last, r.added)
	}
}

// TestPairCommandKeyAlreadyThere: a line that already holds the phone's key (here with no
// restriction) is not a forced-command pairing: pair refuses, records nothing and says why.
func TestPairCommandKeyAlreadyThere(t *testing.T) {
	t.Parallel()
	r := pairRun(t, pairOpts{replies: theCode, existing: func(k string) string { return k + " phone-by-hand" }})
	if r.err == nil || r.last.Reason != pairing.ReasonFailed || r.added != "" || !strings.Contains(r.out, "already on another line") {
		t.Fatalf("err %v, answer %+v, added %q\n%s", r.err, r.last, r.added, r.out)
	}
	var out bytes.Buffer
	if err := devicesCmd([]string{"list", "-state", r.state}, &out, func(string) string { return "" }); err != nil || !strings.Contains(out.String(), "no devices") {
		t.Fatalf("devices list: %v\n%s", err, out.String())
	}
}

// TestPairCommandNoLinkByDefault: without -link the terminal shows the QR code only, never the
// token in text.
func TestPairCommandNoLinkByDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in, inW := io.Pipe()
	defer inW.Close()
	var out bytes.Buffer
	err := pairCmd([]string{"-state", filepath.Join(dir, "state"), "-listen", "127.0.0.1", "-timeout", "300ms", "-no-tty",
		"-authorized-keys", filepath.Join(dir, "ak"), "-sshd-keys", dir}, in, &out)
	if err == nil || !strings.Contains(err.Error(), "expired") || strings.Contains(out.String(), "flightdeck://") || !strings.Contains(out.String(), "Scan it in Flightdeck.") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
}

func TestStrictModesWarnings(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "authorized_keys")
	if w := strictModesWarnings(keys); len(w) != 0 {
		t.Fatalf("a 0700 folder: %q", w)
	}
	os.Chmod(dir, 0o775)
	if w := strictModesWarnings(keys); len(w) != 1 || !strings.Contains(w[0], "chmod go-w "+dir) {
		t.Fatalf("a 0775 folder: %q", w)
	}
}

// TestPairCommandRefusesAPipe: with stdin not a terminal and no -no-tty, pair opens no window.
func TestPairCommandRefusesAPipe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var out bytes.Buffer
	err := pairCmd([]string{"-state", filepath.Join(dir, "state"), "-listen", "127.0.0.1", "-authorized-keys", filepath.Join(dir, "ak")},
		strings.NewReader("y\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "not a terminal") || strings.Contains(out.String(), "flightdeck://") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "ak")); err == nil {
		t.Fatal("authorized_keys was written")
	}
}

func TestPairAddrsAndCommand(t *testing.T) {
	t.Parallel()
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
	cmd, _, err := connectCommand("/tmp/s p/hab.sock", t.TempDir())
	if err != nil || !strings.HasSuffix(cmd, " connect -socket '/tmp/s p/hab.sock'") {
		t.Fatalf("%q %v", cmd, err)
	}
}

// TestStableExe: the forced command names a gff-hab path that survives an upgrade
// (transport.md 6.1.4). A gff-hab running from <state>/builds/<build> (macOS) is named through
// <state>/current; with no current it keeps the build path and warns; the CLI link on PATH wins
// when it is this binary; the Linux copy in <state>/bin is named as it is, with no warning.
func TestStableExe(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state dir")
	rest := filepath.Join("GFF Hab.app", "Contents", "MacOS", "gff-hab")
	self := filepath.Join(state, "builds", "0123456789abcdef", rest)
	os.MkdirAll(filepath.Dir(self), 0o700)
	os.WriteFile(self, []byte("#!/bin/sh\n"), 0o700)
	noPath := func(string) (string, error) { return "", exec.ErrNotFound }

	got, warn := stableExe(self, state, noPath)
	if got != self || !strings.Contains(warn, "build folder") || !strings.Contains(warn, filepath.Join(state, "current")) {
		t.Errorf("no current: %q, warning %q; want the build path and a warning", got, warn)
	}
	if err := os.Symlink(filepath.Join("builds", "0123456789abcdef"), filepath.Join(state, "current")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "current", rest)
	if got, warn := stableExe(self, state, noPath); got != want || warn != "" {
		t.Errorf("with current: %q, warning %q; want %q", got, warn, want)
	}
	// The state folder named through a link (macOS temp folders are): still a build path.
	link := filepath.Join(t.TempDir(), "state-link")
	os.Symlink(state, link)
	if got, warn := stableExe(self, link, noPath); got != filepath.Join(link, "current", rest) || warn != "" {
		t.Errorf("state through a link: %q, warning %q", got, warn)
	}
	cli := filepath.Join(t.TempDir(), "gff-hab")
	os.Symlink(filepath.Join(state, "current", rest), cli)
	if got, warn := stableExe(self, state, func(string) (string, error) { return cli, nil }); got != cli || warn != "" {
		t.Errorf("with a CLI link on PATH: %q, warning %q; want %q", got, warn, cli)
	}
	if got, _ := stableExe(self, state, func(string) (string, error) { return self, nil }); got != want {
		t.Errorf("a PATH folder inside the build was named: %q", got)
	}
	other := filepath.Join(t.TempDir(), "gff-hab")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700)
	if got, _ := stableExe(self, state, func(string) (string, error) { return other, nil }); got != want {
		t.Errorf("another gff-hab on PATH was named: %q", got)
	}

	bin := filepath.Join(state, "bin", "gff-hab")
	os.MkdirAll(filepath.Dir(bin), 0o700)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	if got, warn := stableExe(bin, state, noPath); got != bin || warn != "" {
		t.Errorf("Linux install: %q, warning %q; want %q and no warning", got, warn, bin)
	}
}
