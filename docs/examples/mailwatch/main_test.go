package main

// mailwatch against a fake IMAP server on a loopback port, with invented addresses and an
// invented password: a listed sender's mail is an event, another sender's is not, the cursor
// follows each mail, IDLE wakes on new mail, a start with the cursor reports only what came
// after it, and the password reaches only the LOGIN command.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser     = "me@example.com"
	testLogin    = "invented-7c1e" // the test password
	testValidity = 1700000000
)

type fakeMail struct {
	uid           uint32
	from, subject string
}

// fakeIMAP serves one mailbox to each connection: LOGIN, SELECT, UID SEARCH, UID FETCH, IDLE.
type fakeIMAP struct {
	t    *testing.T
	ln   net.Listener
	mu   sync.Mutex
	mail []fakeMail
	wake chan struct{} // new mail for a connection in IDLE
	seen bytes.Buffer  // every command line, without LOGIN's
}

func newFakeIMAP(t *testing.T) *fakeIMAP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeIMAP{t: t, ln: ln, wake: make(chan struct{}, 1)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeIMAP) add(m fakeMail) {
	s.mu.Lock()
	s.mail = append(s.mail, m)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *fakeIMAP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fmt.Fprint(c, "* OK fake IMAP ready\r\n")
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) < 2 {
			return
		}
		tag, cmd := f[0], strings.ToUpper(f[1])
		s.mu.Lock()
		if cmd != "LOGIN" {
			s.seen.WriteString(l)
		}
		s.mu.Unlock()
		switch {
		case cmd == "LOGIN":
			if strings.TrimSpace(l) != tag+` LOGIN "`+testUser+`" "`+testLogin+`"` {
				fmt.Fprintf(c, "%s NO bad login\r\n", tag)
				continue
			}
			fmt.Fprintf(c, "%s OK logged in\r\n", tag)
		case cmd == "SELECT":
			s.mu.Lock()
			next := uint32(4500)
			if n := len(s.mail); n > 0 {
				next = s.mail[n-1].uid + 1
			}
			fmt.Fprintf(c, "* %d EXISTS\r\n* OK [UIDVALIDITY %d] ok\r\n* OK [UIDNEXT %d] ok\r\n%s OK [READ-WRITE] done\r\n", len(s.mail), testValidity, next, tag)
			s.mu.Unlock()
		case cmd == "UID" && len(f) >= 4 && strings.ToUpper(f[2]) == "SEARCH":
			lo := uint32(0)
			fmt.Sscanf(f[4], "%d:*", &lo)
			s.mu.Lock()
			var ids []string
			for _, m := range s.mail {
				if m.uid >= lo {
					ids = append(ids, fmt.Sprint(m.uid))
				}
			}
			if len(ids) == 0 && len(s.mail) > 0 { // n:* always names the last message
				ids = append(ids, fmt.Sprint(s.mail[len(s.mail)-1].uid))
			}
			s.mu.Unlock()
			fmt.Fprintf(c, "* SEARCH %s\r\n%s OK done\r\n", strings.Join(ids, " "), tag)
		case cmd == "UID" && len(f) >= 4 && strings.ToUpper(f[2]) == "FETCH":
			var uid uint32
			fmt.Sscanf(f[3], "%d", &uid)
			s.mu.Lock()
			for i, m := range s.mail {
				if m.uid == uid {
					h := "From: " + m.from + "\r\nSubject: " + m.subject + "\r\n\r\n"
					fmt.Fprintf(c, "* %d FETCH (UID %d BODY[HEADER.FIELDS (FROM SUBJECT)] {%d}\r\n%s)\r\n", i+1, uid, len(h), h)
				}
			}
			s.mu.Unlock()
			fmt.Fprintf(c, "%s OK done\r\n", tag)
		case cmd == "IDLE":
			fmt.Fprint(c, "+ idling\r\n")
			done := make(chan struct{})
			go func() {
				select {
				case <-s.wake:
					s.mu.Lock()
					fmt.Fprintf(c, "* %d EXISTS\r\n", len(s.mail))
					s.mu.Unlock()
				case <-done:
				}
			}()
			l, err := r.ReadString('\n')
			close(done)
			if err != nil || strings.TrimSpace(l) != "DONE" {
				return
			}
			fmt.Fprintf(c, "%s OK idle done\r\n", tag)
		default:
			fmt.Fprintf(c, "%s BAD unknown\r\n", tag)
		}
	}
}

// lines collects what the watcher prints, one JSON object a line.
type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) all() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, s := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (l *lines) events() (titles []string, cursor string) {
	for _, m := range l.all() {
		if c, ok := m["cursor"].(string); ok {
			cursor = c
		} else {
			titles = append(titles, m["title"].(string))
		}
	}
	return titles, cursor
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startWatch(t *testing.T, s *fakeIMAP, cursor string, out *lines) (stop func() error) {
	cfg := config{host: s.ln.Addr().String(), user: testUser, password: testLogin, mailbox: "INBOX",
		from: map[string]bool{"billing@example.com": true, "alerts@example.net": true}, plain: true, idle: time.Minute, cursor: cursor}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, out) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the watcher did not stop")
			return nil
		}
	}
}

func TestMailwatch(t *testing.T) {
	s := newFakeIMAP(t)
	s.add(fakeMail{4520, "Old <billing@example.com>", "Before the start"})
	out := &lines{}
	stop := startWatch(t, s, "", out)
	waitFor(t, "the first cursor", func() bool { _, c := out.events(); return c == "1700000000:4520" })

	// Mail during IDLE: a listed sender is an event, another sender only moves the cursor.
	s.add(fakeMail{4521, "Billing <billing@example.com>", "=?UTF-8?Q?Invoice_1042?="})
	waitFor(t, "the invoice", func() bool { _, c := out.events(); return c == "1700000000:4521" })
	s.add(fakeMail{4522, "someone@example.org", "Not watched"})
	waitFor(t, "the cursor past the unwatched mail", func() bool { _, c := out.events(); return c == "1700000000:4522" })
	titles, _ := out.events()
	if strings.Join(titles, "|") != "Invoice 1042 from billing@example.com" {
		t.Fatalf("events %q", titles)
	}
	for _, m := range out.all() {
		if m["kind"] == "mail" && (m["key"] != "1700000000:4521" || m["level"] != "active") {
			t.Fatalf("event %v", m)
		}
	}
	stop()

	// Mail while the watcher is down is reported at its next start, from the cursor on.
	s.add(fakeMail{4523, "Alerts <ALERTS@example.net>", "Disk  full\r\n on a test box"})
	out2 := &lines{}
	stop = startWatch(t, s, "1700000000:4522", out2)
	waitFor(t, "the mail from while it was down", func() bool { _, c := out2.events(); return c == "1700000000:4523" })
	stop()
	if titles, _ := out2.events(); strings.Join(titles, "|") != "Disk full on a test box from ALERTS@example.net" {
		t.Fatalf("after the restart %q", titles)
	}

	// A cursor of another mailbox generation starts from now.
	out3 := &lines{}
	stop = startWatch(t, s, "99:1", out3)
	waitFor(t, "the cursor", func() bool { _, c := out3.events(); return c == "1700000000:4523" })
	stop()
	if titles, _ := out3.events(); len(titles) != 0 {
		t.Fatalf("a stale cursor reported old mail: %q", titles)
	}

	s.mu.Lock()
	seen := s.seen.String()
	s.mu.Unlock()
	for _, b := range []string{seen, out.b.String(), out2.b.String(), out3.b.String()} {
		if strings.Contains(b, testLogin) {
			t.Fatal("the password left the LOGIN command")
		}
	}
}

func TestMailwatchRefusals(t *testing.T) {
	s := newFakeIMAP(t)
	cfg := config{host: s.ln.Addr().String(), user: testUser, password: testLogin + "-wrong", mailbox: "INBOX",
		from: map[string]bool{"a@example.com": true}, plain: true, idle: time.Minute}
	if err := run(context.Background(), cfg, &lines{}); err == nil || err.Error() != "login refused" {
		t.Fatalf("a wrong password: %v", err)
	}
	// Plain text to an address that is not loopback is refused before any dial (192.0.2.0/24 is
	// the documentation range).
	cfg.host = "192.0.2.1:143"
	if err := run(context.Background(), cfg, &lines{}); err == nil || !strings.Contains(err.Error(), "192.0.2.1 is not a loopback address") {
		t.Fatalf("plain text to a remote address: %v", err)
	}
}

func TestTitleAndBounds(t *testing.T) {
	long := strings.Repeat("é", 500)
	if got := title(long, "billing@example.com"); len([]rune(got)) != titleMax || !strings.HasSuffix(got, "… from billing@example.com") {
		t.Fatalf("%d runes: %q", len([]rune(got)), got)
	}
	if got := title("Short", "a@example.com"); got != "Short from a@example.com" {
		t.Fatalf("%q", got)
	}
	if code([]string{"* OK [UIDNEXT "}, "UIDNEXT") != 0 || code([]string{"* OK [UIDNEXT 7] x"}, "UIDNEXT") != 7 {
		t.Fatal("code")
	}
	// A response over the bound is an error, not an allocation.
	r, w := net.Pipe()
	defer r.Close()
	go func() {
		w.Write([]byte("* " + strings.Repeat("x", maxResponse+10) + "\r\n"))
		w.Close()
	}()
	c := &client{conn: r, r: bufio.NewReader(r)}
	if _, err := c.line(); err != errResponse {
		t.Fatalf("an oversize line: %v", err)
	}
}
