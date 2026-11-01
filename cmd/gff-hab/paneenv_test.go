package main

import (
	"slices"
	"testing"
)

// TestBasePaneEnv: under an init system gff-hab's environment has no TERM and no locale, and
// carries the job's own variables. A pane gets TERM, COLORTERM and a UTF-8 locale, and none of
// the job's variables. The locale the user's environment sets stays; TERM and COLORTERM are
// always the emulator's, even when gff-hab runs inside another terminal (-init none).
func TestBasePaneEnv(t *testing.T) {
	job := []string{"PATH=/usr/bin:/bin", "HOME=/srv/user", "XPC_SERVICE_NAME=x.gff-hab",
		"INVOCATION_ID=abc", "JOURNAL_STREAM=8:9", "SYSTEMD_EXEC_PID=7", "TERM="}
	for goos, lang := range map[string]string{"darwin": "en_US.UTF-8", "linux": "C.UTF-8"} {
		got := basePaneEnv(slices.Clone(job), goos)
		want := []string{"PATH=/usr/bin:/bin", "HOME=/srv/user", "TERM=xterm-256color", "COLORTERM=truecolor", "LANG=" + lang}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %q\nwant %q", goos, got, want)
		}
	}

	user := []string{"TERM=screen-256color", "COLORTERM=24bit", "LC_ALL=xx_YY.UTF-8", "LC_TIME=zz_ZZ.UTF-8"}
	want := []string{"LC_ALL=xx_YY.UTF-8", "LC_TIME=zz_ZZ.UTF-8", "TERM=xterm-256color", "COLORTERM=truecolor"}
	if got := basePaneEnv(slices.Clone(user), "linux"); !slices.Equal(got, want) {
		t.Errorf("outer terminal's environment: got %q\nwant %q", got, want)
	}
	if got := basePaneEnv([]string{"LANG=xx_YY.UTF-8"}, "darwin"); slices.Contains(got, "LANG=en_US.UTF-8") ||
		!slices.Contains(got, "LANG=xx_YY.UTF-8") {
		t.Errorf("LANG replaced: %q", got)
	}
}

// TestPaneEnvRequestWins: the request's own entries replace the base ones (session.create env).
func TestPaneEnvRequestWins(t *testing.T) {
	got, err := mergeEnv(basePaneEnv(nil, "linux"), []string{"TERM=dumb"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "TERM=dumb") || slices.Contains(got, "TERM=xterm-256color") {
		t.Errorf("env: %q", got)
	}
}
