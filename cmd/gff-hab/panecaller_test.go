package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestPaneCallerDirect: a process inside a real pane clears its environment, so GFF_HAB is
// gone, and dials the control socket itself. It still cannot call a person-only method or say
// hello as a hab; other calls work. An orphan that lost the pane as its parent is caught by the
// pane's session. The test process, outside every pane, calls the same method (hab-logic-api.md
// 4.1).
func TestPaneCallerDirect(t *testing.T) {
	t.Parallel()
	e := newHolderEnv(t, 0)
	e.serve()
	c := e.control()
	helper := filepath.Join(e.work, "panecaller")
	if out, err := exec.Command("go", "build", "-o", helper, "./testdata/panecaller").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	const want = "hello ok\ndevices.list denied\nstructure.get ok\nhello-hab denied\n"
	for _, mode := range []string{"direct", "orphan"} {
		out := filepath.Join(e.work, mode+".out")
		var res api.SessionCreated
		if err := c.Call(ctx, "session.create", api.SessionCreateParams{Argv: []string{helper, mode, e.sock, out}, Cwd: e.work, Cols: 80, Rows: 24}, &res); err != nil {
			t.Fatal(err)
		}
		var got []byte
		waitUntil(t, mode+": the pane process's answers", func() bool {
			var err error
			got, err = os.ReadFile(out)
			return err == nil
		})
		if string(got) != want {
			t.Errorf("%s: the pane process got\n%s\nwant\n%s", mode, got, want)
		}
	}
	var list api.DeviceList
	if err := c.Call(ctx, "devices.list", api.DevicesListParams{}, &list); err != nil {
		t.Fatalf("devices.list outside every pane: %v", err)
	}
}
