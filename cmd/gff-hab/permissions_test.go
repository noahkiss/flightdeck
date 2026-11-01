package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/grants"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestProbePane runs probes in real panes of a real holder: the result line comes back through
// the pane's bytes, the pane is released after, and the structure never lists it.
func TestProbePane(t *testing.T) {
	d := shortDir(t)
	hdir, state := filepath.Join(d, "h"), filepath.Join(d, "state")
	os.MkdirAll(hdir, 0o700)
	startHolder(t, hdir, "one")
	lk, err := openLinkIn(hdir, state, linkOptions{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	if err := lk.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	hide := newHiddenPanes("", t.Logf)
	hs := hidingHolders{inner: lk.Structure(), hide: hide}
	var notices []structure.Notice
	noticed := make(chan struct{}, 16)
	stop := hs.Subscribe(func(n structure.Notice) {
		notices = append(notices, n)
		noticed <- struct{}{}
	})
	defer stop()
	env := func() []string { return []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep} }

	// The real probe. On Linux every macOS check answers "not applicable", as an error; on a Mac
	// a test holder started from a shell is not in the Aqua domain unless the shell is.
	pr := &prober{lk: lk, hide: hide, argv: []string{bin}, env: env, logf: t.Logf}
	inst, checks, err := pr.run(ctx, []string{grants.Domain, grants.FullDiskAccess}, "")
	if err != nil {
		t.Fatal(err)
	}
	if inst != "one" || len(checks) != 2 || checks[0].Grant != grants.Domain || checks[1].Grant != grants.FullDiskAccess || checks[1].State == "" {
		t.Fatalf("instance %q, checks %+v", inst, checks)
	}
	if runtime.GOOS != "darwin" && (checks[0].State != api.PermissionStateError || !strings.Contains(checks[0].Detail, "not applicable")) {
		t.Errorf("domain off a Mac: %+v", checks[0])
	}

	// A probe that prints no result: an error, and the pane goes.
	pr.argv = []string{"/bin/sh", "-c", "echo not a result; sleep 0.3"}
	seen := make(chan bool, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		ps, _ := hs.Panes()
		raw, _ := lk.Structure().Panes()
		seen <- len(ps) == 0 && len(raw) == 1
	}()
	if _, _, err := pr.run(ctx, []string{grants.Domain}, ""); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Errorf("no result: %v", err)
	}
	if !<-seen {
		t.Error("while it ran, the probe pane was not hidden from the structure (or not followed by the link)")
	}
	for _, st := range lk.Panes() {
		if !st.Gone && !st.Lost {
			t.Errorf("pane %s left behind", st.UUID)
		}
	}
	select {
	case <-noticed:
		t.Errorf("the structure heard of a probe pane: %+v", notices)
	case <-time.After(200 * time.Millisecond):
	}

	// No holder: the probe cannot start.
	empty := filepath.Join(d, "none")
	os.MkdirAll(empty, 0o700)
	lk2, err := openLinkIn(empty, filepath.Join(d, "state2"), linkOptions{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer lk2.Close()
	pr2 := &prober{lk: lk2, hide: hide, argv: []string{bin}, env: env, logf: t.Logf}
	if _, _, err := pr2.run(ctx, []string{grants.Domain}, ""); err == nil || !strings.Contains(err.Error(), grants.ErrProbeStart.Error()) {
		t.Errorf("no holder: %v", err)
	}
}

var _ probeLink = (*link.Link)(nil)

// TestProbePaneAfterRestart: gff-hab restarts while a probe pane runs. The holder keeps the pane;
// the new gff-hab must still hide it, and close it.
func TestProbePaneAfterRestart(t *testing.T) {
	d := shortDir(t)
	hdir, state := filepath.Join(d, "h"), filepath.Join(d, "state")
	os.MkdirAll(hdir, 0o700)
	startHolder(t, hdir, "one")
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	list := filepath.Join(state, "macos", probePanesFile)

	lk, err := openLinkIn(hdir, state, linkOptions{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := lk.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	hide := newHiddenPanes(list, t.Logf)
	u := newUUID()
	hide.add(u.String()) // as prober.run does before its spawn
	if _, err := lk.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/", Argv: []string{"/bin/sh", "-c", "sleep 60"},
		Env: []string{"PATH=" + os.Getenv("PATH")}}); err != nil {
		t.Fatal(err)
	}
	lk.Close() // gff-hab ends mid-probe; the holder keeps the pane

	lk2, err := openLinkIn(hdir, state, linkOptions{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer lk2.Close()
	if err := lk2.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	hide2 := newHiddenPanes(list, t.Logf)
	hide2.close = linkCloser(lk2)
	hs := hidingHolders{inner: lk2.Structure(), hide: hide2}
	ps, err := hs.Panes()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.ID == u.String() {
			t.Fatal("after a restart the structure lists the probe pane")
		}
	}
	for {
		raw, _ := lk2.Structure().Panes()
		_, err := os.Stat(list)
		if !slices.ContainsFunc(raw, func(p structure.HolderPane) bool { return p.ID == u.String() }) && os.IsNotExist(err) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the probe pane left from before was not closed (still listed, or still in %s)", probePanesFile)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestHiddenPanesRetryClose: a probe pane whose release failed (the holder connection dropped)
// stays hidden, and is closed when the holder lists it again.
func TestHiddenPanesRetryClose(t *testing.T) {
	list := filepath.Join(t.TempDir(), "macos", probePanesFile)
	hide := newHiddenPanes(list, t.Logf)
	closed := make(chan string, 4)
	hide.close = func(_ context.Context, id string) error { closed <- id; return nil }
	hide.add("p1")
	hide.add("p2")
	hide.done("p1", false) // the release failed
	hide.done("p2", true)
	fake := structure.NewFakeHolder(structure.HolderPane{ID: "p1"}, structure.HolderPane{ID: "p2"}, structure.HolderPane{ID: "user"})
	hs := hidingHolders{inner: fake, hide: hide}
	ps, err := hs.Panes()
	if err != nil || len(ps) != 1 || ps[0].ID != "user" {
		t.Fatalf("panes %+v, %v", ps, err)
	}
	select {
	case id := <-closed:
		if id != "p1" {
			t.Errorf("closed %s, want p1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a probe pane whose release failed was not closed when seen again")
	}
	select {
	case id := <-closed:
		t.Errorf("closed %s again; it was released", id)
	case <-time.After(50 * time.Millisecond):
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(list); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(list)
			t.Fatalf("%s still lists %q", probePanesFile, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A notice that the pane is gone also ends it; a notice for a user pane passes.
	var got []string
	stop := hs.Subscribe(func(n structure.Notice) { got = append(got, n.Pane.ID) })
	defer stop()
	hide.add("p3")
	fake.Add(structure.HolderPane{ID: "p3"}) // a probe runs in it: not closed from here
	hide.done("p3", false)
	fake.Remove("p3")
	fake.Add(structure.HolderPane{ID: "user2"})
	if len(got) != 1 || got[0] != "user2" {
		t.Errorf("notices %v, want only user2", got)
	}
	if b, err := os.ReadFile(list); err == nil {
		t.Errorf("a gone pane is still listed: %q", b)
	}
}

// TestPermissionsCall calls hab.permissions on a served hab. On Linux the rows are the systemd
// ones; with no init system there is one note, and the call passes.
func TestPermissionsCall(t *testing.T) {
	d := shortDir(t)
	hdir, state := filepath.Join(d, "h"), filepath.Join(d, "state")
	os.MkdirAll(hdir, 0o700)
	sock := filepath.Join(d, "run", "hab.sock")
	startHolder(t, hdir, "one")
	startServeWith(t, sock, state, nil, "-holders", hdir)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "permissions") {
		t.Errorf("capabilities %v lack permissions", w.Capabilities)
	}
	var res api.Permissions
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "hab.permissions")), &res); err != nil {
		t.Fatal(err)
	}
	if res.OS == "linux" {
		if len(res.Rows) != 1 || res.Rows[0].State != api.PermissionStateNote {
			t.Errorf("linux rows %+v", res.Rows)
		}
	}
	if kind := callErr(t, sock, "hab.permissions", api.PermissionsParams{Grants: []string{"nope"}}); kind != "invalid" {
		t.Errorf("an unknown grant: %s, want invalid", kind)
	}
	if kind := callErr(t, sock, "hab.permissions", api.PermissionsParams{Request: grants.FullDiskAccess}); kind != "invalid" {
		t.Errorf("a grant no API requests: %s, want invalid", kind)
	}
}

func TestProbeCmdUsage(t *testing.T) {
	if code := probeCmd([]string{"-checks", "domain"}); code != 64 {
		t.Errorf("no nonce: exit %d", code)
	}
}

func TestMacosGrants(t *testing.T) {
	got := macosGrants(func(string) string { return "accessibility, automation:com.apple.Terminal,,screen-recording" })
	if !slices.Equal(got, []string{"accessibility", "automation:com.apple.Terminal", "screen-recording"}) {
		t.Errorf("%q", got)
	}
}
