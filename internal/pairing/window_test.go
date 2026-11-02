package pairing

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fixture struct {
	w       *Window
	hostPub ssh.PublicKey
	admits  atomic.Int32
	done    chan struct{}
	dev     *Device
	err     error

	mu   sync.Mutex
	logs []string
}

// lines is every line the window printed so far.
func (f *fixture) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

// open starts a window on loopback with confirm as the owner.
func open(t *testing.T, life time.Duration, confirm func(context.Context, Device) (bool, error)) *fixture {
	t.Helper()
	return openWith(t, life, confirm, nil)
}

// openWith is open with admit run inside the window's Admit, when not nil.
func openWith(t *testing.T, life time.Duration, confirm func(context.Context, Device) (bool, error), admit func(Device)) *fixture {
	t.Helper()
	_, hk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{done: make(chan struct{})}
	w, err := Open(Config{
		HostKey:  hk,
		Label:    "devbox",
		Listen:   []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Lifetime: life,
		Confirm:  confirm,
		Admit: func(d Device) (Paired, error) {
			f.admits.Add(1)
			if admit != nil {
				admit(d)
			}
			return Paired{User: "user", SSH: SSHInfo{Endpoints: []string{"192.0.2.10:22"}, HostKeys: []string{"ssh-ed25519 AAAA"}}, ForcedCommand: true}, nil
		},
		Logf: func(format string, a ...any) {
			l := fmt.Sprintf(format, a...)
			f.mu.Lock()
			f.logs = append(f.logs, l)
			f.mu.Unlock()
			t.Logf("window: %s", l)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.w = w
	signer, _ := ssh.NewSignerFromKey(hk)
	f.hostPub = signer.PublicKey()
	go func() {
		f.dev, f.err = w.Wait(context.Background())
		close(f.done)
	}()
	t.Cleanup(func() {
		select {
		case <-f.done:
		case <-time.After(10 * time.Second):
			t.Error("the window did not close")
		}
	})
	return f
}

func (f *fixture) wait(t *testing.T) (*Device, error) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
	}
	return f.dev, f.err
}

func phoneKey(t *testing.T) ssh.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dial(f *fixture, key ssh.Signer, hostPub ssh.PublicKey) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.w.Offer().Endpoints[0], &ssh.ClientConfig{
		User:            "anyone",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
		Timeout:         5 * time.Second,
	})
}

// pairWith runs one exchange as the phone, on a raw session channel as the app opens one: it
// sends line, reads every answer, and returns them with the exit status (-1 when none came).
func pairWith(t *testing.T, f *fixture, key ssh.Signer, line string, onWaiting func()) ([]Answer, int) {
	t.Helper()
	c, err := dial(f, key, f.hostPub)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := make(chan int, 1)
	go func() {
		got := -1
		for r := range reqs {
			var st struct{ Status uint32 }
			if r.Type == "exit-status" && ssh.Unmarshal(r.Payload, &st) == nil {
				got = int(st.Status)
			}
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
		status <- got
	}()
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{Subsystem}))
	if err != nil || !ok {
		t.Fatalf("subsystem: %v %v", ok, err)
	}
	fmt.Fprintln(ch, line)
	var as []Answer
	sc := bufio.NewScanner(ch)
	for sc.Scan() {
		var a Answer
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			t.Fatalf("answer %q: %v", sc.Text(), err)
		}
		as = append(as, a)
		if a.State == "waiting" && onWaiting != nil {
			onWaiting()
		}
	}
	ch.Close()
	c.Close()
	return as, <-status
}

func request(tok, name string) string {
	b, _ := json.Marshal(map[string]any{"v": 1, "token": tok, "name": name, "kind": "phone"})
	return string(b)
}

func yes(context.Context, Device) (bool, error) { return true, nil }

func TestGoodTokenPairs(t *testing.T) {
	var asked Device
	f := open(t, time.Minute, func(_ context.Context, d Device) (bool, error) { asked = d; return true, nil })
	key := phoneKey(t)
	as, status := pairWith(t, f, key, request(EncodeToken(f.w.Offer().Token), "Owner's\x1b[2J iPhone"), nil)
	if len(as) != 2 || as[0].State != "waiting" || as[1].State != "paired" || status != 0 {
		t.Fatalf("answers %+v status %d", as, status)
	}
	p := as[1]
	fp := ssh.FingerprintSHA256(key.PublicKey())
	if p.DeviceID != fp || p.Hab.ID != f.w.Offer().DeviceID || p.Hab.Label != "devbox" || p.User != "user" ||
		p.SSH.Endpoints[0] != "192.0.2.10:22" || !p.ForcedCommand {
		t.Fatalf("paired answer %+v", p)
	}
	if asked.Name != "Owner's[2J iPhone" || asked.Fingerprint != fp || asked.Kind != KindPhone {
		t.Fatalf("owner was asked about %+v", asked)
	}
	dev, err := f.wait(t)
	if err != nil || dev.Fingerprint != fp || f.admits.Load() != 1 {
		t.Fatalf("Wait %v %v, admits %d", dev, err, f.admits.Load())
	}
	// Second use: the window is closed.
	if _, err := dial(f, phoneKey(t), f.hostPub); err == nil {
		t.Fatal("the window still accepts connections after a pairing")
	}
}

func TestWrongTokenRefused(t *testing.T) {
	f := open(t, time.Minute, yes)
	other, _ := NewToken()
	for _, tok := range []string{EncodeToken(other), "short", strings.Repeat("A", 22)} {
		as, status := pairWith(t, f, phoneKey(t), request(tok, "phone"), nil)
		if len(as) != 1 || as[0].Reason != ReasonToken || status != 1 {
			t.Fatalf("token %q: answers %+v status %d", tok, as, status)
		}
	}
	if f.admits.Load() != 0 {
		t.Fatal("a wrong token admitted a key")
	}
	// The window is still open for the right token.
	as, _ := pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
	if as[len(as)-1].State != "paired" {
		t.Fatalf("after wrong tokens: %+v", as)
	}
	f.wait(t)
}

// settle waits until src holds at most n of the window's connections: a connection the client
// closed is released a moment after the client returns.
func (f *fixture) settle(t *testing.T, src string, n int) {
	t.Helper()
	a := netip.MustParseAddr(src)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		f.w.mu.Lock()
		c := f.w.conns[a]
		f.w.mu.Unlock()
		if c <= n {
			return
		}
	}
	t.Fatalf("%s still holds more than %d connections", src, n)
}

// TestWrongTokensNeverCloseOrAbort: wrong tokens from one address get that address refused, but
// the window stays open and the device waiting for the owner still pairs.
func TestWrongTokensNeverCloseOrAbort(t *testing.T) {
	release := make(chan struct{})
	f := open(t, time.Minute, func(ctx context.Context, _ Device) (bool, error) {
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	waiting := make(chan struct{})
	firstDone := make(chan []Answer, 1)
	go func() {
		as, _ := pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), func() { close(waiting) })
		firstDone <- as
	}()
	<-waiting
	bad, _ := NewToken()
	for range maxBadTokens + 2 {
		f.settle(t, "127.0.0.1", 1)
		c, err := dial(f, phoneKey(t), f.hostPub)
		if err != nil {
			continue // refused at accept once the limit is reached
		}
		ch, reqs, err := c.OpenChannel("session", nil)
		if err == nil {
			go ssh.DiscardRequests(reqs)
			ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{Subsystem}))
			fmt.Fprintln(ch, request(EncodeToken(bad), "x"))
			sc := bufio.NewScanner(ch)
			for sc.Scan() {
				if !strings.Contains(sc.Text(), `"reason":"token"`) {
					t.Errorf("wrong token answered %s", sc.Text())
				}
			}
		}
		c.Close()
	}
	f.settle(t, "127.0.0.1", 1)
	if c, err := dial(f, phoneKey(t), f.hostPub); err == nil {
		c.Close()
		t.Fatal("an address past the wrong-token limit still connects")
	}
	select {
	case <-f.done:
		t.Fatalf("wrong tokens closed the window: %v", f.err)
	default:
	}
	close(release)
	if as := <-firstDone; len(as) != 2 || as[1].State != "paired" {
		t.Fatalf("the waiting device: %+v", as)
	}
	if dev, err := f.wait(t); err != nil || dev == nil || f.admits.Load() != 1 {
		t.Fatalf("Wait %v %v, admits %d", dev, err, f.admits.Load())
	}
}

// TestConnectionsPerSource: one address holds at most maxConnsPerSource connections; a third is
// closed before the SSH banner, and the slots come back when they close.
func TestConnectionsPerSource(t *testing.T) {
	f := open(t, time.Minute, yes)
	ep := f.w.Offer().Endpoints[0]
	var held []net.Conn
	for range maxConnsPerSource {
		c, err := net.Dial("tcp", ep)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if l, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.HasPrefix(l, "SSH-2.0-") {
			t.Fatalf("banner %q %v", l, err)
		}
	}
	c, err := net.Dial("tcp", ep)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 64)); n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a third connection from one address: read %d, %v", n, err)
	}
	c.Close()
	for _, h := range held {
		h.Close()
	}
	f.settle(t, "127.0.0.1", 0)
	as, _ := pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
	if as[len(as)-1].State != "paired" {
		t.Fatalf("after the held connections closed: %+v", as)
	}
	f.wait(t)
}

func TestExpired(t *testing.T) {
	f := open(t, 300*time.Millisecond, yes)
	if _, err := f.wait(t); !errors.Is(err, ErrExpired) {
		t.Fatalf("err %v", err)
	}
	if _, err := dial(f, phoneKey(t), f.hostPub); err == nil {
		t.Fatal("an expired window accepted a connection")
	}
}

// TestExpiresWhileAsking: the owner never answers; the phone gets expired when the window ends.
func TestExpiresWhileAsking(t *testing.T) {
	f := open(t, 500*time.Millisecond, func(ctx context.Context, _ Device) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	as, status := pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
	if len(as) != 2 || as[1].Reason != ReasonExpired || status == 0 {
		t.Fatalf("answers %+v status %d", as, status)
	}
	if _, err := f.wait(t); !errors.Is(err, ErrExpired) || f.admits.Load() != 0 {
		t.Fatalf("err %v admits %d", err, f.admits.Load())
	}
}

func TestDeclinedAtPrompt(t *testing.T) {
	f := open(t, time.Minute, func(context.Context, Device) (bool, error) { return false, nil })
	as, status := pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
	if len(as) != 2 || as[1].Reason != ReasonDeclined || status != 1 {
		t.Fatalf("answers %+v status %d", as, status)
	}
	if _, err := f.wait(t); !errors.Is(err, ErrDeclined) || f.admits.Load() != 0 {
		t.Fatalf("err %v admits %d", err, f.admits.Load())
	}
}

// TestBusyThenUsed: a second device with the token while the first waits gets busy; once the
// first pairs, the token is spent.
func TestBusyThenUsed(t *testing.T) {
	release := make(chan struct{})
	f := open(t, time.Minute, func(ctx context.Context, _ Device) (bool, error) {
		<-release
		return true, nil
	})
	tok := request(EncodeToken(f.w.Offer().Token), "phone")
	firstDone := make(chan []Answer)
	waiting := make(chan struct{})
	go func() {
		as, _ := pairWith(t, f, phoneKey(t), tok, func() { close(waiting) })
		firstDone <- as
	}()
	<-waiting
	as, _ := pairWith(t, f, phoneKey(t), tok, nil)
	if len(as) != 1 || as[0].Reason != ReasonBusy {
		t.Fatalf("second device: %+v", as)
	}
	close(release)
	if as := <-firstDone; as[len(as)-1].State != "paired" {
		t.Fatalf("first device: %+v", as)
	}
	f.wait(t)
	if f.admits.Load() != 1 {
		t.Fatalf("admits %d", f.admits.Load())
	}
}

func TestOnlyTheSubsystem(t *testing.T) {
	f := open(t, time.Minute, yes)
	defer func() {
		pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
		f.wait(t)
	}()
	c, err := dial(f, phoneKey(t), f.hostPub)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, try := range []func(*ssh.Session) error{
		func(s *ssh.Session) error { return s.Run("gff-hab connect") },
		func(s *ssh.Session) error { return s.Shell() },
		func(s *ssh.Session) error { return s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}) },
		func(s *ssh.Session) error { return s.RequestSubsystem("sftp") },
		func(s *ssh.Session) error { return s.Setenv("A", "b") },
	} {
		s, err := c.NewSession()
		if err != nil {
			// Only one session channel per connection: a fresh connection for each try.
			c.Close()
			if c, err = dial(f, phoneKey(t), f.hostPub); err != nil {
				t.Fatal(err)
			}
			if s, err = c.NewSession(); err != nil {
				t.Fatal(err)
			}
		}
		if err := try(s); err == nil {
			t.Fatal("a request other than the pairing subsystem was accepted")
		}
		s.Close()
	}
	if _, err := c.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("a forwarding channel was accepted")
	}
	if ok, _, _ := c.SendRequest("tcpip-forward", true, nil); ok {
		t.Fatal("a global request was accepted")
	}
}

func TestHostKeyPinnedAndKeyTypes(t *testing.T) {
	f := open(t, time.Minute, yes)
	defer func() {
		pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
		f.wait(t)
	}()
	if _, err := dial(f, phoneKey(t), phoneKey(t).PublicKey()); err == nil {
		t.Fatal("a different host key was accepted by the phone")
	}
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := ssh.NewSignerFromKey(rk)
	if c, err := dial(f, rs, f.hostPub); err == nil {
		c.Close()
		t.Fatal("an RSA key passed auth")
	}
	if c, err := ssh.Dial("tcp", f.w.Offer().Endpoints[0], &ssh.ClientConfig{User: "u", Auth: []ssh.AuthMethod{ssh.Password("x")},
		HostKeyCallback: ssh.FixedHostKey(f.hostPub), Timeout: 5 * time.Second}); err == nil {
		c.Close()
		t.Fatal("password auth passed")
	}
}

func TestBadRequests(t *testing.T) {
	f := open(t, time.Minute, yes)
	tok := EncodeToken(f.w.Offer().Token)
	for line, reason := range map[string]string{
		`not json`:                                       ReasonRequest,
		`{"v":2,"token":"x","name":"n"}`:                 ReasonVersion,
		`{"v":1,"token":"` + tok + `"}`:                  ReasonRequest,
		`{"v":1,"token":"` + tok + `","name":"\u0000‮"}`: ReasonRequest,
		strings.Repeat("x", MaxRequest+10):               ReasonRequest,
	} {
		as, _ := pairWith(t, f, phoneKey(t), line, nil)
		if len(as) != 1 || as[0].Reason != reason {
			t.Errorf("%.40q: %+v, want %s", line, as, reason)
		}
	}
	pairWith(t, f, phoneKey(t), request(tok, "phone"), nil)
	f.wait(t)
}

func TestOpenRefusesEveryInterface(t *testing.T) {
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	for _, a := range []string{"0.0.0.0", "::", "8.8.8.8", "2001:db8::1"} {
		if _, err := Open(Config{HostKey: hk, Listen: []netip.Addr{netip.MustParseAddr(a)}}); err == nil {
			t.Errorf("Open listened on %s", a)
		}
	}
	if _, err := Open(Config{HostKey: hk}); err == nil {
		t.Error("Open with no address")
	}
	f := open(t, time.Minute, yes)
	for _, a := range f.w.Addrs() {
		if !strings.HasPrefix(a.String(), "127.0.0.1:") {
			t.Fatalf("listening on %s", a)
		}
	}
	pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), "phone"), nil)
	f.wait(t)
}

// TestRefusalLinesArePrintable: a client's channel type, request type and device name never
// reach the owner's terminal raw (no ESC, CSI, OSC or BEL), and each refusal line stays short.
func TestRefusalLinesArePrintable(t *testing.T) {
	f := open(t, time.Minute, yes)
	evil := "\x1b[2J\x1b[H\x1b]0;owned\x07\x9b31m" + strings.Repeat("\x1b[41m#", 500)
	c, err := dial(f, phoneKey(t), f.hostPub)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.OpenChannel(evil, nil); err == nil {
		t.Fatal("a channel of another type was accepted")
	}
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	if ok, _ := ch.SendRequest(evil, true, nil); ok {
		t.Fatal("a request of another type was accepted")
	}
	if _, _, err := c.OpenChannel(evil, nil); err == nil {
		t.Fatal("a second channel was accepted")
	}
	c.Close()
	pairWith(t, f, phoneKey(t), request(EncodeToken(f.w.Offer().Token), evil[:24]), nil)
	f.wait(t)
	lines := f.lines()
	refusals := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "refused ") {
			refusals++
		}
		if len(l) > 200 {
			t.Errorf("a line of %d bytes: %.80q", len(l), l)
		}
		for _, r := range l {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Errorf("a control character %U reached the terminal: %q", r, l)
				break
			}
		}
		if strings.Contains(l, "owned") {
			t.Errorf("the client's string was printed: %q", l)
		}
	}
	if refusals < 3 {
		t.Fatalf("%d refusal lines, want 3: %q", refusals, lines)
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"iPhone":                `"iPhone"`,
		"Owner\u2019s iPhone":   `"Owner’s iPhone"`,
		"a\x1b[2Jb\x07":         `"a\x1b[2Jb\a"`,
		"\u202eevil":            `"\u202eevil"`,
		"\xff":                  `"\xff"`,
		strings.Repeat("x", 70): `"` + strings.Repeat("x", 64) + `"...`,
		strings.Repeat("é", 40): `"` + strings.Repeat("é", 32) + `"...`,
	} {
		if got := Printable(in); got != want {
			t.Errorf("Printable(%q) = %s, want %s", in, got, want)
		}
	}
}
