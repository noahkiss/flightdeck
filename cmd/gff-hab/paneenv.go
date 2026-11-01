package main

import (
	"strings"
)

// initVars are set by the init system for gff-hab's own job. A program in a pane is not that
// job, so they are not passed on.
var initVars = []string{
	"XPC_SERVICE_NAME", // launchd: the job's label
	"INVOCATION_ID",    // systemd: the unit's run
	"JOURNAL_STREAM",   // systemd: stdout and stderr go to the journal
	"SYSTEMD_EXEC_PID", // systemd: gff-hab's own pid
}

// baseTERM and baseCOLORTERM describe the terminal a pane is: the parser emulates xterm with
// 24-bit color.
const (
	baseTERM      = "xterm-256color"
	baseCOLORTERM = "truecolor"
)

// defaultLocale is the locale a pane gets when gff-hab has none: a UTF-8 locale every system of
// that kind has.
func defaultLocale(goos string) string {
	if goos == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}

// basePaneEnv is the environment every pane starts from, before the request's own entries: gff-
// hab's environment without the init system's job variables, with TERM and COLORTERM always set
// to the emulator's, and a UTF-8 LANG when no locale variable is set. Under an init system gff-hab's
// environment is bare: PATH and the locale come from install (internal/service), TERM from here.
func basePaneEnv(environ []string, goos string) []string {
	out := make([]string, 0, len(environ)+3)
	set := map[string]bool{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if isInitVar(k) {
			continue
		}
		out = append(out, kv)
		if v != "" {
			set[k] = true
		}
	}
	// The pane's terminal is gff-hab's own emulator, never the one gff-hab was started from, so
	// TERM and COLORTERM are always replaced. A request's env still wins (mergeEnv).
	out = dropKey(dropKey(out, "TERM"), "COLORTERM")
	out = append(out, "TERM="+baseTERM, "COLORTERM="+baseCOLORTERM)
	if !set["LANG"] && !set["LC_ALL"] && !set["LC_CTYPE"] {
		out = dropKey(out, "LANG")
		out = append(out, "LANG="+defaultLocale(goos))
	}
	return out
}

func isInitVar(k string) bool {
	for _, v := range initVars {
		if k == v {
			return true
		}
	}
	return false
}

// dropKey returns env without its entries for k.
func dropKey(env []string, k string) []string {
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return out
}
