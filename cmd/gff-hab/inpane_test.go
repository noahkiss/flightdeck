package main

import (
	"io"
	"strings"
	"testing"
)

// Inside a pane ($GFF_HAB set), the verbs that act as the person without the socket's check, or
// that would run a command to reach a hab, refuse before they act (hab-logic-api.md 4.1).
func TestVerbsInPane(t *testing.T) {
	t.Setenv("GFF_HAB", "/tmp/hook.sock#token")
	if _, err := open("", "true"); err == nil || !strings.Contains(err.Error(), "-via") {
		t.Errorf("-via in a pane: %v", err)
	}
	if err := pairCmd(nil, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "pane") {
		t.Errorf("pair in a pane: %v", err)
	}
	if err := peersCmd([]string{"accept", "desk"}, io.Discard); err == nil || !strings.Contains(err.Error(), "pane") {
		t.Errorf("peers accept in a pane: %v", err)
	}
	if err := devicesCmd([]string{"revoke", "-state", t.TempDir(), "desk"}, io.Discard, func(k string) string {
		if k == "GFF_HAB" {
			return "/tmp/hook.sock#token"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "pane") {
		t.Errorf("devices revoke in a pane: %v", err)
	}
	for _, verb := range []func() error{
		func() error { return install([]string{"-prefix", "x"}, io.Discard) },
		func() error { return uninstall([]string{"-prefix", "x", "-yes"}, strings.NewReader(""), io.Discard) },
	} {
		if err := verb(); err == nil || !strings.Contains(err.Error(), "pane") {
			t.Errorf("install or uninstall in a pane: %v", err)
		}
	}
}
