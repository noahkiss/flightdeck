package grants

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func TestKnownAndLabels(t *testing.T) {
	for _, g := range []string{FullDiskAccess, LocalNetwork, Accessibility, ScreenRecording, DeveloperTools,
		"automation:com.apple.Terminal", Domain, BackgroundItem, Signature, Pasteboard} {
		if !Known(g) {
			t.Errorf("Known(%q) = false", g)
		}
	}
	for _, g := range []string{"", "fda", "automation:", "automation:x", "automation:a..b", "automation:a b.c", "automation:.a.b"} {
		if Known(g) {
			t.Errorf("Known(%q) = true", g)
		}
	}
	if Label(FullDiskAccess) != "fda" || Label(LocalNetwork) != "lan" || Label("automation:com.x.y") != "ae com.x.y" {
		t.Error("labels differ from 7.6")
	}
	if got := SettingsURL(FullDiskAccess); got != "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles" {
		t.Errorf("FDA URL %q", got)
	}
	// No Local Network anchor exists (8.1): the pane without one.
	if got := SettingsURL(LocalNetwork); strings.Contains(got, "?") {
		t.Errorf("Local Network URL %q has an anchor", got)
	}
	for _, g := range []string{FullDiskAccess, LocalNetwork, Accessibility, ScreenRecording, DeveloperTools, BackgroundItem, "automation:a.b"} {
		if SettingsURL(g) == "" || MenuPath(g) == "" {
			t.Errorf("%s: no URL or menu path", g)
		}
	}
}

func TestProbeArgsRoundTrip(t *testing.T) {
	args := ProbeArgs("abc", []string{Domain, FullDiskAccess, "automation:com.a.b"}, LocalNetwork)
	if args[0] != "probe" {
		t.Fatal(args)
	}
	o, err := ParseProbeArgs(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if o.Nonce != "abc" || o.Request != LocalNetwork || !slices.Equal(o.Checks, []string{Domain, FullDiskAccess, "automation:com.a.b"}) {
		t.Fatalf("%+v", o)
	}
	for _, bad := range [][]string{
		{},
		{"-checks", "domain"},
		{"-nonce", "n", "-checks", "nope"},
		{"-nonce", "n", "-request", FullDiskAccess},
		{"-nonce", "n", "extra"},
		{"-nonce", strings.Repeat("n", 65)},
	} {
		if _, err := ParseProbeArgs(bad); err == nil {
			t.Errorf("ParseProbeArgs(%q) accepted", bad)
		}
	}
}

func TestFindResult(t *testing.T) {
	r := ProbeResult{Nonce: "n1", Pid: 7, Checks: []Check{{Grant: FullDiskAccess, State: api.PermissionStateGranted}}}
	line := EncodeResult(r)
	// The terminal turns LF into CR LF, and other output may come first.
	stream := append([]byte("noise\r\n"), strings.ReplaceAll(string(line), "\n", "\r\n")...)
	for cut := 0; cut < len(stream)-2; cut++ { // up to the CR that ends the line
		_, err := FindResult(stream[:cut], "n1")
		if !errors.Is(err, ErrNoResult) {
			t.Fatalf("prefix of %d bytes: %v, want ErrNoResult", cut, err)
		}
	}
	got, err := FindResult(stream, "n1")
	if err != nil || got.Pid != 7 || len(got.Checks) != 1 || got.Checks[0].State != api.PermissionStateGranted {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := FindResult(stream, "n2"); !errors.Is(err, ErrNoResult) {
		t.Errorf("another nonce: %v", err)
	}
	// A line with the right nonce but a body that names another is refused, not trusted.
	forged := EncodeResult(ProbeResult{Nonce: "other"})
	forged = []byte(strings.Replace(string(forged), "other ", "n3 ", 1))
	if _, err := FindResult(forged, "n3"); err == nil || errors.Is(err, ErrNoResult) {
		t.Errorf("forged nonce: %v", err)
	}
	if _, err := FindResult([]byte(marker+" n4 !!!\n"), "n4"); err == nil || errors.Is(err, ErrNoResult) {
		t.Errorf("bad base64: %v", err)
	}
}

// fakeSys is a System with set answers.
type fakeSys struct {
	manager    string
	managerErr error
	open       map[string]error // by path; missing means granted
	block      map[string]bool  // OpenDir never returns
	ax, screen bool
	axReq      int
	screenReq  int
	ae         map[string]int32
	gw         string
	dial       []error // one per call; the last repeats
	dials      int
	clip       string
	clipErr    error
	home       string
	mu         sync.Mutex
	opened     []string // guarded by mu: an open that times out still runs
}

func (f *fakeSys) ManagerName(context.Context) (string, error) { return f.manager, f.managerErr }
func (f *fakeSys) OpenDir(p string) error {
	f.mu.Lock()
	f.opened = append(f.opened, p)
	f.mu.Unlock()
	if f.block[p] {
		select {}
	}
	return f.open[p]
}
func (f *fakeSys) AXTrusted() (bool, error)       { return f.ax, nil }
func (f *fakeSys) AXRequest() error               { f.axReq++; return nil }
func (f *fakeSys) ScreenPreflight() (bool, error) { return f.screen, nil }
func (f *fakeSys) ScreenRequest() error           { f.screenReq++; return nil }
func (f *fakeSys) Automation(t string) (int32, error) {
	st, ok := f.ae[t]
	if !ok {
		return 0, errors.New("no such target")
	}
	return st, nil
}
func (f *fakeSys) Gateway(context.Context) (string, error) {
	if f.gw == "" {
		return "", errors.New("no route")
	}
	return f.gw, nil
}
func (f *fakeSys) Dial(context.Context, string) error {
	i := min(f.dials, len(f.dial)-1)
	f.dials++
	return f.dial[i]
}
func (f *fakeSys) Clipboard(context.Context) (string, error) { return f.clip, f.clipErr }
func (f *fakeSys) SetClipboard(_ context.Context, s string) error {
	f.clip = s
	return nil
}
func (f *fakeSys) Home() string { return f.home }

func stateOf(cs []Check, g string) Check {
	for _, c := range cs {
		if c.Grant == g {
			return c
		}
	}
	return Check{}
}

func TestProbeChecks(t *testing.T) {
	ctx := context.Background()
	sys := &fakeSys{manager: "Aqua", open: map[string]error{}, ae: map[string]int32{
		"a.granted": 0, "a.denied": -1743, "a.undecided": -1744, "a.notrunning": -600, "a.unknown": -1719,
	}, ax: true}
	cs := Probe(ctx, sys, ProbeOptions{Checks: []string{Domain, FullDiskAccess, FilesAndFolders, Accessibility, ScreenRecording,
		"automation:a.granted", "automation:a.denied", "automation:a.undecided", "automation:a.notrunning", "automation:a.unknown",
		LocalNetwork, DeveloperTools}})
	want := map[string]api.PermissionState{
		Domain: api.PermissionStateGranted, FullDiskAccess: api.PermissionStateGranted, FilesAndFolders: api.PermissionStateGranted,
		Accessibility: api.PermissionStateGranted, ScreenRecording: api.PermissionStateDenied,
		"automation:a.granted": api.PermissionStateGranted, "automation:a.denied": api.PermissionStateDenied,
		"automation:a.undecided": api.PermissionStateUnknown, "automation:a.notrunning": api.PermissionStateUnknown,
		"automation:a.unknown": api.PermissionStateUnknown,
		LocalNetwork:           api.PermissionStateUnknown, DeveloperTools: api.PermissionStateUnknown,
	}
	for g, st := range want {
		if got := stateOf(cs, g); got.State != st {
			t.Errorf("%s: %s (%s), want %s", g, got.State, got.Detail, st)
		}
	}
	if sys.axReq != 0 || sys.screenReq != 0 || sys.dials != 0 {
		t.Error("a check without a request asked macOS for something")
	}

	// Not Aqua, or no launchctl: the check cannot run.
	sys.manager = "Background"
	if c := stateOf(Probe(ctx, sys, ProbeOptions{Checks: []string{Domain}}), Domain); c.State != api.PermissionStateError {
		t.Errorf("Background domain: %s", c.State)
	}
	sys.managerErr = errors.New("no launchctl")
	if c := stateOf(Probe(ctx, sys, ProbeOptions{Checks: []string{Domain}}), Domain); c.State != api.PermissionStateError {
		t.Errorf("launchctl error: %s", c.State)
	}

	// FDA denied: Files and Folders cannot be checked without risking a dialog.
	sys.open[FDAPath] = &fs.PathError{Op: "open", Path: FDAPath, Err: syscall.EPERM}
	cs = Probe(ctx, sys, ProbeOptions{Checks: []string{FullDiskAccess, FilesAndFolders}})
	if stateOf(cs, FullDiskAccess).State != api.PermissionStateDenied || stateOf(cs, FilesAndFolders).State != api.PermissionStateUnknown {
		t.Errorf("FDA denied: %+v", cs)
	}

	// A request asks first, once.
	Probe(ctx, sys, ProbeOptions{Checks: []string{Accessibility}, Request: Accessibility})
	Probe(ctx, sys, ProbeOptions{Checks: []string{ScreenRecording}, Request: ScreenRecording})
	if sys.axReq != 1 || sys.screenReq != 1 {
		t.Errorf("requests: ax %d, screen %d", sys.axReq, sys.screenReq)
	}
}

func TestOpenCheck(t *testing.T) {
	ctx := context.Background()
	sys := &fakeSys{open: map[string]error{
		"/denied":  &fs.PathError{Op: "open", Err: syscall.EACCES},
		"/missing": &fs.PathError{Op: "open", Err: syscall.ENOENT},
		"/odd":     errors.New("odd"),
	}, block: map[string]bool{"/hang": true}}
	for path, want := range map[string]api.PermissionState{
		"/ok": api.PermissionStateGranted, "/denied": api.PermissionStateDenied, "/missing": api.PermissionStateUnknown,
		"/odd": api.PermissionStateUnknown, "/hang": api.PermissionStateUnanswered,
	} {
		if got := OpenCheck(ctx, sys, FullDiskAccess, path, 50*time.Millisecond); got.State != want {
			t.Errorf("%s: %s, want %s", path, got.State, want)
		}
	}
}

func TestFilesRequest(t *testing.T) {
	home := "/home/u"
	sys := &fakeSys{home: home, open: map[string]error{FDAPath: &fs.PathError{Err: syscall.EPERM}}}
	c := stateOf(Probe(context.Background(), sys, ProbeOptions{Checks: []string{FilesAndFolders}, Request: FilesAndFolders}), FilesAndFolders)
	if c.State != api.PermissionStateGranted {
		t.Errorf("all folders open: %+v", c)
	}
	sys.open[filepath.Join(home, "Documents")] = &fs.PathError{Err: syscall.EPERM}
	c = stateOf(Probe(context.Background(), sys, ProbeOptions{Checks: []string{FilesAndFolders}, Request: FilesAndFolders}), FilesAndFolders)
	if c.State != api.PermissionStateDenied || !strings.Contains(c.Detail, "Documents") {
		t.Errorf("Documents denied: %+v", c)
	}
}

func TestLocalNetworkRequest(t *testing.T) {
	defer func(w, r time.Duration) { lanWindow, lanRetry = w, r }(lanWindow, lanRetry)
	lanWindow, lanRetry = 200*time.Millisecond, 10*time.Millisecond
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		dial []error
		gw   string
		want api.PermissionState
	}{
		{"connected", []error{nil}, "192.168.1.1", api.PermissionStateGranted},
		{"refused by the gateway", []error{syscall.ECONNREFUSED}, "192.168.1.1", api.PermissionStateGranted},
		{"no route, then allowed", []error{syscall.EHOSTUNREACH, syscall.EHOSTUNREACH, nil}, "192.168.1.1", api.PermissionStateGranted},
		{"no route", []error{syscall.EHOSTUNREACH}, "192.168.1.1", api.PermissionStateDenied},
		{"silence", []error{context.DeadlineExceeded}, "192.168.1.1", api.PermissionStateUnknown},
		{"no gateway", []error{nil}, "", api.PermissionStateUnknown},
		{"link-local gateway", []error{nil}, "169.254.1.1", api.PermissionStateGranted},
		// A VPN or an exit node as the default route: a connection needs no Local Network grant.
		{"public gateway (a VPN)", []error{nil}, "192.0.2.1", api.PermissionStateUnknown},
		{"CGNAT gateway (an exit node)", []error{nil}, "100.64.0.1", api.PermissionStateUnknown},
	} {
		sys := &fakeSys{gw: tc.gw, dial: tc.dial}
		c := stateOf(Probe(ctx, sys, ProbeOptions{Checks: []string{LocalNetwork}, Request: LocalNetwork}), LocalNetwork)
		if c.State != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, c.State, c.Detail, tc.want)
		}
	}
	// A denial keeps trying until the window ends: the alert needs a live process.
	sys := &fakeSys{gw: "192.168.1.1", dial: []error{syscall.EHOSTUNREACH}}
	Probe(ctx, sys, ProbeOptions{Checks: []string{LocalNetwork}, Request: LocalNetwork})
	if sys.dials < 5 {
		t.Errorf("%d dials in the window", sys.dials)
	}
}

func TestPasteboardRestores(t *testing.T) {
	sys := &fakeSys{clip: "the user's text"}
	c := stateOf(Probe(context.Background(), sys, ProbeOptions{Checks: []string{Pasteboard}}), Pasteboard)
	if c.State != api.PermissionStateGranted {
		t.Errorf("%+v", c)
	}
	if sys.clip != "the user's text" {
		t.Errorf("clipboard left as %q", sys.clip)
	}
}

func TestProtected(t *testing.T) {
	home := "/Users/u"
	for dir, want := range map[string]bool{
		"/Users/u/Documents":                     true,
		"/Users/u/Documents/x":                   true,
		"/Users/u/Desktop/p":                     true,
		"/Users/u/Downloads":                     true,
		"/Users/u/Library/Mobile Documents/x":    true,
		"/Volumes/Ext/p":                         true,
		"/Users/u/code/p":                        false,
		"/Users/u/DocumentsOld":                  false,
		"/Users/u/code/../Documents/p":           true,
		"/Users/other/Documents/p":               false,
		"/Users/u/Library/Application Support/x": false,
	} {
		if got := Protected(home, dir); got != want {
			t.Errorf("Protected(%q) = %v", dir, got)
		}
	}
}

// fakeRun answers the codesign, spctl and launchctl calls a check makes.
type fakeRun struct {
	reqs     map[string]string // bundle -> designated requirement
	info     map[string]string // bundle -> codesign -dv output
	broken   map[string]bool
	print    map[string]bool // launchctl print target -> fails
	disabled string          // launchctl print-disabled output
	calls    []string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	last := args[len(args)-1]
	switch {
	case name == "codesign" && args[0] == "--verify" && args[1] == "--strict":
		if f.broken[last] {
			return nil, errors.New("codesign: a sealed resource is missing or invalid")
		}
		return nil, nil
	case name == "codesign" && args[0] == "-dv":
		return []byte(f.info[last]), nil
	case name == "codesign" && args[0] == "-d":
		return []byte("Executable=x\ndesignated => " + f.reqs[last] + "\n"), nil
	case name == "codesign" && strings.HasPrefix(args[1], "-R="):
		if !SameRequirement(strings.TrimPrefix(args[1], "-R="), f.reqs[last]) {
			return nil, errors.New("does not satisfy")
		}
		return nil, nil
	case name == "spctl":
		return []byte(last + ": accepted\nsource=Notarized Developer ID\n"), nil
	case name == "launchctl" && args[0] == "print-disabled":
		return []byte(f.disabled), nil
	case name == "launchctl" && args[0] == "print":
		if f.print[last] {
			return nil, errors.New("Could not find service")
		}
		return []byte("state = running\n"), nil
	case name == "loginctl":
		return []byte("Linger=yes\n"), nil
	case name == "systemctl":
		return []byte("enabled\n"), nil
	}
	return nil, fmt.Errorf("unexpected %s %v", name, args)
}

const team = "TEAM123456"

func devID(id string) (info, req string) { return devIDOf(id, team) }

// devIDOf is a valid Developer ID signature of id by team tm.
func devIDOf(id, tm string) (info, req string) {
	return "Identifier=" + id + "\nAuthority=Developer ID Application: Someone (" + tm + ")\nAuthority=Developer ID Certification Authority\nCodeDirectory v=20500 size=38815 flags=0x10000(runtime) hashes=1206+3 location=embedded\nTeamIdentifier=" + tm + "\n",
		strings.ReplaceAll(ReleaseRequirement(id, tm), `"`+tm+`"`, tm) // codesign prints the team bare
}

func TestCheckSignature(t *testing.T) {
	ctx := context.Background()
	f := &fakeRun{reqs: map[string]string{}, info: map[string]string{}, broken: map[string]bool{}}
	f.info["/rel.app"], f.reqs["/rel.app"] = devID("p.hab-holder")
	f.info["/adhoc.app"], f.reqs["/adhoc.app"] = "Identifier=p.hab-holder.dev\nCodeDirectory v=20500 size=1 flags=0x10002(adhoc,runtime) hashes=1\n", `cdhash H"abc"`
	f.info["/odd.app"], f.reqs["/odd.app"] = devID("p.hab-holder")
	f.reqs["/odd.app"] = `identifier "p.hab-holder" and anchor apple generic`
	f.info["/noruntime.app"] = "Identifier=p.hab\nTeamIdentifier=" + team + "\nAuthority=Developer ID Application: x\nCodeDirectory v=20500 size=1 flags=0x0(none) hashes=1\n"
	f.reqs["/noruntime.app"] = "identifier x"
	f.info["/dev.app"] = "Identifier=p.hab.dev\nAuthority=Apple Development: x (Y)\nTeamIdentifier=" + team + "\nCodeDirectory v=20500 size=1 flags=0x10000(runtime) hashes=1\n"
	f.reqs["/dev.app"] = `identifier "p.hab.dev" and anchor apple generic and certificate leaf[subject.OU] = ` + team
	f.broken["/broken.app"] = true

	if si := CheckSignature(ctx, f.run, "/rel.app"); si.State != api.PermissionStateGranted || si.AdHoc || !strings.Contains(si.Detail, "notarized") || strings.Contains(si.Detail, "not notarized") {
		t.Errorf("release: %+v", si)
	}
	if si := CheckSignature(ctx, f.run, "/adhoc.app"); si.State != api.PermissionStateGranted || !si.AdHoc {
		t.Errorf("ad-hoc: %+v", si)
	}
	if si := CheckSignature(ctx, f.run, "/dev.app"); si.State != api.PermissionStateGranted || si.AdHoc || !strings.Contains(si.Detail, "dev build") {
		t.Errorf("Apple Development: %+v", si)
	}
	for _, b := range []string{"/odd.app", "/noruntime.app", "/broken.app"} {
		if si := CheckSignature(ctx, f.run, b); si.State != api.PermissionStateDenied {
			t.Errorf("%s: %+v", b, si)
		}
	}
}

// harness is a darwin Service with fakes.
type harness struct {
	t        *testing.T
	sys      *fakeSys
	run      *fakeRun
	probed   [][]string
	request  []string
	checks   map[string]api.PermissionState
	holders  []HolderInfo
	dirs     []string
	cfg      Config
	probeErr error
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, sys: &fakeSys{manager: "Aqua", open: map[string]error{}, home: "/Users/u"},
		run:    &fakeRun{reqs: map[string]string{}, info: map[string]string{}, broken: map[string]bool{}, print: map[string]bool{}},
		checks: map[string]api.PermissionState{}}
	h.run.info["/b/H.app"], h.run.reqs["/b/H.app"] = devID("p.hab-holder")
	h.run.info["/b/A.app"], h.run.reqs["/b/A.app"] = devID("p.hab")
	h.holders = []HolderInfo{{Instance: "h1", Pid: 10, Bundle: "/b/H.app"}}
	h.cfg = Config{GOOS: "darwin", OSVersion: "27.2", Run: h.run.run, Sys: h.sys, HabBundle: "/b/A.app", StateDir: t.TempDir(), UID: 501,
		Holders:     func() []HolderInfo { return h.holders },
		ProjectDirs: func() []string { return h.dirs },
		Jobs: func() []Job {
			return []Job{{Label: "p.gff-hab", Process: api.PermissionProcessHab}, {Label: "p.gff-hab-holder.h1", Process: api.PermissionProcessHolder, Instance: "h1"}}
		},
		Probe: func(_ context.Context, checks []string, request string) (string, []Check, error) {
			h.probed = append(h.probed, checks)
			h.request = append(h.request, request)
			if h.probeErr != nil {
				return "", nil, h.probeErr
			}
			var out []Check
			for _, g := range checks {
				st, ok := h.checks[g]
				if !ok {
					st = api.PermissionStateGranted
				}
				if g == LocalNetwork && request != LocalNetwork && !ok {
					st = api.PermissionStateUnknown
				}
				out = append(out, Check{Grant: g, State: st})
			}
			return "h1", out, nil
		},
	}
	return h
}

func (h *harness) check(p api.PermissionsParams) api.Permissions {
	h.t.Helper()
	res, err := New(h.cfg).Check(context.Background(), p)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func find(res api.Permissions, proc api.PermissionProcess, g string) (api.PermissionRow, bool) {
	for _, r := range res.Rows {
		if r.Process == proc && r.Grant == g && !r.Draining {
			return r, true
		}
	}
	return api.PermissionRow{}, false
}

func failing(res api.Permissions) []string {
	var out []string
	for _, r := range res.Rows {
		if r.Required && r.State != api.PermissionStateGranted {
			out = append(out, string(r.Process)+" "+r.Grant+" "+string(r.State))
		}
	}
	return out
}

func TestServiceDefaults(t *testing.T) {
	h := newHarness(t)
	res := h.check(api.PermissionsParams{})
	if res.OS != "darwin" || res.OSVersion != "27.2" {
		t.Errorf("os %q %q", res.OS, res.OSVersion)
	}
	// FDA by default (Q1), Local Network required (Q2) and unknown until a walk sees it.
	if got := failing(res); !slices.Equal(got, []string{"holder local-network unknown"}) {
		t.Errorf("failing rows: %v", got)
	}
	ax, _ := find(res, api.PermissionProcessHolder, Accessibility)
	if ax.State != api.PermissionStateOff || ax.Required {
		t.Errorf("accessibility not opted in: %+v", ax)
	}
	if !slices.Equal(h.probed[0], []string{Domain, FullDiskAccess, LocalNetwork}) {
		t.Errorf("probe checks %v: an opt-in grant that is off must not be probed", h.probed[0])
	}
	if h.request[0] != "" {
		t.Error("a check sent a request")
	}
	// gff-hab's own FDA: not required with no project in a protected folder.
	// Nor checked: a denied open would add a switched-off GFF Hab row to Settings (spec 8.1).
	if r, _ := find(res, api.PermissionProcessHab, FullDiskAccess); r.Required || r.State != api.PermissionStateNote || r.Reveal != "" {
		t.Errorf("hab FDA with no protected project: %+v", r)
	}
	h.sys.mu.Lock()
	opened := slices.Contains(h.sys.opened, FDAPath)
	h.sys.mu.Unlock()
	if opened {
		t.Error("gff-hab opened the Full Disk Access folder with no project in a protected folder")
	}
	for _, g := range []string{Domain, Signature, BackgroundItem} {
		if r, ok := find(res, api.PermissionProcessHab, g); !ok || r.State != api.PermissionStateGranted {
			t.Errorf("hab %s: %+v", g, r)
		}
	}
	if r, ok := find(res, api.PermissionProcessHolder, Signature); !ok || r.State != api.PermissionStateGranted || r.Instance != "h1" {
		t.Errorf("holder signature: %+v", r)
	}
}

func TestServiceMissingRows(t *testing.T) {
	h := newHarness(t)
	h.checks[FullDiskAccess] = api.PermissionStateDenied
	h.sys.open[FDAPath] = &fs.PathError{Err: syscall.EPERM}
	h.dirs = []string{"/Users/u/Documents/proj"}
	h.cfg.Grants = []string{Accessibility, "automation:com.apple.Terminal"}
	h.checks["automation:com.apple.Terminal"] = api.PermissionStateUnknown
	h.run.disabled = "disabled services = {\n\t\"p.gff-hab-holder.h1\" => disabled\n}\n"
	res := h.check(api.PermissionsParams{})
	want := []string{"holder full-disk-access denied", "holder local-network unknown", "holder automation:com.apple.Terminal unknown",
		"holder background-item denied", "hab full-disk-access denied"}
	if got := failing(res); !slices.Equal(got, want) {
		t.Errorf("failing rows:\n got %v\nwant %v", got, want)
	}
	fda, _ := find(res, api.PermissionProcessHolder, FullDiskAccess)
	if fda.Settings == "" || fda.Reveal != "/b/H.app" {
		t.Errorf("holder FDA row must open Settings and reveal the bundle: %+v", fda)
	}
	if r, _ := find(res, api.PermissionProcessHab, FullDiskAccess); !strings.Contains(r.Detail, "Documents") || r.Reveal != "/b/A.app" {
		t.Errorf("hab FDA: %+v", r)
	}
	// A job that is not loaded at all.
	h.run.disabled = ""
	h.run.print["gui/501/p.gff-hab"] = true
	res = h.check(api.PermissionsParams{Grants: []string{BackgroundItem}})
	if r, _ := find(res, api.PermissionProcessHab, BackgroundItem); r.State != api.PermissionStateDenied {
		t.Errorf("unloaded job: %+v", r)
	}
	if len(h.probed) != 1 {
		t.Errorf("a background check ran a probe: %v", h.probed)
	}
}

func TestServiceDropFDA(t *testing.T) {
	h := newHarness(t)
	h.cfg.Grants = []string{"no-full-disk-access"}
	h.checks[FullDiskAccess] = api.PermissionStateDenied
	h.checks[FilesAndFolders] = api.PermissionStateUnknown
	res := h.check(api.PermissionsParams{})
	if r, _ := find(res, api.PermissionProcessHolder, FullDiskAccess); r.State != api.PermissionStateOff || r.Required {
		t.Errorf("dropped FDA: %+v", r)
	}
	if r, _ := find(res, api.PermissionProcessHolder, FilesAndFolders); r.State != api.PermissionStateUnknown || !r.Required {
		t.Errorf("files: %+v", r)
	}
}

// TestServiceUnknownOptIns: an opt-in list with a typo must fail the check, not drop the grant.
func TestServiceUnknownOptIns(t *testing.T) {
	ok := []string{Accessibility, ScreenRecording, DeveloperTools, Pasteboard, "no-full-disk-access", "automation:com.apple.Terminal"}
	if bad := UnknownOptIns(ok); len(bad) != 0 {
		t.Errorf("known names reported unknown: %q", bad)
	}
	if bad := UnknownOptIns([]string{"acessibility", "automation:x", Accessibility, Domain}); !slices.Equal(bad, []string{"acessibility", "automation:x", Domain}) {
		t.Errorf("unknown %q", bad)
	}
	h := newHarness(t)
	h.checks[LocalNetwork] = api.PermissionStateGranted
	if got := failing(h.check(api.PermissionsParams{})); len(got) != 0 {
		t.Fatalf("a set-up Mac fails %v", got)
	}
	h.cfg.Grants = []string{"acessibility"}
	res := h.check(api.PermissionsParams{})
	r, found := find(res, api.PermissionProcessHab, OptIns)
	if !found || r.State != api.PermissionStateDenied || !r.Required || !strings.Contains(r.Detail, `"acessibility"`) {
		t.Errorf("a typo in the opt-in list: %+v (found %v)", r, found)
	}
	if res = h.check(api.PermissionsParams{Grants: []string{FullDiskAccess}}); len(failing(res)) != 0 {
		t.Errorf("a check of one grant reports the opt-in list: %v", failing(res))
	}
}

func TestServiceKeepsWalkAnswers(t *testing.T) {
	h := newHarness(t)
	// The walk's request sees the network; a later check reports it.
	res := h.check(api.PermissionsParams{Grants: []string{LocalNetwork}, Request: LocalNetwork})
	if r, _ := find(res, api.PermissionProcessHolder, LocalNetwork); r.State != api.PermissionStateGranted || r.Remembered {
		t.Fatalf("request: %+v", r)
	}
	if h.request[0] != LocalNetwork {
		t.Error("request not passed to the probe")
	}
	res = h.check(api.PermissionsParams{})
	if r, _ := find(res, api.PermissionProcessHolder, LocalNetwork); r.State != api.PermissionStateGranted || !strings.Contains(r.Detail, "seen by gff setup") || !r.Remembered {
		t.Errorf("kept answer: %+v", r)
	}
	if got := failing(res); len(got) != 0 {
		t.Errorf("failing %v", got)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.StateDir, "macos", "last-pass")); err != nil {
		t.Errorf("no last-pass record: %v", err)
	}
	// A build with another requirement has no answer, as TCC has no grant for it.
	h.run.info["/b/H2.app"], h.run.reqs["/b/H2.app"] = devID("q.hab-holder")
	h.holders = []HolderInfo{{Instance: "h1", Bundle: "/b/H2.app"}}
	res = h.check(api.PermissionsParams{})
	if r, _ := find(res, api.PermissionProcessHolder, LocalNetwork); r.State != api.PermissionStateUnknown {
		t.Errorf("new requirement: %+v", r)
	}
}

func TestServiceDrainingHolders(t *testing.T) {
	h := newHarness(t)
	h.run.info["/b/old/H.app"], h.run.reqs["/b/old/H.app"] = devID("p.hab-holder")
	// A valid release of another team: its own signature passes, but TCC keys its grants apart.
	h.run.info["/b/other/H.app"], h.run.reqs["/b/other/H.app"] = devIDOf("p.hab-holder", "OTHERTEAM1")
	// Two dev builds: ad-hoc, each requirement its own code hash.
	adhoc := "Identifier=p.hab-holder.dev\nCodeDirectory v=20500 size=1 flags=0x10002(adhoc,runtime) hashes=1\n"
	h.run.info["/b/adhoc1/H.app"], h.run.reqs["/b/adhoc1/H.app"] = adhoc, `cdhash H"aaaa"`
	h.run.info["/b/adhoc2/H.app"], h.run.reqs["/b/adhoc2/H.app"] = adhoc, `cdhash H"bbbb"`
	// A broken bundle fails its own signature check before any comparison.
	h.run.info["/b/broken/H.app"], h.run.reqs["/b/broken/H.app"] = devID("p.hab-holder")
	h.run.broken["/b/broken/H.app"] = true
	h.holders = append(h.holders, HolderInfo{Instance: "old", Draining: true, Bundle: "/b/old/H.app"},
		HolderInfo{Instance: "other", Draining: true, Bundle: "/b/other/H.app"},
		HolderInfo{Instance: "broken", Draining: true, Bundle: "/b/broken/H.app"})
	h.checks[LocalNetwork] = api.PermissionStateGranted
	res := h.check(api.PermissionsParams{})
	rows := map[string]api.PermissionRow{}
	for _, r := range res.Rows {
		if r.Draining {
			rows[r.Instance] = r
		}
	}
	if old := rows["old"]; old.State != api.PermissionStateGranted || !strings.Contains(old.Detail, "shares its grants") {
		t.Errorf("same requirement: %+v", old)
	}
	if other := rows["other"]; other.State != api.PermissionStateDenied || !other.Required || !strings.Contains(other.Detail, "another signing identity") {
		t.Errorf("another team: %+v", other)
	}
	if broken := rows["broken"]; broken.State != api.PermissionStateDenied || !broken.Required || !strings.Contains(broken.Detail, "broken") {
		t.Errorf("a broken bundle: %+v", broken)
	}
	if len(h.probed) != 1 {
		t.Errorf("%d probes; a draining holder takes no new pane", len(h.probed))
	}

	// The current holder an ad-hoc build; a draining one a different ad-hoc build.
	h.holders = []HolderInfo{{Instance: "h1", Bundle: "/b/adhoc1/H.app"}, {Instance: "prev", Draining: true, Bundle: "/b/adhoc2/H.app"}}
	res = h.check(api.PermissionsParams{})
	n := 0
	for _, r := range res.Rows {
		if r.Draining {
			n++
			if r.State != api.PermissionStateDenied || !strings.Contains(r.Detail, "another signing identity") {
				t.Errorf("another ad-hoc build: %+v", r)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d draining rows, want 1", n)
	}
}

func TestServiceCannotRun(t *testing.T) {
	h := newHarness(t)
	h.probeErr = fmt.Errorf("%w within 5s: no holder", ErrProbeStart)
	res := h.check(api.PermissionsParams{})
	if r, _ := find(res, api.PermissionProcessHolder, Domain); r.State != api.PermissionStateError {
		t.Errorf("probe failure: %+v", r)
	}
	h = newHarness(t)
	h.cfg.Probe = nil
	res = h.check(api.PermissionsParams{})
	if r, _ := find(res, api.PermissionProcessHolder, Domain); r.State != api.PermissionStateError {
		t.Errorf("no link: %+v", r)
	}
}

func TestServiceRegrant(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(filepath.Join(h.cfg.StateDir, "macos"), 0o700)
	rp := filepath.Join(h.cfg.StateDir, "macos", RegrantFile)
	os.WriteFile(rp, []byte("team OLD to NEW\n"), 0o600)
	// Local Network unknown: the check does not pass, so the record stays.
	res := h.check(api.PermissionsParams{})
	if r, ok := find(res, api.PermissionProcessHab, RegrantOwed); !ok || r.State != api.PermissionStateDenied || !r.Required {
		t.Fatalf("regrant row: %+v", r)
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatal("record removed by a failing check")
	}
	// Every required grant seen again: this check is the proof (6.4).
	h.checks[LocalNetwork] = api.PermissionStateGranted
	res = h.check(api.PermissionsParams{})
	if _, ok := find(res, api.PermissionProcessHab, RegrantOwed); ok {
		t.Error("regrant row in a passing check")
	}
	if _, err := os.Stat(rp); !os.IsNotExist(err) {
		t.Error("record kept after a passing check")
	}
}

func TestServiceRefuses(t *testing.T) {
	s := New(Config{GOOS: "darwin", Sys: &fakeSys{}})
	for _, p := range []api.PermissionsParams{{Grants: []string{"nope"}}, {Request: FullDiskAccess}} {
		var inv *InvalidError
		if _, err := s.Check(context.Background(), p); !errors.As(err, &inv) {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestServiceLinux(t *testing.T) {
	r := &fakeRun{}
	res, err := New(Config{GOOS: "linux", Run: r.run, LogicUnit: "p-gff-hab.service", UID: 1000}).Check(context.Background(), api.PermissionsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := failing(res); len(got) != 0 || len(res.Rows) != 2 {
		t.Errorf("rows %+v", res.Rows)
	}
	res, _ = New(Config{GOOS: "linux"}).Check(context.Background(), api.PermissionsParams{})
	if len(res.Rows) != 1 || res.Rows[0].State != api.PermissionStateNote {
		t.Errorf("no install: %+v", res.Rows)
	}
}

func TestServiceOneAtATime(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	h.cfg.Probe = func(context.Context, []string, string) (string, []Check, error) {
		started <- struct{}{}
		<-release
		return "h1", nil, nil
	}
	s := New(h.cfg)
	done := make(chan struct{})
	go func() {
		s.Check(context.Background(), api.PermissionsParams{Grants: []string{FullDiskAccess}})
		close(done)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Check(ctx, api.PermissionsParams{Grants: []string{FullDiskAccess}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second check: %v, want it to wait", err)
	}
	close(release)
	<-done
}
