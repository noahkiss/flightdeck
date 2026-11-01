package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/grants"
)

func r(proc api.PermissionProcess, g string, st api.PermissionState, req bool, detail string) api.PermissionRow {
	return api.PermissionRow{Process: proc, Grant: g, State: st, Required: req, Detail: detail}
}

const H, B = api.PermissionProcessHolder, api.PermissionProcessHab

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []api.PermissionRow
		want int
	}{
		{"all granted", []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateGranted, true, "")}, 0},
		{"unknown opt-in passes", []api.PermissionRow{r(H, "automation:a.b", api.PermissionStateUnknown, false, "")}, 0},
		{"off passes", []api.PermissionRow{r(H, grants.Accessibility, api.PermissionStateOff, false, "")}, 0},
		{"denied", []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, "")}, 1},
		{"unanswered", []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateUnanswered, true, "")}, 1},
		{"unknown required fails", []api.PermissionRow{r(H, grants.LocalNetwork, api.PermissionStateUnknown, true, "")}, 1},
		{"regrant owed", []api.PermissionRow{r(B, grants.RegrantOwed, api.PermissionStateDenied, true, "")}, 1},
		{"error wins", []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, ""), r(H, grants.Domain, api.PermissionStateError, true, "not Aqua")}, 2},
		{"a newer state word on a required row fails", []api.PermissionRow{r(H, grants.FullDiskAccess, "later", true, "")}, 1},
	} {
		if got, _ := exitCode(api.Permissions{Rows: tc.rows}); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPrintCheck(t *testing.T) {
	res := api.Permissions{OS: "darwin", OSVersion: "27.1", Rows: []api.PermissionRow{
		r(H, grants.Domain, api.PermissionStateGranted, true, "Aqua"),
		r(H, grants.Signature, api.PermissionStateGranted, true, "p.hab-holder, team T, Developer ID, notarized"),
		r(H, grants.FullDiskAccess, api.PermissionStateGranted, true, ""),
		r(H, grants.LocalNetwork, api.PermissionStateUnknown, true, "no check exists without a connection; gff setup makes one"),
		r(H, grants.Accessibility, api.PermissionStateOff, false, "not turned on in macos.grants"),
		{Process: H, Instance: "abc", Draining: true, Grant: grants.Signature, State: api.PermissionStateGranted, Required: true, Detail: "same identity"},
		r(B, grants.FullDiskAccess, api.PermissionStateDenied, true, "a project is in /Users/u/Documents/p"),
		r(H, grants.DevBuild, api.PermissionStateNote, false, "dev build: grants last until the next rebuild"),
	}}
	var out bytes.Buffer
	code, why := exitCode(res)
	printCheck(&out, "this Mac", res, code, why)
	got := out.String()
	for _, want := range []string{
		"host      this Mac (macOS 27.1, Aqua)\n",
		"holder    sig  ok       p.hab-holder, team T, Developer ID, notarized\n",
		"holder    fda  ok\n",
		"holder    lan  unknown  no check exists",
		"holder    ax   off      not turned on in macos.grants\n",
		"draining  sig  ok       holder abc: same identity\n",
		"hab       fda  missing  a project is in /Users/u/Documents/p\n",
		"holder    dev  note     dev build",
		"\n1 missing, 1 unknown. Run `gff setup` to fix.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "aqua") {
		t.Errorf("a granted domain row belongs on the host line:\n%s", got)
	}

	out.Reset()
	lin := api.Permissions{OS: "linux", Rows: []api.PermissionRow{r(B, grants.Lingering, api.PermissionStateGranted, true, "")}}
	code, why = exitCode(lin)
	printCheck(&out, "this host", lin, code, why)
	if !strings.Contains(out.String(), "macOS grants: not applicable") || !strings.Contains(out.String(), "Every required grant is in place.") {
		t.Errorf("linux:\n%s", out.String())
	}

	out.Reset()
	bad := api.Permissions{OS: "darwin", Rows: []api.PermissionRow{r(H, grants.Domain, api.PermissionStateError, true, "launchd domain \"Background\", not Aqua")}}
	code, why = exitCode(bad)
	printCheck(&out, "this Mac", bad, code, why)
	if code != 2 || !strings.Contains(out.String(), "The check could not run: launchd domain") {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
}

func TestParseSetup(t *testing.T) {
	for _, args := range [][]string{
		{"--grant", "nope"},
		{"--json"},
		{"--host", "h", "--socket", "/s"},
		{"--timeout", "0s"},
		{"extra"},
		{"--bogus"},
	} {
		if code := setupMain(args, strings.NewReader(""), io.Discard, io.Discard); code != exitUsage {
			t.Errorf("%q: exit %d, want 64", args, code)
		}
	}
	o, err := parseSetup([]string{"--check", "--json", "--grant", "full-disk-access", "--grant", "automation:com.apple.Terminal", "--timeout", "1m"}, io.Discard)
	if err != nil || !o.check || !o.json || len(o.grants) != 2 || o.timeout != time.Minute {
		t.Errorf("%+v %v", o, err)
	}
}

func TestNoHabExits2(t *testing.T) {
	var out bytes.Buffer
	code := setupMain([]string{"--check", "--socket", t.TempDir() + "/none.sock"}, strings.NewReader(""), &out, io.Discard)
	if code != exitCannot || !strings.Contains(out.String(), "no hab") {
		t.Errorf("exit %d: %s", code, out.String())
	}
	out.Reset()
	code = setupMain([]string{"--check", "--json", "--socket", t.TempDir() + "/none.sock"}, strings.NewReader(""), &out, io.Discard)
	var v struct {
		Exit int               `json:"exit"`
		Rows []json.RawMessage `json:"rows"`
	}
	if code != exitCannot || json.Unmarshal(out.Bytes(), &v) != nil || v.Exit != 2 || v.Rows == nil {
		t.Errorf("json exit %d: %s", code, out.String())
	}
}

// fakeHab answers hab.permissions from a table of states that a test changes.
type fakeHab struct {
	mu     sync.Mutex
	os     string
	rows   []api.PermissionRow
	calls  []api.PermissionsParams
	onCall func(n int, p api.PermissionsParams) // may change rows
	opened [][]string
}

func (f *fakeHab) call(_ context.Context, p api.PermissionsParams) (api.Permissions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	if f.onCall != nil {
		f.onCall(len(f.calls), p)
	}
	var rows []api.PermissionRow
	for _, row := range f.rows {
		if len(p.Grants) == 0 || slices.Contains(p.Grants, row.Grant) {
			rows = append(rows, row)
		}
	}
	return api.Permissions{OS: f.os, OSVersion: "27.2", Rows: rows}, nil
}

func (f *fakeHab) set(proc api.PermissionProcess, g string, st api.PermissionState) {
	for i := range f.rows {
		if f.rows[i].Process == proc && f.rows[i].Grant == g {
			f.rows[i].State = st
		}
	}
}

func (f *fakeHab) open(args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, args)
	return nil
}

func newWalker(f *fakeHab, opts setupOptions, lines chan string) (*walker, *bytes.Buffer) {
	var out bytes.Buffer
	if opts.timeout == 0 {
		opts.timeout = 2 * time.Second
	}
	return &walker{opts: opts, out: &out, where: "this Mac", local: true, call: f.call, open: f.open, lines: lines, tick: 5 * time.Millisecond}, &out
}

func TestWalkSetUpMacChangesNothing(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateGranted, true, ""),
		r(H, grants.Accessibility, api.PermissionStateOff, false, "")}}
	w, out := newWalker(f, setupOptions{}, nil)
	if code := w.run(context.Background()); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(f.opened) != 0 || len(f.calls) != 1 || f.calls[0].Request != "" {
		t.Errorf("a set-up Mac: opened %v, calls %+v (7.7)", f.opened, f.calls)
	}
}

func TestWalkFullDiskAccess(t *testing.T) {
	fda := r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, "")
	fda.Settings, fda.Reveal = grants.SettingsURL(grants.FullDiskAccess), "/state/builds/x/GFF Holder.app"
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{fda, r(B, grants.Domain, api.PermissionStateGranted, true, "Aqua")}}
	f.onCall = func(n int, p api.PermissionsParams) {
		if n == 4 { // the user turned it on while the walk waited
			f.set(H, grants.FullDiskAccess, api.PermissionStateGranted)
		}
	}
	w, out := newWalker(f, setupOptions{}, nil)
	if code := w.run(context.Background()); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	want := [][]string{{fda.Settings}, {"-R", fda.Reveal}}
	if !slices.EqualFunc(f.opened, want, slices.Equal) {
		t.Errorf("opened %v, want %v", f.opened, want)
	}
	for _, p := range f.calls[1 : len(f.calls)-1] {
		if !slices.Equal(p.Grants, []string{grants.FullDiskAccess}) || p.Request != "" {
			t.Errorf("a wait re-check sent %+v", p)
		}
	}
	if last := f.calls[len(f.calls)-1]; len(last.Grants) != 0 {
		t.Errorf("the walk must end with the full check, got %+v", last)
	}
	for _, s := range []string{"Opened System Settings > Privacy & Security > Full Disk Access", fda.Reveal, "fda: ok", "Every required grant is in place."} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestWalkRequestsFirst(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{r(H, grants.LocalNetwork, api.PermissionStateUnknown, true, "")}}
	f.onCall = func(n int, p api.PermissionsParams) {
		if p.Request == grants.LocalNetwork {
			f.set(H, grants.LocalNetwork, api.PermissionStateGranted)
		}
	}
	w, out := newWalker(f, setupOptions{}, nil)
	if code := w.run(context.Background()); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if f.calls[1].Request != grants.LocalNetwork {
		t.Errorf("second call %+v, want the local network request", f.calls[1])
	}
	if len(f.opened) != 0 {
		t.Errorf("a request that answered granted opened %v", f.opened)
	}
}

func TestWalkSkipAndTimeout(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{
		r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, ""),
		r(H, grants.Accessibility, api.PermissionStateDenied, true, ""),
	}}
	lines := make(chan string, 1)
	lines <- "s" // skips the first grant
	w, out := newWalker(f, setupOptions{timeout: 50 * time.Millisecond, noOpen: true}, lines)
	if code := w.run(context.Background()); code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, s := range []string{"fda: skipped", "ax: still missing after 50ms", "Open System Settings > Privacy & Security > Accessibility.", "2 missing. Run `gff setup` to fix."} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if len(f.opened) != 0 {
		t.Errorf("--no-open opened %v", f.opened)
	}
}

func TestWalkStops(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, ""),
		r(H, grants.Accessibility, api.PermissionStateDenied, true, "")}}
	ctx, cancel := context.WithCancel(context.Background())
	f.onCall = func(n int, _ api.PermissionsParams) {
		if n == 3 {
			cancel() // Ctrl-C while the first grant waits
		}
	}
	w, out := newWalker(f, setupOptions{}, nil)
	code := w.run(ctx)
	if code != 1 || !strings.Contains(out.String(), "Stopped.") || strings.Contains(out.String(), "holder ax: missing.\n  ") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if last := f.calls[len(f.calls)-1]; len(last.Grants) != 0 {
		t.Error("a stopped walk must still print the summary")
	}
}

func TestWalkElsewhere(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, "")}}
	w, out := newWalker(f, setupOptions{}, nil)
	w.local = false
	if code := w.run(context.Background()); code != 1 || len(f.opened) != 0 || !strings.Contains(out.String(), "Run gff setup there") {
		t.Errorf("remote walk: exit %d, opened %v\n%s", code, f.opened, out)
	}
	f = &fakeHab{os: "linux", rows: []api.PermissionRow{r(B, grants.Lingering, api.PermissionStateDenied, true, "")}}
	w, out = newWalker(f, setupOptions{}, nil)
	if code := w.run(context.Background()); code != 1 || !strings.Contains(out.String(), "loginctl enable-linger") {
		t.Errorf("linux walk: exit %d\n%s", code, out)
	}
}

func TestWalkCheckOnly(t *testing.T) {
	f := &fakeHab{os: "darwin", rows: []api.PermissionRow{r(H, grants.FullDiskAccess, api.PermissionStateDenied, true, "")}}
	w, out := newWalker(f, setupOptions{check: true, json: true}, nil)
	if code := w.run(context.Background()); code != 1 || len(f.opened) != 0 || len(f.calls) != 1 {
		t.Errorf("--check: exit %d, opened %v, %d calls", code, f.opened, len(f.calls))
	}
	var v struct {
		Exit int                 `json:"exit"`
		Rows []api.PermissionRow `json:"rows"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v.Exit != 1 || len(v.Rows) != 1 {
		t.Errorf("%v %+v", err, v)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	errHab := func(context.Context, api.PermissionsParams) (api.Permissions, error) {
		return api.Permissions{}, errors.New("method not found")
	}
	w, out = newWalker(f, setupOptions{check: true}, nil)
	w.call = errHab
	if code := w.run(context.Background()); code != 2 || !strings.Contains(out.String(), "could not run") {
		t.Errorf("an old hab: exit %d\n%s", code, out)
	}
}
