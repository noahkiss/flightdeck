package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Defaults for session.create (hab-logic-api.md 3.9).
const (
	defaultCols = 80
	defaultRows = 24
)

// actions serves session.create and pane.close over the holder link and the structure store.
type actions struct {
	sp      *spawner
	store   *structure.Store
	getenv  func(string) string
	environ func() []string
	home    string // the default working directory
	// refresh asks for a read of a project's repository state (internal/gitstatus); nil for none.
	refresh func(project string)
}

var _ control.Actions = (*actions)(nil)

// CreateSession starts a program in a new pane through the spawner, then files the pane as a
// session. It checks every input before it spawns. If filing fails after the spawn, it closes
// the pane again, so no unfiled pane is left behind.
func (a *actions) CreateSession(ctx context.Context, p api.SessionCreateParams) (string, string, error) {
	if p.Harness != "" {
		return "", "", control.Errorf(api.ErrorKindUnavailable,
			"session.create with a harness needs that harness's adapter; Part 4 stage 4 (harness adapters) builds it. Give argv instead")
	}
	cols, rows := p.Cols, p.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	if cols < 1 || rows < 1 || cols > 0xffff || rows > 0xffff {
		return "", "", control.Errorf(api.ErrorKindInvalid, "size %dx%d: cols and rows must be 1 to 65535", cols, rows)
	}
	argv := p.Argv
	if len(argv) == 0 {
		sh := a.getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		argv = []string{sh}
	}
	if argv[0] == "" {
		return "", "", control.Errorf(api.ErrorKindInvalid, "argv[0] is empty")
	}
	env, err := mergeEnv(a.environ(), p.Env)
	if err != nil {
		return "", "", err
	}

	project, projectTop := p.Project, ""
	switch {
	case p.Project != "" && p.ProjectDir != "":
		return "", "", control.Errorf(api.ErrorKindInvalid, "give project or project_dir, not both")
	case p.Project != "":
		if _, ok := a.store.Project(p.Project); !ok {
			return "", "", control.Errorf(api.ErrorKindNotFound, "project %s does not exist", p.Project)
		}
	case p.ProjectDir != "":
		if !filepath.IsAbs(p.ProjectDir) {
			return "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %q is not an absolute path", p.ProjectDir)
		}
		if project, projectTop, err = a.projectFor(ctx, p.ProjectDir); err != nil {
			return "", "", err
		}
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = p.ProjectDir
	}
	if cwd == "" {
		cwd = a.home
	}
	if !filepath.IsAbs(cwd) {
		return "", "", control.Errorf(api.ErrorKindInvalid, "cwd %q is not an absolute path", cwd)
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", "", control.Errorf(api.ErrorKindInvalid, "cwd %q is not a directory", cwd)
	}

	name := p.Name
	if name == "" {
		name = a.freeName(filepath.Base(argv[0]))
	} else if _, taken := a.store.SessionByName(name); taken {
		return "", "", control.Errorf(api.ErrorKindConflict, "a session named %q exists", name)
	}

	st, err := a.sp.spawn(ctx, hp.Spawn{Cols: uint16(cols), Rows: uint16(rows), Cwd: cwd, Argv: argv, Env: env})
	if err != nil {
		if errors.Is(err, holderclient.ErrNoHolder) {
			return "", "", control.Errorf(api.ErrorKindUnavailable, "no holder is running on this host")
		}
		return "", "", control.Errorf(api.ErrorKindUnavailable, "spawn: %v", err)
	}
	pane := st.UUID.String()
	ss, err := a.store.CreateSession(structure.SessionSpec{Name: name, Project: project, Pane: pane, Index: -1})
	if err != nil {
		if cerr := a.sp.lk.ClosePane(ctx, st.UUID); cerr != nil {
			err = fmt.Errorf("%w; closing the new pane %s also failed: %v", err, pane, cerr)
		}
		return "", "", storeErr(err)
	}
	a.noteDir(ctx, project, projectTop, p.Project != "" && p.Cwd != "", cwd)
	return ss.ID, pane, nil
}

// noteDir records the repository folder a new session works in as its project's most recently
// used directory, then asks for a read of the project's repository state. top is the folder
// project_dir resolved to. With a project given by id and a cwd, the cwd's repository counts
// when it has the project's identity.
func (a *actions) noteDir(ctx context.Context, project, top string, checkCwd bool, cwd string) {
	if project == "" {
		return
	}
	if top == "" && checkCwd {
		t, remote, root, err := gitIdentity(ctx, cwd)
		if err == nil {
			if pr, ok := a.store.ProjectByIdentity(remote, root); ok && pr.ID == project {
				top = t
			}
		}
	}
	if top != "" {
		_ = a.store.AddProjectDir(project, top) // the project may be gone meanwhile
	}
	if a.refresh != nil {
		a.refresh(project)
	}
}

var _ control.SessionRecords = (*actions)(nil)

// RenameSession renames a session record (session.rename). A name another session holds is a
// conflict.
func (a *actions) RenameSession(session, name string) error {
	if _, err := a.store.RenameSession(session, name); err != nil {
		return storeErr(err)
	}
	return nil
}

// CloseSession removes a session record (session.close). A live pane keeps running, unfiled
// (hab-logic-api.md 2.2).
func (a *actions) CloseSession(session string) error {
	if err := a.store.CloseSession(session); err != nil {
		return storeErr(err)
	}
	return nil
}

// ClosePane releases a pane through the link (hab-logic-api.md 3.9).
func (a *actions) ClosePane(ctx context.Context, pane string) error {
	u, ok := parseUUID(pane)
	if !ok {
		return control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane)
	}
	err := a.sp.lk.ClosePane(ctx, u)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, link.ErrNoPane):
		return control.Errorf(api.ErrorKindNotFound, "pane %s does not exist", pane)
	case errors.Is(err, link.ErrLost):
		return control.Errorf(api.ErrorKindUnavailable, "pane %s is lost: its holder is not connected", pane)
	case errors.Is(err, link.ErrReleasing):
		return control.Errorf(api.ErrorKindConflict, "pane %s is being released already", pane)
	}
	return control.Errorf(api.ErrorKindUnavailable, "close pane %s: %v", pane, err)
}

// freeName returns base, or base-2, base-3 and so on: the first name no session holds.
func (a *actions) freeName(base string) string {
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	name := base
	for i := 2; ; i++ {
		if _, taken := a.store.SessionByName(name); !taken {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// projectFor finds the project of the git repository that holds dir, by its remote, else its root
// commit, and creates it when it is new. The new project takes the repository folder's name. It
// also returns the repository's top folder.
func (a *actions) projectFor(ctx context.Context, dir string) (id, top string, err error) {
	top, remote, root, err := gitIdentity(ctx, dir)
	if err != nil {
		return "", "", err
	}
	if pr, ok := a.store.ProjectByIdentity(remote, root); ok {
		return pr.ID, top, nil
	}
	pr, err := a.store.CreateProject(filepath.Base(top), remote, root)
	if errors.Is(err, structure.ErrConflict) { // created meanwhile by another call
		if pr, ok := a.store.ProjectByIdentity(remote, root); ok {
			return pr.ID, top, nil
		}
	}
	if err != nil {
		return "", "", storeErr(err)
	}
	return pr.ID, top, nil
}

// gitIdentity reads the identity of the repository that holds dir: its top folder, the remote
// (origin, else the first remote) and, with no remote, the root commit.
func gitIdentity(ctx context.Context, dir string) (top, remote, root string, err error) {
	top, err = git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %s is not inside a git repository", dir)
	}
	if remote, err = git(ctx, dir, "remote", "get-url", "origin"); err != nil {
		remote = ""
		if list, err := git(ctx, dir, "remote"); err == nil && list != "" {
			first, _, _ := strings.Cut(list, "\n")
			remote, _ = git(ctx, dir, "remote", "get-url", first)
		}
	}
	if remote != "" {
		return top, remote, "", nil
	}
	roots, err := git(ctx, dir, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || roots == "" {
		return "", "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %s: the repository has no remote and no commit", dir)
	}
	list := strings.Fields(roots)
	slices.Sort(list) // a repository with several roots: pick one the same way every time
	return top, "", list[0], nil
}

// git runs git in dir and returns its trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// mergeEnv returns base with each KEY=value of extra added; an extra entry replaces the base
// entry of the same key.
func mergeEnv(base, extra []string) ([]string, error) {
	keys := map[string]bool{}
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, control.Errorf(api.ErrorKindInvalid, "env entry %q is not KEY=value", kv)
		}
		keys[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...), nil
}

// parseUUID reads a pane id: 32 hex digits, with or without the dashes.
func parseUUID(s string) (hp.UUID, bool) {
	var u hp.UUID
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != len(u) {
		return u, false
	}
	copy(u[:], b)
	return u, true
}

// storeErr maps a structure store error to its API kind.
func storeErr(err error) error {
	for _, k := range []struct {
		err  error
		kind api.ErrorKind
	}{
		{structure.ErrNotFound, api.ErrorKindNotFound},
		{structure.ErrConflict, api.ErrorKindConflict},
		{structure.ErrInvalid, api.ErrorKindInvalid},
	} {
		if errors.Is(err, k.err) {
			return control.Errorf(k.kind, "%v", err)
		}
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", err)
}
