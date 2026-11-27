package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJobStates: with no install record nothing is asked; with one, each job's state comes from
// four parsed fields, and no line of the raw output, such as a path, reaches the result.
func TestJobStates(t *testing.T) {
	state := t.TempDir()
	asked := 0
	none := func(context.Context, string, ...string) ([]byte, error) { asked++; return nil, errors.New("no") }
	if js, err := JobStates(context.Background(), state, []string{"one"}, none); err != nil || js != nil || asked != 0 {
		t.Fatalf("no record: %v %v, %d calls", js, err, asked)
	}

	for _, c := range []struct {
		init Init
		out  string
		want []string
	}{
		{Launchd, "gui/501/x = {\n\tpath = /some/where/x.plist\n\tstate = running\n\tpid = 42\n\truns = 3\n\tlast exit code = 0\n}\n",
			[]string{"fd.gff-hab: loaded true, running true, last exit 0, runs 3", "fd.gff-hab-holder.one: loaded true, running true, last exit 0, runs 3"}},
		{Systemd, "LoadState=loaded\nActiveState=failed\nExecMainStatus=2\nNRestarts=5\nFragmentPath=/some/where\n",
			[]string{"fd-gff-hab.service: loaded true, running false, last exit 2, runs 5", "fd-gff-hab-holder@one.service: loaded true, running false, last exit 2, runs 5"}},
	} {
		rec := `{"version":1,"init":"` + string(c.init) + `","prefix":"fd","unit_dir":"/u","files":[],"dirs":[]}`
		if err := os.WriteFile(filepath.Join(state, "install.json"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		run := func(_ context.Context, name string, args ...string) ([]byte, error) { return []byte(c.out), nil }
		js, err := JobStates(context.Background(), state, []string{"one"}, run)
		if err != nil || len(js) != 2 {
			t.Fatalf("%s: %v %v", c.init, js, err)
		}
		for i, j := range js {
			if got := j.String(); got != c.want[i] || strings.Contains(got, "/some/where") {
				t.Errorf("%s: %q, want %q", c.init, got, c.want[i])
			}
		}
	}
}
