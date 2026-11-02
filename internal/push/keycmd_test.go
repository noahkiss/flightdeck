package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// script writes a /bin/sh script and returns the argv that runs it. The tests run a shell on
// purpose; gff-hab itself never does.
func script(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key.sh")
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"/bin/sh", p}
}

// pemBody is the base64 body of a PEM key, the part that must never reach a log.
func pemBody(pemKey []byte) string {
	lines := strings.Split(strings.TrimSpace(string(pemKey)), "\n")
	return strings.Join(lines[1:len(lines)-1], "")
}

// pemOf encodes a key as a .p8 PEM.
func pemOf(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// logBuf collects log lines.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(f string, a ...any) {
	l.mu.Lock()
	fmt.Fprintf(&l.b, f+"\n", a...)
	l.mu.Unlock()
}

func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// TestKeyCommand: a command that prints the key gives the key; a non-zero exit names its status
// and the first line of stderr; a timeout, oversized output and output that is no key are
// errors; stdin is /dev/null, so a command that reads it ends at once. No error holds the key.
func TestKeyCommand(t *testing.T) {
	ctx := context.Background()
	k, pemKey := testKey(t)
	keyPath := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(keyPath, pemKey, 0o600)

	got, err := KeyCommand{Argv: []string{"/bin/cat", keyPath}}.Run(ctx)
	if err != nil || !got.Equal(k) {
		t.Fatalf("good command: %v", err)
	}

	_, err = KeyCommand{Argv: script(t, "echo 'vault is locked' >&2\necho second line >&2\nexit 3\n")}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "vault is locked") || strings.Contains(err.Error(), "second line") {
		t.Errorf("non-zero exit: %v", err)
	}

	// A command that echoes the key on stderr: the error quotes none of it.
	for _, body := range []string{
		"cat '" + keyPath + "' >&2\nexit 1\n",
		"tail -n +2 '" + keyPath + "' | tr -d '\\n' >&2\nexit 1\n",
	} {
		_, err = KeyCommand{Argv: script(t, body)}.Run(ctx)
		if err == nil || strings.Contains(err.Error(), pemBody(pemKey)[:20]) || strings.Contains(err.Error(), "PRIVATE") {
			t.Errorf("key on stderr reached the error: %v", err)
		}
	}

	// A timeout kills the whole group: the background child too, and at once, not after the
	// 2 s WaitDelay that a child holding stdout would cost.
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err = KeyCommand{Argv: script(t, "sleep 20 &\necho $! > '"+pidFile+"'\nwait\n"), Timeout: 300 * time.Millisecond}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "ran past") {
		t.Errorf("timeout: %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("timeout took %v", d)
	}
	b, _ := os.ReadFile(pidFile)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		t.Fatalf("pid file: %q", b)
	}
	gone := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			gone = true
			break
		}
	}
	if !gone {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the background child outlived the timeout")
	}

	// A command that exits 0 but leaves a child holding stdout (an agent it started): the key
	// it printed stands.
	got, err = KeyCommand{Argv: script(t, "sleep 4 &\ncat '"+keyPath+"'\n")}.Run(ctx)
	if err != nil || got == nil || !got.Equal(k) {
		t.Errorf("a child holding stdout: %v", err)
	}

	// A command killed by a signal says so.
	_, err = KeyCommand{Argv: script(t, "kill -9 $$\n")}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "killed by signal") || strings.Contains(err.Error(), "-1") {
		t.Errorf("signal: %v", err)
	}

	start = time.Now()
	_, err = KeyCommand{Argv: script(t, "while :; do cat '"+keyPath+"'; done\n"), MaxOutput: 1024}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "more than 1024 bytes") || strings.Contains(err.Error(), pemBody(pemKey)[:20]) {
		t.Errorf("oversized output: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("oversized output took %v", d)
	}

	_, err = KeyCommand{Argv: script(t, "echo not a key\n")}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "printed no APNs key") {
		t.Errorf("not a key: %v", err)
	}
	// A P-384 key is no APNs key either.
	_, err = KeyCommand{Argv: script(t, "openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 2>/dev/null || echo none\n")}.Run(ctx)
	if err == nil {
		t.Error("a P-384 key parsed")
	}

	// stdin is /dev/null: cat ends at once with nothing.
	_, err = KeyCommand{Argv: []string{"/bin/cat"}, Timeout: 5 * time.Second}.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("stdin: %v", err)
	}

	if _, err = (KeyCommand{Argv: []string{filepath.Join(t.TempDir(), "missing")}}).Run(ctx); err == nil {
		t.Error("a missing program ran")
	}
}

// TestStderrHint: the first non-empty line, cut, with control characters replaced; nothing
// for a PEM marker or a long base64 run.
func TestStderrHint(t *testing.T) {
	for in, want := range map[string]string{
		"\n  locked: unlock the vault\nmore":   "locked: unlock the vault",
		"-----BEGIN PRIVATE KEY-----":          "",
		"MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEH": "",
		"a\x1b[31mred":                         "a?[31mred",
		strings.Repeat("ab ", 60):              strings.Repeat("ab ", 40)[:120] + "…",
	} {
		if got := stderrHint([]byte(in)); got != want {
			t.Errorf("stderrHint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKeyringBackoff: a failure waits 30 s, doubling to 30 minutes, and logs once per streak;
// a good run logs the recovery and holds off a re-run for 5 minutes; a run that prints the same
// key reports no change.
func TestKeyringBackoff(t *testing.T) {
	c := newClock()
	logs := &logBuf{}
	k1, _ := testKey(t)
	k2, _ := testKey(t)
	var result *ecdsa.PrivateKey
	runs := 0
	r := newKeyring(func(context.Context) (*ecdsa.PrivateKey, error) {
		runs++
		if result == nil {
			return nil, errors.New("push key_command pass: exit status 1")
		}
		return result, nil
	}, c.Now, logs.logf)
	ctx := context.Background()

	waits := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range waits {
		if _, err := r.Load(ctx); err == nil || errors.Is(err, errKeyWait) {
			t.Fatalf("run %d: %v", i, err)
		}
		if r.Next() != w {
			t.Fatalf("after failure %d: next %v, want %v", i+1, r.Next(), w)
		}
		c.Add(w - time.Second)
		if _, err := r.Load(ctx); !errors.Is(err, errKeyWait) {
			t.Fatalf("run inside the backoff: %v", err)
		}
		c.Add(time.Second)
	}
	if runs != len(waits) || strings.Count(logs.String(), "\n") != 1 || r.Key() != nil {
		t.Fatalf("runs %d, logs %q", runs, logs.String())
	}
	result = k1
	if changed, err := r.Load(ctx); !changed || err != nil || !r.Key().Equal(k1) {
		t.Fatalf("good run: %v %v", changed, err)
	}
	if !strings.Contains(logs.String(), "printed a key") || r.Next() != keyRerunAfter {
		t.Fatalf("after a good run: %q, next %v", logs.String(), r.Next())
	}
	if _, err := r.Load(ctx); !errors.Is(err, errKeyWait) {
		t.Fatalf("re-run inside 5 minutes: %v", err)
	}
	c.Add(keyRerunAfter)
	if changed, err := r.Load(ctx); changed || err != nil {
		t.Fatalf("same key: changed %v, %v", changed, err)
	}
	// A failure after a good run keeps the key; the backoff starts again at 30 s.
	c.Add(keyRerunAfter)
	result = nil
	if _, err := r.Load(ctx); err == nil || !r.Key().Equal(k1) || r.Next() != keyRetryFirst {
		t.Fatalf("failure after a key: %v, next %v", err, r.Next())
	}
	c.Add(keyRetryFirst)
	result = k2
	if changed, _ := r.Load(ctx); !changed || !r.Key().Equal(k2) {
		t.Fatal("a new key was not taken")
	}
}

// TestClientReloadsRejectedKey: a client whose key command first prints a key APNs does not
// know gets InvalidProviderToken; inside the 5-minute hold the command does not run again and
// the refusal stands; after it, the refusal runs the command, which now prints the right key,
// and the message goes out. Before any key, Send answers ErrNoKey without a request. The key
// never reaches the log.
func TestClientReloadsRejectedKey(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	other, otherPEM := testKey(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	logs := &logBuf{}
	ring := NewKeyring(KeyCommand{Argv: []string{"/bin/cat", keyPath}}, r.clock.Now, logs.logf)
	c := NewClient(ClientConfig{Signer: NewKeyringSigner(ring, testKeyID, testTeamID, r.clock.Now), Keys: ring, Topic: testTopic, HTTP: r.httpClient,
		Hosts: map[string]string{Production: r.prod.srv.URL, Development: r.dev.srv.URL}})
	tok := strings.Repeat("05", 32)
	msg := Message{Token: tok, Environment: Production, Payload: []byte(`{}`)}

	if err := c.Send(ctx, msg); !errors.Is(err, ErrNoKey) || len(r.prod.requests()) != 0 {
		t.Fatalf("before a key: %v", err)
	}
	// The first run fails: no file yet.
	if _, err := ring.Load(ctx); err == nil {
		t.Fatal("a missing key file loaded")
	}
	os.WriteFile(keyPath, otherPEM, 0o600)
	r.clock.Add(keyRetryFirst)
	if changed, err := ring.Load(ctx); !changed || err != nil || !ring.Key().Equal(other) {
		t.Fatalf("first key: %v", err)
	}
	var e *Error
	if err := c.Send(ctx, msg); !errors.As(err, &e) || e.Reason != "InvalidProviderToken" {
		t.Fatalf("unknown key: %v", err)
	}
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("inside the hold: %d requests, want 1", n)
	}
	// The right key now; after the hold, the refusal runs the command again.
	der := r.key
	os.WriteFile(keyPath, pemOf(t, der), 0o600)
	r.clock.Add(keyRerunAfter)
	if err := c.Send(ctx, msg); err != nil {
		t.Fatalf("after the re-run: %v", err)
	}
	reqs := r.prod.requests()
	if len(reqs) != 3 || reqs[1].JWTGood || !reqs[2].JWTGood {
		t.Fatalf("requests %d: %+v", len(reqs), reqs)
	}
	for _, p := range [][]byte{otherPEM, pemOf(t, der)} {
		if strings.Contains(logs.String(), pemBody(p)[:20]) {
			t.Fatalf("the key reached the log: %q", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "printed a key") {
		t.Errorf("log: %q", logs.String())
	}
}

// TestKeyringOwnContext: a run started for a send with a short deadline goes on past it in the
// keyring's own context: the send gives up, the run ends, the key changes, nothing is logged as
// a failure, and the next send uses the new key.
func TestKeyringOwnContext(t *testing.T) {
	k1, _ := testKey(t)
	k2, _ := testKey(t)
	logs := &logBuf{}
	release := make(chan struct{})
	first := true
	r := newKeyring(func(ctx context.Context) (*ecdsa.PrivateKey, error) {
		if first {
			first = false
			return k1, nil
		}
		select {
		case <-release:
			return k2, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil, logs.logf)
	defer r.Close()
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.next = time.Time{} // past the 5-minute hold
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Load(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short deadline: %v", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for r.Key().Equal(k1) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !r.Key().Equal(k2) || logs.String() != "" {
		t.Fatalf("after the run: new key %v, log %q", r.Key().Equal(k2), logs.String())
	}
}

// TestKeyringCloseDuringRun: Close ends a run that is on; it logs nothing and sets no backoff.
func TestKeyringCloseDuringRun(t *testing.T) {
	logs := &logBuf{}
	started := make(chan struct{})
	r := newKeyring(func(ctx context.Context) (*ecdsa.PrivateKey, error) {
		close(started)
		<-ctx.Done()
		return nil, errors.New("push key_command x: killed by signal killed")
	}, nil, logs.logf)
	done := make(chan error, 1)
	go func() { _, err := r.Load(context.Background()); done <- err }()
	<-started
	r.Close()
	if err := <-done; err == nil {
		t.Fatal("a closed run gave a key")
	}
	if logs.String() != "" || r.Next() != 0 {
		t.Fatalf("log %q, next %v", logs.String(), r.Next())
	}
	if _, err := r.Load(context.Background()); err == nil {
		t.Fatal("a run after Close")
	}
}

// TestSignerFollowsKeyring: a keyring signer signs with the keyring's current key at each
// Token. Sends racing a key change never put the older key back: once the change is in, every
// token verifies with the new key.
func TestSignerFollowsKeyring(t *testing.T) {
	k1, _ := testKey(t)
	k2, _ := testKey(t)
	c := newClock()
	var mu sync.Mutex
	next := k1
	r := newKeyring(func(context.Context) (*ecdsa.PrivateKey, error) { mu.Lock(); defer mu.Unlock(); return next, nil }, c.Now, nil)
	defer r.Close()
	s := NewKeyringSigner(r, testKeyID, testTeamID, c.Now)
	if _, err := s.Token(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("before a key: %v", err)
	}
	r.Load(context.Background())
	tok, err := s.Token()
	if _, _, _, ok := checkJWT(&k1.PublicKey, tok); err != nil || !ok {
		t.Fatalf("first key: %v", err)
	}
	var wg sync.WaitGroup
	stopc := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopc:
					return
				default:
					s.Token()
				}
			}
		}()
	}
	mu.Lock()
	next = k2
	mu.Unlock()
	c.Add(keyRerunAfter)
	if changed, err := r.Load(context.Background()); !changed || err != nil {
		t.Fatalf("second key: %v %v", changed, err)
	}
	for range 200 {
		tok, _ := s.Token()
		if _, _, _, ok := checkJWT(&k2.PublicKey, tok); !ok {
			t.Fatal("a token signed with the older key after the change")
		}
	}
	close(stopc)
	wg.Wait()
	if tok, _ := s.Token(); func() bool { _, _, _, ok := checkJWT(&k2.PublicKey, tok); return !ok }() {
		t.Fatal("the older key came back")
	}
}
