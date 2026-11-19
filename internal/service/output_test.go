package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdOutputFiles: both units append their output to the files launchd's jobs write, in
// the log directory, with '%' escaped and the holder's instance as %i (diagnostics.md 4.3).
func TestSystemdOutputFiles(t *testing.T) {
	c := testConfig(Systemd, "/srv/fd%test")
	logs := strings.ReplaceAll(c.LogDir(), "%", "%%")
	u := string(holderUnit(c))
	want := "append:" + filepath.Join(logs, "gff-hab-holder.%i.log") + "\n"
	if !strings.Contains(u, "StandardOutput="+want) || !strings.Contains(u, "StandardError="+want) {
		t.Errorf("holder unit, want %q:\n%s", want, u)
	}
	l := string(logicUnit(c))
	want = "append:" + filepath.Join(logs, "hab.stderr") + "\n"
	if !strings.Contains(l, "StandardOutput="+want) || !strings.Contains(l, "StandardError="+want) {
		t.Errorf("logic unit, want %q:\n%s", want, l)
	}
}
