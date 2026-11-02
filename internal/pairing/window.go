package pairing

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Window limits (transport.md 6.1.3).
const (
	MaxLifetime      = 5 * time.Minute
	maxConns         = 4
	handshakeTimeout = 10 * time.Second
	requestTimeout   = 20 * time.Second
	maxBadTokens     = 10
	maxRefusalLines  = 20
)

// Errors Wait returns when no device paired.
var (
	ErrExpired   = errors.New("the pairing window expired")
	ErrDeclined  = errors.New("pairing declined")
	ErrTooMany   = errors.New("too many refused tokens")
	ErrAdmitFail = errors.New("the key could not be saved")
)

// Device is a device whose token matched: what the owner is asked to confirm.
type Device struct {
	Name        string
	Kind        string
	Key         ssh.PublicKey
	Fingerprint string
	Remote      net.Addr
}

// Paired is what Admit returns for the phone: how to reach the system sshd.
type Paired struct {
	User          string
	SSH           SSHInfo
	ForcedCommand bool
}

// Config sets up a window.
type Config struct {
	HostKey  ed25519.PrivateKey
	Label    string
	Listen   []netip.Addr // checked by the caller with CheckListen or Pick
	Port     int          // 0: the system picks one
	Lifetime time.Duration
	// Confirm asks the owner. It must return when ctx ends.
	Confirm func(ctx context.Context, d Device) (bool, error)
	// Admit saves the key once the owner said yes.
	Admit func(d Device) (Paired, error)
	// Logf prints one line to the owner's terminal (refusals, the closing count).
	Logf func(format string, args ...any)
}

// Window is an open pairing window.
type Window struct {
	cfg      Config
	offer    Offer
	token    []byte
	srv      *ssh.ServerConfig
	lns      []net.Listener
	deadline time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	killed   context.Context // ends every connection
	kill     context.CancelFunc
	sem      chan struct{}
	wg       sync.WaitGroup

	mu        sync.Mutex
	pending   bool // a device waits for the owner
	done      bool // the token is burned
	badTokens int
	refusals  int
	result    chan result
}

type result struct {
	dev *Device
	err error
}

// Open listens on every address in cfg.Listen and returns the window. It serves nothing until
// Wait runs.
func Open(cfg Config) (*Window, error) {
	if len(cfg.Listen) == 0 {
		return nil, errors.New("no address to listen on")
	}
	if len(cfg.Listen) > MaxEndpoints {
		return nil, fmt.Errorf("%d addresses; at most %d", len(cfg.Listen), MaxEndpoints)
	}
	for _, a := range cfg.Listen {
		if err := CheckListen(a); err != nil {
			return nil, err
		}
	}
	if cfg.Lifetime <= 0 || cfg.Lifetime > MaxLifetime {
		cfg.Lifetime = MaxLifetime
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	signer, err := ssh.NewSignerFromKey(cfg.HostKey)
	if err != nil {
		return nil, err
	}
	tok, err := NewToken()
	if err != nil {
		return nil, err
	}
	w := &Window{cfg: cfg, token: tok, sem: make(chan struct{}, maxConns), result: make(chan result, 1)}
	w.srv = &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			switch key.Type() {
			case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
				// x/crypto hands the connection the Permissions of the key that authenticated.
				return &ssh.Permissions{Extensions: map[string]string{"key": string(key.Marshal())}}, nil
			}
			return nil, fmt.Errorf("key type %s is not accepted", key.Type())
		},
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-gff-hab-pair",
	}
	w.srv.AddHostKey(signer)
	port := cfg.Port
	for _, a := range cfg.Listen {
		ln, err := net.Listen("tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
		if err != nil && port != 0 && cfg.Port == 0 {
			ln, err = net.Listen("tcp", net.JoinHostPort(a.String(), "0"))
		}
		if err != nil {
			w.closeListeners()
			return nil, err
		}
		if port == 0 {
			port = ln.Addr().(*net.TCPAddr).Port
		}
		w.lns = append(w.lns, ln)
	}
	w.offer = Offer{DeviceID: ssh.FingerprintSHA256(signer.PublicKey()), Token: tok, Label: cfg.Label}
	for _, ln := range w.lns {
		w.offer.Endpoints = append(w.offer.Endpoints, ln.Addr().String())
	}
	return w, nil
}

// Offer is what the window's QR code carries.
func (w *Window) Offer() Offer { return w.offer }

// Addrs are the addresses the window listens on.
func (w *Window) Addrs() []net.Addr {
	var out []net.Addr
	for _, ln := range w.lns {
		out = append(out, ln.Addr())
	}
	return out
}

// Lifetime is how long the window stays open.
func (w *Window) Lifetime() time.Duration { return w.cfg.Lifetime }

func (w *Window) closeListeners() {
	for _, ln := range w.lns {
		ln.Close()
	}
}

// Wait serves the window until one device pairs (it returns that device), the owner declines,
// the window expires, too many tokens are refused, or ctx ends. The window is closed when it
// returns, and every connection has ended.
func (w *Window) Wait(ctx context.Context) (*Device, error) {
	w.deadline = time.Now().Add(w.cfg.Lifetime)
	w.ctx, w.cancel = context.WithDeadline(ctx, w.deadline)
	defer w.cancel()
	w.killed, w.kill = context.WithCancel(context.Background())
	defer w.kill()
	for _, ln := range w.lns {
		w.wg.Add(1)
		go w.accept(ln)
	}
	var r result
	select {
	case r = <-w.result:
	case <-w.ctx.Done():
		if ctx.Err() != nil {
			r.err = ctx.Err()
		} else {
			r.err = ErrExpired
		}
	}
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
	w.closeListeners()
	// End a pending question, so its phone hears expired; give each phone a moment to read its
	// answer and hang up; then close what is left.
	w.cancel()
	idle := make(chan struct{})
	go func() { w.wg.Wait(); close(idle) }()
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
	}
	w.kill()
	<-idle
	if n := w.refusals - maxRefusalLines; n > 0 {
		w.cfg.Logf("%d more refusals were not shown", n)
	}
	return r.dev, r.err
}

func (w *Window) finish(r result) {
	select {
	case w.result <- r:
	default:
	}
}

// refuse logs one refusal line, up to maxRefusalLines.
func (w *Window) refuse(remote net.Addr, fp, why string) {
	w.mu.Lock()
	w.refusals++
	show := w.refusals <= maxRefusalLines
	w.mu.Unlock()
	if !show {
		return
	}
	if fp == "" {
		fp = "no key"
	}
	w.cfg.Logf("refused %s (%s): %s", remote, fp, why)
}

func (w *Window) accept(ln net.Listener) {
	defer w.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case w.sem <- struct{}{}:
		default:
			w.refuse(c.RemoteAddr(), "", "too many connections at once")
			c.Close()
			continue
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			w.serve(c)
		}()
	}
}

func (w *Window) serve(c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(w.killed, func() { c.Close() })
	defer stop()
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	srv := *w.srv
	srv.PublicKeyCallback = func(m ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		p, err := w.srv.PublicKeyCallback(m, key)
		if err != nil {
			w.refuse(m.RemoteAddr(), ssh.FingerprintSHA256(key), err.Error())
		}
		return p, err
	}
	conn, chans, reqs, err := ssh.NewServerConn(c, &srv)
	if err != nil {
		if w.ctx.Err() == nil {
			w.refuse(c.RemoteAddr(), "", "handshake: "+err.Error())
		}
		return
	}
	defer conn.Close()
	key, err := ssh.ParsePublicKey([]byte(conn.Permissions.Extensions["key"]))
	if err != nil {
		return
	}
	c.SetDeadline(time.Now().Add(requestTimeout))
	go ssh.DiscardRequests(reqs)
	// One session channel per connection; every other channel is refused, and the loop keeps
	// draining so a client cannot stall the connection by opening more.
	first := make(chan ssh.NewChannel, 1)
	go func() {
		sent := false
		for nc := range chans {
			if nc.ChannelType() == "session" && !sent {
				sent = true
				first <- nc
				continue
			}
			w.refuse(conn.RemoteAddr(), ssh.FingerprintSHA256(key), "channel "+nc.ChannelType()+" refused")
			nc.Reject(ssh.Prohibited, "only one session channel")
		}
		close(first)
	}()
	nc, ok := <-first
	if !ok {
		return
	}
	ch, creqs, err := nc.Accept()
	if err != nil {
		return
	}
	w.session(conn, c, key, ch, creqs)
}
