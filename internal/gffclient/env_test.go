package gffclient

// The test environment: the real holder and gff-hab binaries (built once in TestMain), a
// control client that stands for another device, a transport that records what gff sends on
// its control connection, a fake host terminal, and a debounce timer the test fires.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/testtmp"
	"github.com/noahkiss/flightdeck/internal/vt"
)

var (
	habBin, holderBin, gffBin string
	buildFlags                []string // race_test.go adds -race under the race detector
	gffTags                   []string // ghostty_test.go adds the ghostty tag
)

func TestMain(m *testing.M) {
	d, err := testtmp.MainDir("gffcl")
	if err != nil {
		panic(err)
	}
	habBin = filepath.Join(d, "gff-hab")
	holderBin = filepath.Join(d, "gff-hab-holder")
	gffBin = filepath.Join(d, "gff")
	os.Setenv("GFF_HOLDER_DIR", filepath.Join(d, "holders"))       // never the user's real holders
	os.Setenv("GFF_HAB_PEERS", filepath.Join(d, "peers.toml"))     // never the user's real peer list
	os.Setenv("GFF_HAB_CONFIG", filepath.Join(d, "hab.toml"))      // never the user's real hab config
	os.Setenv("GFF_HAB_DETECT", "off")                             // the catalog runs no program from PATH
	os.Setenv("GFF_WORKSPACE_DIR", filepath.Join(d, "workspaces")) // never the user's real workspace
	for _, b := range []struct {
		out, pkg string
		tags     []string
		pure     bool
	}{
		{habBin, "github.com/noahkiss/flightdeck/cmd/gff-hab", nil, false},
		{holderBin, "github.com/noahkiss/flightdeck/cmd/gff-hab-holder", nil, true},
		{gffBin, "github.com/noahkiss/flightdeck/cmd/gff", gffTags, false},
	} {
		args := []string{"build"}
		if !b.pure {
			args = append(args, buildFlags...)
		}
		if len(b.tags) > 0 {
			args = append(args, "-tags", strings.Join(b.tags, ","))
		}
		cmd := exec.Command("go", append(args, "-o", b.out, b.pkg)...)
		if b.pure {
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, out)
			testtmp.Exit(1, d)
		}
	}
	testtmp.Exit(m.Run(), d)
}

// waitLong bounds every wait; GFF_TEST_TIMEOUT_SCALE raises it on a slow machine.
func waitLong() time.Duration {
	d := 15 * time.Second
	if s := os.Getenv("GFF_TEST_TIMEOUT_SCALE"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 {
			panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", s))
		}
		d = time.Duration(float64(d) * f)
	}
	return d
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong())
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shortDir returns a private directory with a short path (macOS socket paths max 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffc")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o700)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// hab is one holder and one gff-hab serving it.
type hab struct {
	t    *testing.T
	sock string
}

func startHab(t *testing.T) *hab {
	t.Helper()
	d := shortDir(t)
	hdir, state, sock := filepath.Join(d, "h"), filepath.Join(d, "s"), filepath.Join(d, "run", "hab.sock")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(holderBin, "-dir", hdir, "-instance", "one")
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	holderDone := make(chan struct{})
	go func() { holder.Wait(); close(holderDone) }()
	t.Cleanup(func() { stopProc(holder, holderDone) })
	waitUntil(t, "the holder's registry file", func() bool {
		m, _ := filepath.Glob(filepath.Join(hdir, "*.json"))
		return len(m) > 0
	})
	serve := exec.Command(habBin, "serve", "-socket", sock, "-state", state, "-host-id", "test-host", "-holders", hdir)
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	go func() { serve.Wait(); close(serveDone) }()
	t.Cleanup(func() { stopProc(serve, serveDone) }) // runs before the holder's
	waitUntil(t, "gff-hab's socket", func() bool {
		c, err := control.Dial(sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return &hab{t: t, sock: sock}
}

func stopProc(cmd *exec.Cmd, done chan struct{}) {
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(waitLong()):
		cmd.Process.Kill()
		<-done
	}
}

// other is a control client that stands for another device.
func (h *hab) other() *control.Client {
	t := h.t
	t.Helper()
	c, err := control.Dial(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong())
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "other", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func call(t *testing.T, c *control.Client, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong())
	defer cancel()
	if err := c.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// spawn starts script under /bin/sh in a new pane of cols x rows that c owns (reason create).
func spawn(t *testing.T, c *control.Client, script string, cols, rows int) string {
	t.Helper()
	var res api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{
		Argv: []string{"/bin/sh", "-c", script}, Cwd: "/", Cols: cols, Rows: rows,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}}, &res)
	return res.Pane
}

// paneSize reads a pane's size from the structure.
func paneSize(t *testing.T, c *control.Client, pane string) (int, int, string) {
	t.Helper()
	var res api.StructureResult
	call(t, c, "structure.get", api.StructureGetParams{}, &res)
	for _, p := range res.Structure.Panes {
		if p.ID == pane {
			return p.Cols, p.Rows, p.Owner
		}
	}
	t.Fatalf("pane %s not in the structure", pane)
	return 0, 0, ""
}

// request is one JSON-RPC request gff sent on its control connection.
type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (r request) pane() string {
	var p struct {
		Pane string `json:"pane"`
	}
	json.Unmarshal(r.Params, &p)
	return p.Pane
}

// recorder is a Unix socket transport that records every request on control connections. While
// it holds data dials, each one waits, past its context, until the test releases them: a hab
// that hangs.
type recorder struct {
	habconn.UnixTransport
	mu        sync.Mutex
	reqs      []request
	gate      chan struct{} // non-nil: data dials wait until it closes
	dataDials int
}

// holdData makes data dials wait until releaseData.
func (r *recorder) holdData() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = make(chan struct{})
}

func (r *recorder) releaseData() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
}

// dials counts the data dials begun.
func (r *recorder) dials() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dataDials
}

func (r *recorder) Dial(ctx context.Context, kind habconn.Kind) (io.ReadWriteCloser, error) {
	if kind == habconn.KindData {
		r.mu.Lock()
		r.dataDials++
		g := r.gate
		r.mu.Unlock()
		if g != nil {
			<-g
		}
	}
	s, err := r.UnixTransport.Dial(ctx, kind)
	if err != nil || kind != habconn.KindControl {
		return s, err
	}
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var q request
			if json.Unmarshal(sc.Bytes(), &q) == nil && q.Method != "" {
				r.mu.Lock()
				r.reqs = append(r.reqs, q)
				r.mu.Unlock()
			}
		}
		io.Copy(io.Discard, pr)
	}()
	return &teeStream{ReadWriteCloser: s, w: pw}, nil
}

// calls returns the requests with method for pane, in order.
func (r *recorder) calls(method, pane string) []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []request
	for _, q := range r.reqs {
		if q.Method == method && (pane == "" || q.pane() == pane) {
			out = append(out, q)
		}
	}
	return out
}

type teeStream struct {
	io.ReadWriteCloser
	w *io.PipeWriter
}

func (s *teeStream) Write(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Write(p)
	s.w.Write(p[:n])
	return n, err
}

func (s *teeStream) Close() error {
	s.w.Close()
	return s.ReadWriteCloser.Close()
}

// host is a fake host terminal: input the test types, output parsed into a screen, a size,
// and an optional responder that answers every query it sees, as a real terminal would.
type host struct {
	t       *testing.T
	inR     *io.PipeReader
	inW     *io.PipeWriter
	resized chan struct{}
	answer  bool
	replies chan []byte

	mu         sync.Mutex
	cols, rows int
	term       vt.Terminal
	log        bytes.Buffer
	answered   int
}

func newHost(t *testing.T, cols, rows int, answer bool) *host {
	term, _ := vt.Fake{}.New(vt.Config{Cols: cols, Rows: rows})
	h := &host{t: t, resized: make(chan struct{}), cols: cols, rows: rows, term: term, answer: answer,
		replies: make(chan []byte, 64)}
	h.inR, h.inW = io.Pipe()
	t.Cleanup(func() { h.inW.Close() })
	go func() {
		for r := range h.replies {
			h.inW.Write(r)
		}
	}()
	t.Cleanup(func() { close(h.replies) })
	return h
}

// queryRE matches the queries a terminal answers.
var queryRE = regexp.MustCompile(`\x1b\[[?>=]?[0-9;]*[nc]|\x1b\[[0-9;\x08]*t|\x1b\]52;[^\x07\x1b]*;\?|\x1b\]1[0-2];\?|\x1bP[$+]q`)

func (h *host) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log.Write(p)
	h.term.Write(p)
	if h.answer {
		for _, m := range queryRE.FindAllIndex(h.log.Bytes(), -1)[h.answered:] {
			_ = m
			h.answered++
			h.replies <- []byte("\x1b[4;600;800t\x1b]52;c;c2VjcmV0\x07\x1b[?62;22c")
		}
	}
	return len(p), nil
}

func (h *host) size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cols, h.rows
}

// resize changes the size and waits until the client has taken the signal.
func (h *host) resize(cols, rows int) {
	h.mu.Lock()
	h.cols, h.rows = cols, rows
	h.term.Resize(cols, rows)
	h.mu.Unlock()
	select {
	case h.resized <- struct{}{}:
	case <-time.After(waitLong()):
		h.t.Fatal("the client did not take the resize")
	}
}

func (h *host) typ(s string) {
	if _, err := h.inW.Write([]byte(s)); err != nil {
		h.t.Fatal(err)
	}
}

// screen returns the host screen as text.
func (h *host) screen() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, _ := h.term.PlainText()
	return s
}

func (h *host) output() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.log.Bytes()...)
}

func (h *host) waitScreen(what string, cond func(string) bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitLong())
	for {
		s := h.screen()
		if cond(s) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; the host screen:\n%s", what, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeTimer is the debounce timer, fired by the test.
type fakeTimer struct {
	c      chan time.Time
	mu     sync.Mutex
	resets int
}

func (f *fakeTimer) C() <-chan time.Time { return f.c }
func (f *fakeTimer) Reset(time.Duration) {
	f.mu.Lock()
	f.resets++
	f.mu.Unlock()
}
func (f *fakeTimer) Stop() {}

func (f *fakeTimer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resets
}

// fire delivers the timer's tick; it waits until the client takes it.
func (f *fakeTimer) fire(t *testing.T) {
	select {
	case f.c <- time.Now():
	case <-time.After(waitLong()):
		t.Fatal("the client did not take the timer's tick")
	}
}

// gff is the client under test, run in this process.
type gff struct {
	rec   *recorder
	conn  *habconn.Conn
	host  *host
	timer *fakeTimer
	done  chan error
}

func startGFF(t *testing.T, h *hab, hst *host, split Split, panes ...PaneSpec) *gff {
	t.Helper()
	return startGFFRec(t, h, hst, &recorder{UnixTransport: habconn.UnixTransport{Path: h.sock}}, split, panes...)
}

// startGFFHeld starts gff with its data dials held: opening its panes hangs until the test
// calls releaseData. The cleanup releases them first.
func startGFFHeld(t *testing.T, h *hab, hst *host, split Split, panes ...PaneSpec) *gff {
	t.Helper()
	rec := &recorder{UnixTransport: habconn.UnixTransport{Path: h.sock}}
	rec.holdData()
	g := startGFFRec(t, h, hst, rec, split, panes...)
	t.Cleanup(rec.releaseData) // runs before startGFFRec's cleanup
	return g
}

func startGFFRec(t *testing.T, h *hab, hst *host, rec *recorder, split Split, panes ...PaneSpec) *gff {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := habconn.Connect(ctx, habconn.Config{Transport: rec,
		Client: api.ClientInfo{Name: "gff", Version: "test", Kind: api.ClientKindTerminal}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	g := &gff{rec: rec, conn: conn, host: hst, timer: &fakeTimer{c: make(chan time.Time)}, done: make(chan error, 1)}
	go func() {
		g.done <- Run(ctx, Config{Conn: conn, Parser: vt.Fake{}, Panes: panes, Split: split,
			In: hst.inR, Out: hst, Size: hst.size, Resized: hst.resized,
			NewTimer: func() Timer { return g.timer }, Logf: t.Logf})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-g.done:
		case <-time.After(waitLong()):
			t.Error("Run did not return")
		}
		conn.Close()
	})
	return g
}

func (g *gff) clientID() string {
	w, _ := g.conn.Welcome()
	return w.ClientID
}
