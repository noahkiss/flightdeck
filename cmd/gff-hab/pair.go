package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/habkey"
	"github.com/noahkiss/flightdeck/internal/pairing"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// pairCmd is `gff-hab pair` (transport.md 6.1): open a pairing window, show its code, ask the
// owner about the device that presents the token, and admit its key to the system sshd.
func pairCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	label := fs.String("label", "", "the hab's label in the code (default: the host name up to the first dot)")
	var listen listFlag
	fs.Var(&listen, "listen", "address to listen on, repeatable (default: every LAN and tailnet address)")
	port := fs.Int("port", 0, "port for the pairing window (default: one the system picks)")
	timeout := fs.Duration("timeout", pairing.MaxLifetime, "how long the window stays open, at most 5m")
	authKeys := fs.String("authorized-keys", "", "authorized_keys file for the system sshd (default: ~/.ssh/authorized_keys)")
	command := fs.String("command", "", "forced command in the key line (default: this gff-hab's path, then connect)")
	sock := socketFlag(fs)
	sshPort := fs.Int("ssh-port", 22, "the system sshd's port, told to the device")
	sshdKeys := fs.String("sshd-keys", "/etc/ssh", "folder with the system sshd's ssh_host_*_key.pub files")
	noTTY := fs.Bool("no-tty", false, "read the confirmation from a stdin that is not a terminal")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("usage: gff-hab pair [-label name] [-listen addr]... [-port n] [-timeout d] [-authorized-keys path] [-command cmd]")
	}
	if *timeout <= 0 || *timeout > pairing.MaxLifetime {
		return fmt.Errorf("-timeout %v: the window stays open at most %v", *timeout, pairing.MaxLifetime)
	}
	if !*noTTY && !stdinIsTerminal(in) {
		return errors.New("stdin is not a terminal: the owner confirms each device at the terminal (-no-tty reads the answer from a pipe)")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := habkey.LoadOrCreate(dir)
	if err != nil {
		return err
	}
	addrs, err := pairAddrs(listen)
	if err != nil {
		return err
	}
	if *authKeys == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*authKeys = filepath.Join(home, ".ssh", "authorized_keys")
	}
	if *command == "" {
		if *command, err = connectCommand(*sock); err != nil {
			return err
		}
	}
	if *label == "" {
		h, _ := os.Hostname()
		*label, _, _ = strings.Cut(h, ".")
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	var sshEndpoints []string
	for _, a := range addrs {
		sshEndpoints = append(sshEndpoints, net.JoinHostPort(a.String(), strconv.Itoa(*sshPort)))
	}
	hostKeys := sshdHostKeys(*sshdKeys)
	store := devicestore.Open(dir)
	lines := lineReader(in)

	w, err := pairing.Open(pairing.Config{
		HostKey:  key.Private,
		Label:    *label,
		Listen:   addrs,
		Port:     *port,
		Lifetime: *timeout,
		Logf:     func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) },
		Confirm: func(ctx context.Context, d pairing.Device) (bool, error) {
			return confirmDevice(ctx, d, lines, out, *authKeys)
		},
		Admit: func(d pairing.Device) (pairing.Paired, error) {
			now := time.Now()
			line, err := devicestore.KeyLine(d.Key, *command, d.Name, now)
			if err != nil {
				return pairing.Paired{}, err
			}
			added, err := devicestore.AppendAuthorizedKey(*authKeys, d.Key, line)
			if err != nil {
				return pairing.Paired{}, err
			}
			if !added {
				// Another line already holds this key, maybe with no restriction: recording
				// the device as forced-command, or revoking it later, would be wrong.
				return pairing.Paired{}, fmt.Errorf("this key is already on another line of %s, so no restricted line was added. Remove that line (gff-hab devices revoke, for a device paired before) or pair with another key, then run gff-hab pair again", *authKeys)
			}
			err = store.Add(devicestore.Record{
				ID: d.Fingerprint, Label: d.Name, Kind: d.Kind, PairedAt: now.UTC().Format(time.RFC3339),
				Key:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(d.Key))),
				AuthorizedKeys: *authKeys, State: devicestore.StatePaired,
			})
			if err != nil {
				return pairing.Paired{}, err
			}
			return pairing.Paired{User: u.Username, SSH: pairing.SSHInfo{Endpoints: sshEndpoints, HostKeys: hostKeys}, ForcedCommand: true}, nil
		},
	})
	if err != nil {
		return err
	}
	link, err := w.Offer().Link()
	if err != nil {
		return err
	}
	code, err := pairing.QR(link)
	if err != nil {
		return err
	}
	var eps []string
	for _, a := range w.Addrs() {
		eps = append(eps, a.String())
	}
	m := int(w.Lifetime() / time.Minute)
	s := int(w.Lifetime()%time.Minute) / int(time.Second)
	fmt.Fprintf(out, "Pairing window open for %d:%02d on %s.\n", m, s, strings.Join(eps, ", "))
	if len(listen) == 0 {
		fmt.Fprintln(out, "Those are this host's LAN and tailnet addresses. On a cloud host a private address can be reachable from the internet through the provider's NAT: pair there with -listen <tailnet ip>.")
	}
	fmt.Fprintf(out, "%s%s\nScan it in Flightdeck, or paste the link. Ctrl-C closes the window.\n", code, link)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := w.Wait(ctx)
	switch {
	case err == nil:
		fmt.Fprintf(out, "Paired %s: its key is in %s and in the hab's device list.\n", pairing.Printable(dev.Name), *authKeys)
		return nil
	case errors.Is(err, context.Canceled):
		return errors.New("pairing window closed")
	case errors.Is(err, pairing.ErrExpired):
		return errors.New("the pairing window expired; no device paired")
	case errors.Is(err, pairing.ErrDeclined):
		return errors.New("declined; the pairing window is closed. Run gff-hab pair again to pair a device")
	}
	return fmt.Errorf("%v; the pairing window is closed", err)
}

// codeLen is how many characters of the key fingerprint the owner types to confirm.
const codeLen = 4

// confirmTries is how many codes the owner may type before the device is declined.
const confirmTries = 3

// confirmDevice asks the owner about d. The owner confirms by typing the last codeLen characters
// of the key fingerprint the phone shows; the terminal never prints them, so the owner must read
// them off the phone, which proves the device asking is the one in hand. Enter alone, or
// confirmTries wrong codes, declines. Lines typed before the question are thrown away.
func confirmDevice(ctx context.Context, d pairing.Device, lines <-chan stampedLine, out io.Writer, authKeys string) (bool, error) {
	asked := time.Now()
	fp := d.Fingerprint
	code := fp[len(fp)-codeLen:]
	fmt.Fprintf(out, "\nPair %s (%s)?\n  key  %s %s%s\n  from %s\nTo add it to %s, type the last %d characters of the key fingerprint the phone shows (Enter alone declines): ",
		pairing.Printable(d.Name), d.Kind, d.Key.Type(), fp[:len(fp)-codeLen], strings.Repeat("?", codeLen), hostOf(d.Remote), authKeys, codeLen)
	for tries := 0; ; {
		select {
		case l, ok := <-lines:
			if !ok {
				fmt.Fprintln(out)
				return false, nil
			}
			if l.at.Before(asked) {
				continue // typed before the question: drained, never an answer
			}
			a := strings.TrimSpace(l.text)
			switch {
			case a == "":
				return false, nil
			case a == code:
				return true, nil
			}
			if tries++; tries == confirmTries {
				fmt.Fprintln(out, "That does not match the phone's key; declined.")
				return false, nil
			}
			fmt.Fprintf(out, "That does not match the phone's key. Type the last %d characters the phone shows: ", codeLen)
		case <-ctx.Done():
			fmt.Fprintln(out)
			return false, ctx.Err()
		}
	}
}

// stampedLine is one line of input and when it was read.
type stampedLine struct {
	text string
	at   time.Time
}

// lineReader hands lines from r to one channel, each stamped when read, closed at the end of r.
// It keeps reading while nobody asks, so input typed early is stamped early; past 16 waiting
// lines it drops the newest.
func lineReader(r io.Reader) <-chan stampedLine {
	ch := make(chan stampedLine, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			select {
			case ch <- stampedLine{sc.Text(), time.Now()}:
			default:
			}
		}
	}()
	return ch
}

// stdinIsTerminal says whether r is a terminal.
func stdinIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

func hostOf(a net.Addr) string {
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

// pairAddrs is the window's addresses: each -listen, checked, else every LAN and tailnet address.
func pairAddrs(listen []string) ([]netip.Addr, error) {
	if len(listen) == 0 {
		addrs, err := pairing.LocalAddrs()
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, errors.New("this host has no LAN or tailnet address to open the pairing window on; name one with -listen")
		}
		return addrs, nil
	}
	var out []netip.Addr
	for _, s := range listen {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("-listen %q: %v", s, err)
		}
		if err := pairing.CheckListen(a); err != nil {
			return nil, fmt.Errorf("-listen %s: %v", s, err)
		}
		out = append(out, a.Unmap())
	}
	if len(out) > pairing.MaxEndpoints {
		return nil, fmt.Errorf("%d addresses; at most %d", len(out), pairing.MaxEndpoints)
	}
	return out, nil
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9/._+:@%=-]+$`)

// shellWord quotes s for the shell sshd runs a forced command through.
func shellWord(s string) (string, error) {
	if plainWord.MatchString(s) {
		return s, nil
	}
	if strings.ContainsAny(s, "'\"\\\r\n") {
		return "", fmt.Errorf("%q holds a quote, a backslash or a line break", s)
	}
	return "'" + s + "'", nil
}

// connectCommand is the forced command of a paired key: the gff-hab on PATH when it is this
// binary (a stable link survives upgrades), else this binary's own path, then connect.
func connectCommand(socket string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := self
	if p, err := exec.LookPath("gff-hab"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			a, errA := os.Stat(abs)
			b, errB := os.Stat(self)
			if errA == nil && errB == nil && os.SameFile(a, b) {
				path = abs
			}
		}
	}
	w, err := shellWord(path)
	if err != nil {
		return "", err
	}
	cmd := w + " connect"
	if socket != "" {
		sw, err := shellWord(socket)
		if err != nil {
			return "", err
		}
		cmd += " -socket " + sw
	}
	return cmd, nil
}

// sshdHostKeys reads the system sshd's Ed25519 and ECDSA host keys from dir, as authorized-key
// lines with no comment. Unreadable or other keys are skipped.
func sshdHostKeys(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "ssh_host_*_key.pub"))
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || len(b) > 16<<10 {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(b)
		if err != nil {
			continue
		}
		switch pub.Type() {
		case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
			out = append(out, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))))
		}
	}
	return out
}

// devicesCmd is `gff-hab devices list` and `gff-hab devices revoke <id|label>`.
func devicesCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "revoke") {
		return errors.New("usage: gff-hab devices list [-state dir] [-json], or gff-hab devices revoke [-state dir] <id|label>")
	}
	sub := args[0]
	fs := flag.NewFlagSet("devices "+sub, flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	asJSON := fs.Bool("json", false, "list: print the records as JSON")
	fs.Parse(args[1:])
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	store := devicestore.Open(dir)
	if sub == "revoke" {
		if fs.NArg() != 1 {
			return errors.New("devices revoke needs one device id or label")
		}
		r, n, err := store.Revoke(fs.Arg(0), time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "revoked %s (%s)\n", r.Label, r.ID)
		if r.AuthorizedKeys != "" {
			fmt.Fprintf(out, "  removed %d line(s) from %s; a session already open runs until it drops\n", n, r.AuthorizedKeys)
		}
		return nil
	}
	if fs.NArg() != 0 {
		return errors.New("devices list takes no arguments")
	}
	devs, err := store.List()
	if err != nil {
		return err
	}
	if *asJSON {
		if devs == nil {
			devs = []devicestore.Record{}
		}
		b, _ := json.MarshalIndent(devs, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	}
	if len(devs) == 0 {
		fmt.Fprintln(out, "no devices; pair one with gff-hab pair")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tKIND\tSTATE\tPAIRED\tID")
	for _, d := range devs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Label, d.Kind, d.State, d.PairedAt, d.ID)
	}
	return tw.Flush()
}
