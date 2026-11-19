package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdOutputFiles: both units append their output to the files launchd's jobs write, in
// the log directory, through a shell that makes the folder first, with '%' and '$' escaped and
// the holder's instance as %i (diagnostics.md 4.3).
func TestSystemdOutputFiles(t *testing.T) {
	c := testConfig(Systemd, "/srv/fd%test")
	logs := strings.ReplaceAll(c.LogDir(), "%", "%%")
	script := systemdQuote(outputScript, true)
	u := string(holderUnit(c))
	want := `ExecStart="/bin/sh" "-c" ` + script + ` "` + filepath.Join(logs, "gff-hab-holder.%i.log") + `" "` +
		strings.ReplaceAll(c.HolderPath(), "%", "%%") + `"`
	if !strings.Contains(u, want) || strings.Contains(u, "StandardOutput") {
		t.Errorf("holder unit, want %s:\n%s", want, u)
	}
	l := string(logicUnit(c))
	want = `ExecStart="/bin/sh" "-c" ` + script + ` "` + filepath.Join(logs, "hab.stderr") + `" "` +
		strings.ReplaceAll(c.HabPath(), "%", "%%") + `"`
	if !strings.Contains(l, want) || strings.Contains(l, "StandardOutput") {
		t.Errorf("logic unit, want %s:\n%s", want, l)
	}
	if !strings.Contains(script, "$${0%%/*}") {
		t.Errorf("script not escaped: %s", script)
	}
}
