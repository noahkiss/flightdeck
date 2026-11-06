package gffclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/hosts"
)

// TestTabRenameCloseLast: tab mode renames a tab (the bar shows the name), goes back to the
// last tab with Tab, and closes a tab: its views close, their programs keep running in the
// sidebar. Closing the only tab leaves an empty tab. The workspace's name shows beside the
// clock.
func TestTabRenameCloseLast(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.AddSession("job")
	a.AddSession("other")
	hst := newHost(t, 140, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Workspace = "devbox" }, specsOf(a.Sock)...)
	hst.waitScreen("the rows and the workspace name", func(s string) bool {
		top := strings.Split(s, "\n")[0]
		return line(s, "   job") > 0 && line(s, "   other") > 0 && strings.Contains(top, "1/1 hosts · devbox · ")
	})
	hst.typ("\r")
	hst.waitScreen("job in tab 1", func(s string) bool { return strings.Contains(s, " job · one") })

	hst.typ(tabKey + "r")
	hst.waitScreen("the rename prompt", func(s string) bool { return strings.Contains(s, "rename tab 1") })
	hst.typ("\x7fwork  space\r")
	hst.waitScreen("the new name", func(s string) bool { return strings.Contains(s, " 1:work space ") })

	hst.typ(tabKey + "n")
	hst.waitScreen("tab 2", func(s string) bool { return strings.Contains(s, " 2:2 ") && strings.Contains(s, "empty tab") })
	hst.typ("j\r")
	hst.waitScreen("other in tab 2", func(s string) bool {
		return strings.Contains(s, " other · one") && !strings.Contains(s, " job · one")
	})

	hst.typ(tabKey + "\t")
	hst.waitScreen("the last tab: 1", func(s string) bool {
		return strings.Contains(s, " job · one") && !strings.Contains(s, " other · one")
	})
	hst.typ("\t") // tab mode stays after a tab move
	hst.waitScreen("the last tab: 2", func(s string) bool { return strings.Contains(s, " other · one") })
	hst.typ("\x1b")

	hst.typ(tabKey + "x")
	hst.waitScreen("tab 2 closed, other still in the sidebar", func(s string) bool {
		return !strings.Contains(s, " 2:2 ") && strings.Contains(s, " 1:work space ") && strings.Contains(s, " job · one") &&
			!strings.Contains(s, " other · one") && line(s, "other") > 0
	})
	hst.typ(tabKey + "x")
	hst.waitScreen("the only tab closed: an empty tab", func(s string) bool {
		return strings.Contains(s, " 1:1 ") && strings.Contains(s, "empty tab") && line(s, "job") > 0 && line(s, "other") > 0
	})
	if n := a.PaneCloses(); n != 0 {
		t.Fatalf("%d pane.close calls: closing a tab ends no program", n)
	}
}

// specsOf is the hosts of startWorkspace: the sockets labelled one, two, ... in order.
func specsOf(socks ...string) []hosts.Spec {
	var specs []hosts.Spec
	for i, sock := range socks {
		specs = append(specs, hosts.Spec{Label: []string{"one", "two", "three"}[i],
			Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: sock}}})
	}
	return specs
}

// TestWorkspaceConfig: [workspace] name sets the workspace's name; a bad name or an unknown key
// is a problem on its line and the default stays.
func TestWorkspaceConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, problems, err := LoadSettings(path)
	if err != nil || s.Workspace.Name != DefaultWorkspace || len(problems) != 0 {
		t.Fatalf("no file: %+v %v %v", s.Workspace, problems, err)
	}
	write("[workspace]\nname = \"work-2\"\n")
	if s, problems, _ = LoadSettings(path); s.Workspace.Name != "work-2" || len(problems) != 0 {
		t.Fatalf("a name: %+v %v", s.Workspace, problems)
	}
	write("[workspace]\nname = \"../x\"\ncolour = 1\n")
	s, problems, _ = LoadSettings(path)
	if s.Workspace.Name != DefaultWorkspace || len(problems) != 2 ||
		!strings.Contains(problems[0], ":3: [workspace] colour: unknown key") || !strings.Contains(problems[1], ":2: [workspace] name: want a name") {
		t.Fatalf("bad lines: %+v %q", s.Workspace, problems)
	}
	for name, ok := range map[string]bool{"default": true, "a.b_c-1": true, ".hidden": false, "": false, "a b": false, strings.Repeat("x", 65): false} {
		if ValidWorkspaceName(name) != ok {
			t.Errorf("ValidWorkspaceName(%q) = %v", name, !ok)
		}
	}
}

// TestCloseTabDuringStart keeps the started session in the sidebar after its tab closes.
func TestCloseTabDuringStart(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("host online", func(s string) bool { return strings.Contains(s, "1/1 hosts") })
	release := a.HoldCreates()
	defer release()
	hst.typ("n")
	hst.waitScreen("directory prompt", func(s string) bool { return strings.Contains(s, "new session") })
	hst.typ("\r")
	hst.waitScreen("command prompt", func(s string) bool { return strings.Contains(s, "command on one") })
	hst.typ("\r")
	hst.waitScreen("starting", func(s string) bool { return strings.Contains(s, "starting on one") })
	hst.typ(tabKey + "x")
	hst.waitScreen("closed", func(s string) bool { return strings.Contains(s, "empty tab") })
	release()
	hst.waitScreen("session in sidebar", func(s string) bool { return line(s, "sh") > 0 && strings.Contains(s, "started after its tab closed") })
	if !strings.Contains(hst.screen(), "empty tab") {
		t.Fatal("start reopened the closed tab")
	}
}
