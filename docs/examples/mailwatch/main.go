// Command mailwatch is an example watcher for a watch round (docs/specs/rounds.md 4.4, 4.6). It
// logs in to an IMAP server, reports each new mail from a listed sender as one event line on
// stdout, prints the cursor after each mail it read, and waits in IMAP IDLE between.
//
// Its credentials come from the environment, which a secret set of the hab's config file fills:
// IMAP_HOST (host or host:port; port 993 by default), IMAP_USER and IMAP_PASSWORD. It speaks TLS,
// and plain text only to a loopback address with -plain. It never prints the password.
//
//	mailwatch -from billing@example.com,alerts@example.net
//
// GFF_WATCH_CURSOR, set by gff-hab at each start, is the last cursor it printed:
// "<uidvalidity>:<uid>". With none, or one of another mailbox generation, it reports only mail
// that comes after its start.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	host, user, password string
	mailbox              string
	from                 map[string]bool
	plain                bool
	idle                 time.Duration // IDLE is renewed after this; servers drop it at 30 minutes
	cursor               string
}

func main() {
	from := flag.String("from", "", "comma-separated sender addresses to report")
	mailbox := flag.String("mailbox", "INBOX", "the mailbox to watch")
	plain := flag.Bool("plain", false, "plain text instead of TLS; only to a loopback address")
	flag.Parse()
	cfg := config{host: os.Getenv("IMAP_HOST"), user: os.Getenv("IMAP_USER"), password: os.Getenv("IMAP_PASSWORD"),
		mailbox: *mailbox, from: map[string]bool{}, plain: *plain, idle: 25 * time.Minute, cursor: os.Getenv("GFF_WATCH_CURSOR")}
	for _, a := range strings.Split(*from, ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			cfg.from[a] = true
		}
	}
	if cfg.host == "" || cfg.user == "" || cfg.password == "" || len(cfg.from) == 0 {
		fmt.Fprintln(os.Stderr, "mailwatch: needs -from and $IMAP_HOST, $IMAP_USER and $IMAP_PASSWORD")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, cfg, os.Stdout); err != nil && ctx.Err() == nil {
		// gff-hab restarts the watcher with a backoff (rounds.md 4.4).
		fmt.Fprintf(os.Stderr, "mailwatch: %v\n", err)
		os.Exit(1)
	}
}

// run watches until ctx ends or the connection fails.
func run(ctx context.Context, cfg config, out io.Writer) error {
	addr := cfg.host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "993")
	}
	host, port, _ := net.SplitHostPort(addr)
	if cfg.plain {
		// Every address the name has must be loopback, and the dial goes to one of those.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return err
		}
		if len(ips) == 0 {
			return fmt.Errorf("%s has no address", host)
		}
		for _, ip := range ips {
			if !ip.IP.IsLoopback() {
				return fmt.Errorf("-plain sends the password in clear text; %s is not a loopback address", ip.IP)
			}
		}
		addr = net.JoinHostPort(ips[0].IP.String(), port)
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	conn := raw
	if !cfg.plain {
		conn = tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	c := &client{conn: conn, r: bufio.NewReader(conn)}
	if _, err := c.line(); err != nil { // the greeting
		return err
	}
	if _, err := c.cmd("LOGIN " + quote(cfg.user) + " " + quote(cfg.password)); err != nil {
		return errors.New("login refused") // the server's text might echo the password
	}
	sel, err := c.cmd("SELECT " + quote(cfg.mailbox))
	if err != nil {
		return err
	}
	validity, next := code(sel, "UIDVALIDITY"), code(sel, "UIDNEXT")
	if validity == 0 || next == 0 {
		return errors.New("the server sent no UIDVALIDITY or UIDNEXT")
	}
	last := next - 1
	if v, u, ok := strings.Cut(cfg.cursor, ":"); ok {
		cv, _ := strconv.ParseUint(v, 10, 32)
		cu, err := strconv.ParseUint(u, 10, 32)
		if uint32(cv) == validity && err == nil && uint32(cu) <= last {
			last = uint32(cu)
		}
	}
	enc := json.NewEncoder(out)
	enc.Encode(map[string]string{"cursor": fmt.Sprintf("%d:%d", validity, last)})
	for {
		uids, err := c.search(last + 1)
		if err != nil {
			return err
		}
		for _, uid := range uids {
			if uid <= last {
				continue // "n:*" names the last message even when it is older than n
			}
			from, subject, err := c.headers(uid)
			if err != nil {
				return err
			}
			if cfg.from[strings.ToLower(from)] {
				enc.Encode(map[string]any{"kind": "mail", "key": fmt.Sprintf("%d:%d", validity, uid),
					"title": subject + " from " + from, "level": "active",
					"data": map[string]string{"from": from, "subject": subject}})
			}
			last = uid
			enc.Encode(map[string]string{"cursor": fmt.Sprintf("%d:%d", validity, last)})
		}
		if err := c.idle(cfg.idle); err != nil {
			return err
		}
	}
}

// client is a minimal IMAP4rev1 client: one command at a time, literals read whole.
type client struct {
	conn net.Conn
	r    *bufio.Reader
	tag  int
}

// line reads one response line, with any literal it ends with folded in.
func (c *client) line() (string, error) {
	var b strings.Builder
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		l = strings.TrimRight(l, "\r\n")
		b.WriteString(l)
		i := strings.LastIndexByte(l, '{')
		if i < 0 || !strings.HasSuffix(l, "}") {
			return b.String(), nil
		}
		n, err := strconv.Atoi(l[i+1 : len(l)-1])
		if err != nil || n < 0 || n > 1<<20 {
			return b.String(), nil
		}
		lit := make([]byte, n)
		if _, err := io.ReadFull(c.r, lit); err != nil {
			return "", err
		}
		b.WriteString("\n")
		b.Write(lit)
	}
}

// cmd sends one command and returns its untagged lines; a NO or BAD is an error.
func (c *client) cmd(s string) ([]string, error) {
	c.tag++
	tag := "a" + strconv.Itoa(c.tag)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, s); err != nil {
		return nil, err
	}
	var lines []string
	for {
		l, err := c.line()
		if err != nil {
			return nil, err
		}
		if rest, ok := strings.CutPrefix(l, tag+" "); ok {
			if strings.HasPrefix(rest, "OK") {
				return lines, nil
			}
			return nil, fmt.Errorf("%s: %s", strings.Fields(s)[0], rest)
		}
		lines = append(lines, l)
	}
}

func (c *client) search(from uint32) ([]uint32, error) {
	lines, err := c.cmd(fmt.Sprintf("UID SEARCH UID %d:*", from))
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "* SEARCH"); ok {
			for _, f := range strings.Fields(rest) {
				if n, err := strconv.ParseUint(f, 10, 32); err == nil {
					uids = append(uids, uint32(n))
				}
			}
		}
	}
	return uids, nil
}

// headers reads one message's sender address and decoded subject, leaving it unread.
func (c *client) headers(uid uint32) (from, subject string, err error) {
	lines, err := c.cmd(fmt.Sprintf("UID FETCH %d (BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)])", uid))
	if err != nil {
		return "", "", err
	}
	for _, l := range lines {
		_, block, ok := strings.Cut(l, "\n")
		if !ok {
			continue
		}
		msg, err := mail.ReadMessage(strings.NewReader(block + "\r\n"))
		if err != nil {
			continue
		}
		if a, err := mail.ParseAddress(msg.Header.Get("From")); err == nil {
			from = a.Address
		}
		subject = msg.Header.Get("Subject")
		if s, err := new(mime.WordDecoder).DecodeHeader(subject); err == nil {
			subject = s
		}
	}
	return from, oneLine(subject), nil
}

// idle waits in IDLE until the server reports new mail or after d, then ends it.
func (c *client) idle(d time.Duration) error {
	c.tag++
	tag := "a" + strconv.Itoa(c.tag)
	if _, err := fmt.Fprintf(c.conn, "%s IDLE\r\n", tag); err != nil {
		return err
	}
	if l, err := c.line(); err != nil || !strings.HasPrefix(l, "+") {
		return fmt.Errorf("IDLE refused: %q %v", l, err)
	}
	c.conn.SetReadDeadline(time.Now().Add(d))
	for {
		l, err := c.line()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			break
		}
		if err != nil {
			return err
		}
		if strings.HasSuffix(l, " EXISTS") {
			break
		}
	}
	c.conn.SetReadDeadline(time.Time{})
	if _, err := fmt.Fprint(c.conn, "DONE\r\n"); err != nil {
		return err
	}
	for {
		l, err := c.line()
		if err != nil {
			return err
		}
		if strings.HasPrefix(l, tag+" ") {
			return nil
		}
	}
}

// code reads a response code's number, such as [UIDNEXT 4521].
func code(lines []string, name string) uint32 {
	for _, l := range lines {
		if _, rest, ok := strings.Cut(l, "["+name+" "); ok {
			n, _ := strconv.ParseUint(strings.TrimRight(strings.Fields(rest)[0], "]"), 10, 32)
			return uint32(n)
		}
	}
	return 0
}

// quote is an IMAP quoted string. A CR or LF cannot be quoted; it is left out.
func quote(s string) string {
	s = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", "", "\n", "").Replace(s)
	return "\"" + s + "\""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
