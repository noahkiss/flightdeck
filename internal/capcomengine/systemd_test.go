package capcomengine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The engine's unit: its own transient unit with the environment file, the kept variables and
// the log; never a credential on the command line. Adopt reads the unit's main pid.
func TestSystemdJob(t *testing.T) {
	var calls []string
	j := SystemdJob{Unit: "fdtest-gff-hab-capcom.service", EnvFile: "/state/capcom/engine.env", Keep: []string{"PATH", "HOME"},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "systemctl" {
				return []byte(strconv.Itoa(os.Getpid()) + "\n"), nil
			}
			return nil, nil
		}}
	spec := JobSpec{Argv: []string{"/usr/bin/node", "/x/gff-hab-capcom.mjs"}, Dir: "/state/capcom/engine", Log: "/state/capcom/engine.log",
		Env: []string{"PATH=/usr/bin", "HOME=/h", "LC_ALL=C", "SECRET=x", "GFF_HAB=http+unix:///s/hooks/abc"}}
	p, err := j.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if p.Pid() != os.Getpid() {
		t.Fatalf("pid %d", p.Pid())
	}
	run := calls[0]
	for _, want := range []string{"systemd-run --user --unit=fdtest-gff-hab-capcom.service", "--property=EnvironmentFile=/state/capcom/engine.env",
		"--property=WorkingDirectory=/state/capcom/engine", "--setenv=PATH=/usr/bin", "--setenv=HOME=/h", "--setenv=LC_ALL=C",
		"-- /usr/bin/node /x/gff-hab-capcom.mjs"} {
		if !strings.Contains(run, want) {
			t.Errorf("%q lacks %q", run, want)
		}
	}
	if strings.Contains(run, "SECRET") || strings.Contains(run, "GFF_HAB=") {
		t.Fatalf("a variable outside Keep: %q", run)
	}
}
