package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/grants"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/service"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The permission check (macos-identity.md 7.2): hab.permissions runs the holder's checks in a
// probe pane, `gff-hab probe`, so the holder is the responsible process, and gff-hab's own checks
// in gff-hab. Probe panes are hidden: the structure never lists them.

// probeStart bounds the wait for a probe pane to start (7.6: exit 2 past it).
const probeStart = 5 * time.Second

// probePanesFile, under <state>/macos, lists the probe panes gff-hab has not seen closed, one
// "<uuid> <RFC 3339 time>" per line.
const probePanesFile = "probe-panes"

// staleProbe is how long an open probe pane that no holder ever listed is kept. A probe lives
// 2.5 minutes at most, and a holder lists its panes as soon as the link connects.
const staleProbe = 24 * time.Hour

// hiddenPanes holds the uuids of probe panes. A pane is open from before its spawn until gff-hab
// sees it closed or gone. The open ids are kept in a file, so a gff-hab that restarts mid-probe,
// while the holder keeps the pane, still hides it and closes it. A closed id stays an hour, long
// after its pane is gone: link notices may arrive late, and a gone uuid is never reused.
type hiddenPanes struct {
	mu   sync.Mutex
	path string // "" keeps nothing on disk
	ids  map[string]*hiddenPane
	// close releases a pane; nil until the link exists. A probe pane that is not running a
	// probe in this gff-hab, and that the holder still lists, is closed with it.
	close func(ctx context.Context, id string) error
	logf  func(string, ...any)
}

type hiddenPane struct {
	at      time.Time
	open    bool // not yet seen closed or gone
	busy    bool // a probe runs in it now
	closing bool
}

// newHiddenPanes loads the open probe panes from path.
func newHiddenPanes(path string, logf func(string, ...any)) *hiddenPanes {
	h := &hiddenPanes{path: path, ids: map[string]*hiddenPane{}, logf: logf}
	if path == "" {
		return h
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	now := time.Now()
	for _, line := range strings.Split(string(b), "\n") {
		id, ts, _ := strings.Cut(strings.TrimSpace(line), " ")
		if id == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			at = now
		}
		h.ids[id] = &hiddenPane{at: at, open: true}
	}
	return h
}

// save writes the open ids; the caller holds mu.
func (h *hiddenPanes) save() {
	if h.path == "" {
		return
	}
	var open []string
	for id, p := range h.ids {
		if p.open {
			open = append(open, id+" "+p.at.UTC().Format(time.RFC3339))
		}
	}
	if len(open) == 0 {
		if err := os.Remove(h.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.logf("probe panes: %v", err)
		}
		return
	}
	slices.Sort(open)
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		h.logf("probe panes: %v", err)
		return
	}
	if err := grants.WriteFileAtomic(h.path, []byte(strings.Join(open, "\n")+"\n")); err != nil {
		h.logf("probe panes: %v", err)
	}
}

// add records a probe pane before its spawn.
func (h *hiddenPanes) add(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for k, p := range h.ids {
		if !p.open && now.Sub(p.at) > time.Hour || p.open && !p.busy && !p.closing && now.Sub(p.at) > staleProbe {
			delete(h.ids, k)
		}
	}
	h.ids[id] = &hiddenPane{at: now, open: true, busy: true}
	h.save()
}

// done ends a probe in its pane; closed says the pane was released or never started.
func (h *hiddenPanes) done(id string, closed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.ids[id]
	if p == nil {
		return
	}
	p.busy = false
	if closed && p.open {
		p.open, p.at = false, time.Now()
		h.save()
	}
}

func (h *hiddenPanes) has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ids[id]
	return ok
}

// seen notes that the holder lists probe pane id, alive or exited. An open one no probe runs in
// is left from an earlier gff-hab, or from a close that failed: it is closed now.
func (h *hiddenPanes) seen(id string) {
	h.mu.Lock()
	p := h.ids[id]
	if p == nil || !p.open || p.busy || p.closing || h.close == nil {
		h.mu.Unlock()
		return
	}
	p.closing = true
	h.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := h.close(ctx, id)
		if err != nil && !errors.Is(err, link.ErrReleasing) {
			h.logf("probe pane %s left from before: close: %v", id, err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		p.closing = false
		if err == nil && p.open {
			p.open, p.at = false, time.Now()
			h.save()
		}
	}()
}

// gone notes that the holder no longer has probe pane id.
func (h *hiddenPanes) gone(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.ids[id]; p != nil && p.open {
		p.open, p.at = false, time.Now()
		h.save()
	}
}

// linkCloser is hiddenPanes.close for a link.
func linkCloser(lk probeLink) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		u, ok := parseUUID(id)
		if !ok {
			return fmt.Errorf("pane id %q", id)
		}
		return lk.ClosePane(ctx, u)
	}
}

// hidingHolders is the structure's view of the holders without the probe panes.
type hidingHolders struct {
	inner structure.Holders
	hide  *hiddenPanes
}

func (h hidingHolders) Panes() ([]structure.HolderPane, error) {
	ps, err := h.inner.Panes()
	if err != nil {
		return nil, err
	}
	out := ps[:0:0]
	for _, p := range ps {
		if h.hide.has(p.ID) {
			h.hide.seen(p.ID)
		} else {
			out = append(out, p)
		}
	}
	return out, nil
}

// Complete passes the inner holders' answer on (structure.Completer).
func (h hidingHolders) Complete() bool { return structure.Complete(h.inner) }

func (h hidingHolders) Subscribe(fn func(structure.Notice)) func() {
	return h.inner.Subscribe(func(n structure.Notice) {
		switch {
		case !h.hide.has(n.Pane.ID):
			fn(n)
		case n.Kind == structure.PaneGone:
			h.hide.gone(n.Pane.ID)
		default:
			h.hide.seen(n.Pane.ID)
		}
	})
}

// probeLink is what a probe needs from the holder link.
type probeLink interface {
	Spawn(ctx context.Context, s hp.Spawn) (link.PaneStatus, error)
	Subscribe(ctx context.Context, u hp.UUID, from uint64) (*link.Subscription, error)
	ClosePane(ctx context.Context, u hp.UUID) error
}

// prober runs probe panes on the current holder.
type prober struct {
	lk   probeLink
	hide *hiddenPanes
	argv []string // the probe program; the probe arguments follow
	env  func() []string
	logf func(string, ...any)
}

// run starts a probe pane, reads its result line, and releases the pane.
func (p *prober) run(ctx context.Context, checks []string, request string) (string, []grants.Check, error) {
	nonce := grants.NewNonce()
	u := newUUID()
	p.hide.add(u.String())
	// The pane is released after the probe, or after a spawn that failed (it may have started
	// late). A failed release keeps the pane on the open list, so it is closed when next seen.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := p.lk.ClosePane(cctx, u)
		if err != nil && !errors.Is(err, link.ErrNoPane) {
			p.logf("probe pane %s: close: %v", u, err)
		}
		p.hide.done(u.String(), err == nil || errors.Is(err, link.ErrNoPane))
	}()
	sctx, cancel := context.WithTimeout(ctx, probeStart)
	st, err := p.lk.Spawn(sctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/",
		Argv: append(append([]string{}, p.argv...), grants.ProbeArgs(nonce, checks, request)...), Env: p.env()})
	cancel()
	if err != nil {
		return "", nil, fmt.Errorf("%w within %v: %v", grants.ErrProbeStart, probeStart, err)
	}
	limit := grants.FDATimeout + 10*time.Second
	if request != "" {
		limit = grants.RequestTimeout + grants.LANKeepAlive + 10*time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	sub, err := p.lk.Subscribe(rctx, u, 0)
	if err != nil {
		return st.Instance, nil, fmt.Errorf("read the probe pane: %w", err)
	}
	defer sub.Close()
	var out []byte
	for {
		select {
		case ch, ok := <-sub.C:
			if !ok {
				return st.Instance, nil, fmt.Errorf("the probe pane ended with no result: %v", sub.Err())
			}
			switch ch.Kind {
			case link.ChunkBytes:
				if len(out)+len(ch.Data) > grants.MaxProbeOutput {
					return st.Instance, nil, errors.New("the probe printed too much")
				}
				out = append(out, ch.Data...)
				if r, err := grants.FindResult(out, nonce); err == nil {
					return st.Instance, r.Checks, nil
				} else if !errors.Is(err, grants.ErrNoResult) {
					return st.Instance, nil, err
				}
			case link.ChunkExited:
				return st.Instance, nil, fmt.Errorf("the probe exited (%s) with no result: %q", exitText(ch.Exit), tail(out, 200))
			}
		case <-rctx.Done():
			return st.Instance, nil, fmt.Errorf("the probe gave no result in %v", limit)
		}
	}
}

func exitText(e hp.ExitStatus) string {
	x := link.APIExit(e)
	switch {
	case x.Signal != "":
		return "signal " + x.Signal
	case x.Code != nil:
		return "status " + strconv.Itoa(*x.Code)
	}
	return "no status"
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// permissionService adapts grants.Service to the control connection.
type permissionService struct{ s *grants.Service }

func (p permissionService) Permissions(ctx context.Context, params api.PermissionsParams) (api.Permissions, error) {
	res, err := p.s.Check(ctx, params)
	var inv *grants.InvalidError
	if errors.As(err, &inv) {
		return res, control.Errorf(api.ErrorKindInvalid, "%s", inv.Error())
	}
	return res, err
}

// macosGrants reads the opt-in grant list (macos.grants, 4.1) from $GFF_MACOS_GRANTS: names
// separated by commas or spaces.
func macosGrants(getenv func(string) string) []string {
	return strings.FieldsFunc(getenv("GFF_MACOS_GRANTS"), func(r rune) bool { return r == ',' || r == ' ' })
}

// osVersion is the macOS version, or "" elsewhere.
func osVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// holderBundle is the GFF Holder.app a holder process runs from, or "".
func holderBundle(pid int) string {
	if runtime.GOOS != "darwin" || pid <= 0 {
		return ""
	}
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	b, err := service.BundleRoot(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return b
}

// permissionsConfig is what serve passes to grants.New.
type permissionsConfig struct {
	sf        *serviceFlags
	holderDir string
	store     *structure.Store
	probe     grants.ProbeFunc
	state     string
}

func newPermissions(pc permissionsConfig) (*grants.Service, error) {
	cfg := grants.Config{
		GOOS:      runtime.GOOS,
		OSVersion: osVersion(),
		Probe:     pc.probe,
		Run:       service.Exec,
		Grants:    macosGrants(os.Getenv),
		UID:       os.Getuid(),
		StateDir:  pc.state,
		ProjectDirs: func() []string {
			var out []string
			for _, p := range pc.store.ProjectDirs() {
				out = append(out, p.Dirs...)
			}
			return out
		},
	}
	if exe, err := os.Executable(); err == nil && runtime.GOOS == "darwin" {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			if b, err := service.BundleRoot(exe); err == nil {
				cfg.HabBundle = b
			}
		}
	}
	if pc.holderDir != "" {
		cfg.Holders = func() []grants.HolderInfo {
			entries, _ := holderreg.List(pc.holderDir)
			var out []grants.HolderInfo
			for _, e := range entries {
				out = append(out, grants.HolderInfo{Instance: e.Instance, Pid: e.Pid, Draining: e.Draining, Bundle: holderBundle(e.Pid)})
			}
			return out
		}
	}
	in, err := initFor(*pc.sf.init, service.None)
	if err != nil {
		return nil, err
	}
	if in != service.None {
		c, err := pc.sf.config(in)
		if err != nil {
			return nil, err
		}
		switch in {
		case service.Launchd:
			holders := cfg.Holders
			cfg.Jobs = func() []grants.Job {
				jobs := []grants.Job{{Label: c.LogicJob(), Process: api.PermissionProcessHab}}
				if holders != nil {
					for _, h := range holders() {
						jobs = append(jobs, grants.Job{Label: c.HolderJob(h.Instance), Process: api.PermissionProcessHolder, Instance: h.Instance})
					}
				}
				return jobs
			}
		case service.Systemd:
			cfg.LogicUnit = c.LogicJob()
		}
	}
	return grants.New(cfg), nil
}

// probeCmd is `gff-hab probe`: run the checks in this process and print the result line. It runs
// in a probe pane, never by hand.
func probeCmd(args []string) int {
	o, err := grants.ParseProbeArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gff-hab:", err)
		return 64
	}
	limit := grants.FDATimeout + 5*time.Second
	if o.Request != "" {
		limit = grants.RequestTimeout + grants.LANKeepAlive
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	checks := grants.Probe(ctx, grants.NewSystem(), o)
	os.Stdout.Write(grants.EncodeResult(grants.ProbeResult{Nonce: o.Nonce, Pid: os.Getpid(), Checks: checks}))
	return 0
}
