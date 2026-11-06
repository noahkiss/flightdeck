package gffclient

// The saved workspace (gff-client.md 5.1, 5.6): gff writes the workspace's tabs, their layout
// trees, the hab pane and session each view shows, and the focus to a local file, and the next
// start restores them. A new workspace starts with the config's startup tabs.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/noahkiss/flightdeck/api"
)

// StartupTab is one tab a new workspace starts with: a name, and optionally a program to start
// in it on a host, in a folder.
type StartupTab struct {
	Name    string
	Host    string // a host label; empty: the first host
	Dir     string // an absolute folder on the host; empty: the hab's default
	Command string // run under /bin/sh -c; empty: the hab's shell. No dir and no command: an empty tab
}

// WorkspacePath is the file that keeps workspace name: $GFF_WORKSPACE_DIR/<name>.json, else
// gff/workspaces/<name>.json under the user config directory on macOS and under
// $XDG_STATE_HOME (or ~/.local/state) elsewhere.
func WorkspacePath(name string) (string, error) {
	return workspacePath(name, runtime.GOOS, os.Getenv, os.UserHomeDir)
}

func workspacePath(name, goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if !ValidWorkspaceName(name) {
		return "", fmt.Errorf("workspace name %q: use 1 to 64 letters, digits, dots, dashes and underscores", name)
	}
	if d := getenv("GFF_WORKSPACE_DIR"); d != "" {
		return filepath.Join(d, name+".json"), nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	base := ""
	switch {
	case goos == "darwin":
		base = filepath.Join(h, "Library", "Application Support")
	case getenv("XDG_STATE_HOME") != "":
		base = getenv("XDG_STATE_HOME")
	default:
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "gff", "workspaces", name+".json"), nil
}

type savedWorkspace struct {
	Version int        `json:"version"`
	Current int        `json:"current"`
	Tabs    []savedTab `json:"tabs"`
}

type savedTab struct {
	Name   string     `json:"name"`
	Split  string     `json:"split"`
	Zoom   bool       `json:"zoom,omitempty"`
	Focus  int        `json:"focus"`
	Layout *savedNode `json:"layout,omitempty"`
}

// savedNode is one node of a tab's tree: a view, a split (split names its direction) or a
// stack.
type savedNode struct {
	View  *savedView   `json:"view,omitempty"`
	Split string       `json:"split,omitempty"`
	Stack bool         `json:"stack,omitempty"`
	Open  int          `json:"open,omitempty"`
	Kids  []*savedNode `json:"kids,omitempty"`
}

type savedView struct {
	Host    string `json:"host"`
	Pane    string `json:"pane"`
	Session string `json:"session,omitempty"`
	Label   string `json:"label,omitempty"`
}

func splitWord(s Split) string {
	if s == SplitRows {
		return "rows"
	}
	return "columns"
}

func splitOf(w string) Split {
	if w == "rows" {
		return SplitRows
	}
	return SplitColumns
}

// snapshot is the workspace as the file keeps it.
func (c *client) snapshot() savedWorkspace {
	var save func(n *node) *savedNode
	save = func(n *node) *savedNode {
		switch n.kind {
		case leafNode:
			return &savedNode{View: &savedView{Host: n.v.h.label, Pane: n.v.spec.ID, Session: n.v.spec.Session, Label: n.v.spec.Label}}
		case stackNode:
			sn := &savedNode{Stack: true, Open: n.open}
			for _, k := range n.kids {
				sn.Kids = append(sn.Kids, save(k))
			}
			return sn
		}
		sn := &savedNode{Split: splitWord(n.dir)}
		for _, k := range n.kids {
			sn.Kids = append(sn.Kids, save(k))
		}
		return sn
	}
	w := savedWorkspace{Version: 1, Current: c.cur}
	for _, t := range c.tabs {
		t.sync()
		st := savedTab{Name: t.name, Split: splitWord(t.split), Zoom: t.zoom, Focus: t.focus}
		if t.root != nil {
			st.Layout = save(t.root)
		}
		w.Tabs = append(w.Tabs, st)
	}
	return w
}

// saveWorkspace writes the workspace file when the workspace changed since the last write. A
// failure shows once on the bottom row and is logged.
func (c *client) saveWorkspace() {
	if c.cfg.StatePath == "" || !c.ws || !c.restored {
		return
	}
	b, err := json.MarshalIndent(c.snapshot(), "", " ")
	if err != nil || string(b) == string(c.savedBytes) {
		return
	}
	if err := writeFileAtomic(c.cfg.StatePath, append(b, '\n')); err != nil {
		if c.saveErr == "" {
			c.ui.msg = "save the workspace: " + err.Error()
		}
		c.saveErr = err.Error()
		c.cfg.Logf("save the workspace: %v", err)
		return
	}
	c.savedBytes, c.saveErr = b, ""
}

// writeFileAtomic writes data to path through a temporary file and a rename, mode 0600, making
// the folder 0700 when it is missing.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// restoreWorkspace builds the tabs from the workspace file. existed reports a workspace that
// was saved; a new one gets the startup tabs instead. writable is false when the file could
// not be read for another reason than its absence: gff then never writes over it. A file that
// does not parse is set aside as <file>.bad and the workspace starts new. A view whose host is
// no longer configured is left out.
func (c *client) restoreWorkspace() (existed, writable bool, err error) {
	b, err := os.ReadFile(c.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, true, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read the workspace: %w; it will not be saved", err)
	}
	var w savedWorkspace
	if err := json.Unmarshal(b, &w); err != nil || w.Version != 1 {
		if err := os.Rename(c.cfg.StatePath, c.cfg.StatePath+".bad"); err != nil {
			return false, false, fmt.Errorf("the workspace file does not read and cannot be set aside: %w", err)
		}
		return false, true, fmt.Errorf("the workspace file did not read; set aside as %s.bad", filepath.Base(c.cfg.StatePath))
	}
	var tabs []*tab
	for _, st := range w.Tabs {
		t := &tab{name: st.Name, split: splitOf(st.Split), zoom: st.Zoom}
		if t.name == "" {
			t.name = fmt.Sprint(len(tabs) + 1)
		}
		t.root = c.buildNode(st.Layout, nil)
		t.focus = st.Focus
		t.relist(nil)
		t.focus = min(max(st.Focus, 0), max(len(t.views)-1, 0))
		if len(t.views) < 2 {
			t.zoom = false
		}
		tabs = append(tabs, t)
	}
	if len(tabs) > 0 {
		c.tabs = tabs
		c.cur = min(max(w.Current, 0), len(tabs)-1)
	}
	if b, err := json.MarshalIndent(c.snapshot(), "", " "); err == nil {
		c.savedBytes = b
	}
	return true, true, nil
}

// buildNode builds a tree from the file's, dropping views of hosts gff no longer has and nodes
// left empty; a split or stack left with one child gives way to it.
func (c *client) buildNode(sn *savedNode, up *node) *node {
	if sn == nil {
		return nil
	}
	if sv := sn.View; sv != nil {
		hs := c.hosts[sv.Host]
		if hs == nil || sv.Pane == "" {
			return nil
		}
		label := sv.Label
		if label == "" {
			label = sv.Pane
		}
		v := &view{spec: PaneSpec{ID: sv.Pane, Label: label, Host: sv.Host, Session: sv.Session}, h: hs}
		c.startInput(v)
		return &node{kind: leafNode, v: v, up: up}
	}
	n := &node{kind: splitNode, dir: splitOf(sn.Split), up: up}
	if sn.Stack {
		n.kind = stackNode
	}
	for _, k := range sn.Kids {
		kid := c.buildNode(k, n)
		if kid == nil || n.kind == stackNode && kid.kind != leafNode {
			continue
		}
		n.kids = append(n.kids, kid)
	}
	switch len(n.kids) {
	case 0:
		return nil
	case 1:
		n.kids[0].up = up
		return n.kids[0]
	}
	n.open = min(max(sn.Open, 0), len(n.kids)-1)
	return n
}

// pendingStart is a startup tab's program, waiting for its host to come online.
type pendingStart struct {
	t     *tab
	host  string
	p     api.SessionCreateParams
	label string
}

// startupTabs makes the config's startup tabs in a new workspace, and queues their programs.
func (c *client) startupTabs() {
	if len(c.cfg.StartupTabs) == 0 {
		return
	}
	c.tabs = nil
	for _, st := range c.cfg.StartupTabs {
		t := &tab{name: st.Name, split: c.cfg.Split}
		if t.name == "" {
			t.name = c.freeTabName(len(c.tabs) + 1)
		}
		c.tabs = append(c.tabs, t)
		if st.Dir == "" && st.Command == "" {
			continue
		}
		host := st.Host
		if host == "" {
			host = c.first
		}
		p := api.SessionCreateParams{Cwd: st.Dir}
		label := "shell"
		if st.Command != "" {
			p.Argv = []string{"/bin/sh", "-c", st.Command}
			label = st.Command
		}
		c.ui.pending = append(c.ui.pending, pendingStart{t: t, host: host, p: p, label: label})
	}
	c.cur = 0
}

// runPending starts each waiting startup program whose host is online. One on a host gff does
// not have says so on the bottom row.
func (u *workspaceUI) runPending() {
	c := u.c
	var left []pendingStart
	for _, ps := range u.pending {
		hm, ok := c.cfg.Hosts.Host(ps.host)
		switch {
		case !ok:
			u.msg = "startup tab " + ps.t.name + ": no host " + ps.host
		case hm.Online():
			u.startIn(ps.t, ps.host, ps.p, ps.label, nil, placeEnd)
		default:
			left = append(left, ps)
		}
	}
	u.pending = left
}
