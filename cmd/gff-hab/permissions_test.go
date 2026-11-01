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
	hide := newHiddenPanes()
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
