package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/worktree"
)

// teardownTime bounds a worktree's teardown script.
const teardownTime = 2 * time.Minute

// defaultWorktreeRoot is where new worktrees go, under the hab's home, unless -worktrees or
// $GFF_HAB_WORKTREES says otherwise (hab-logic-api.md 3.10).
func defaultWorktreeRoot(home string) string {
	return filepath.Join(home, ".gff", "worktrees")
}

// worktreeRoot picks the folder for new worktrees: the flag, else $GFF_HAB_WORKTREES, else the
// default. It must be absolute; a leading ~/ means the home folder.
func worktreeRoot(flagValue string, getenv func(string) string, home string) (string, error) {
	root := flagValue
	if root == "" {
		root = getenv("GFF_HAB_WORKTREES")
	}
	if root == "" {
		return defaultWorktreeRoot(home), nil
	}
	if rest, ok := strings.CutPrefix(root, "~/"); ok {
		root = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("worktree folder %q is not an absolute path", root)
	}
	return filepath.Clean(root), nil
}

// worktrees serves the worktree.* methods (hab-logic-api.md 3.10) over internal/worktree and the
// session actions: a setup script runs as a session in the holder.
type worktrees struct {
	a    *actions
	git  worktree.Git
	root string // absolute folder for new worktrees
	now  func() time.Time
	logf func(format string, args ...any)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per project: one create, setup or remove at a time
}

var _ control.Worktrees = (*worktrees)(nil)

func newWorktrees(a *actions, root string, logf func(string, ...any)) *worktrees {
	return &worktrees{a: a, root: root, now: time.Now, logf: logf, locks: map[string]*sync.Mutex{}}
}

// lock holds the project's lock until the returned function runs.
func (w *worktrees) lock(project string) func() {
	w.mu.Lock()
	l := w.locks[project]
	if l == nil {
		l = &sync.Mutex{}
		w.locks[project] = l
	}
	w.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// resolve finds the project and a folder in its repository: project_dir, or the project's most
// recently used folder on this host that still exists.
func (w *worktrees) resolve(ctx context.Context, t api.WorktreeProject) (project, dir string, err error) {
	if t.ProjectDir != "" {
		if !filepath.IsAbs(t.ProjectDir) {
			return "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %q is not an absolute path", t.ProjectDir)
		}
		project, top, err := w.a.projectFor(ctx, t.ProjectDir)
		if err != nil {
			return "", "", err
		}
		return project, top, nil
	}
	if _, ok := w.a.store.Project(t.Project); !ok {
		return "", "", control.Errorf(api.ErrorKindNotFound, "project %s does not exist", t.Project)
	}
	for _, pd := range w.a.store.ProjectDirs() {
		if pd.ID != t.Project {
			continue
		}
		for _, d := range pd.Dirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				return t.Project, d, nil
			}
		}
	}
	return "", "", control.Errorf(api.ErrorKindUnavailable,
		"project %s has no folder on this host yet; give project_dir", t.Project)
}

// ListWorktrees lists the repository's working trees (worktree.list).
func (w *worktrees) ListWorktrees(ctx context.Context, p api.WorktreeListParams) (api.WorktreeList, error) {
	project, dir, err := w.resolve(ctx, p.WorktreeProject)
	if err != nil {
		return api.WorktreeList{}, err
	}
	list, err := w.git.List(ctx, dir)
	if err != nil {
		return api.WorktreeList{}, worktreeErr(err)
	}
	out := api.WorktreeList{Project: project, Worktrees: make([]api.Worktree, 0, len(list))}
	for _, wt := range list {
		out.Worktrees = append(out.Worktrees, w.toAPI(ctx, wt))
	}
	return out, nil
}

func (w *worktrees) toAPI(ctx context.Context, wt worktree.Worktree) api.Worktree {
	out := api.Worktree{Path: wt.Path, Branch: wt.Branch, Head: wt.Head, Main: wt.Main, Locked: wt.Locked, Prunable: wt.Prunable}
	if !wt.Main && !wt.Prunable {
		var mark worktree.Mark
		mark, out.Managed, _ = w.git.ReadMark(ctx, wt.Path)
		if out.Managed && !mark.Approved && mark.Foreign != "" {
			cfg, err := worktree.ReadConfig(wt.Path)
			out.SetupPending = err == nil && cfg.Setup != ""
		}
	}
	return out
}

// CreateWorktree makes a worktree, copies the included files, and starts its setup script unless
// the code may come from someone else (worktree.create).
func (w *worktrees) CreateWorktree(ctx context.Context, client string, p api.WorktreeCreateParams) (api.WorktreeCreated, error) {
	project, dir, err := w.resolve(ctx, p.WorktreeProject)
	if err != nil {
		return api.WorktreeCreated{}, err
	}
	pr, _ := w.a.store.Project(project)
	defer w.lock(project)()

	c, err := w.git.Create(ctx, worktree.CreateOptions{
		Source: dir, Root: w.root, Branch: p.Branch, Base: p.Base, Path: p.Path, Remote: pr.Remote,
	})
	if err != nil {
		return api.WorktreeCreated{}, worktreeErr(err)
	}
	mode := p.Setup
	if mode == "" {
		mode = "auto"
	}
	mark := worktree.Mark{
		Source:   c.Source,
		Created:  w.now().UTC(),
		Foreign:  c.Foreign,
		Approved: c.Foreign == "" || mode == "run",
	}
	out := api.WorktreeCreated{Project: project, Copied: c.Copied}
	cfg, cfgErr := worktree.ReadConfig(c.Worktree.Path)
	if cfgErr == nil && mark.Approved {
		mark.Digest = cfg.Digest() // the approval covers these scripts only
	}
	switch {
	case cfgErr != nil:
		mark.Approved = false
		out.Setup = api.WorktreeSetup{State: "error", Reason: cfgErr.Error()}
	case cfg.Setup == "":
		out.Setup = api.WorktreeSetup{State: "none"}
	case mode == "skip":
		out.Setup = api.WorktreeSetup{State: "skipped"}
	case !mark.Approved:
		out.Setup = api.WorktreeSetup{State: "pending",
			Reason:         c.Foreign + "; worktree.setup runs it once the user says yes",
			Script:         cfg.Setup,
			TeardownScript: cfg.Teardown}
	default:
		out.Setup = w.startSetup(ctx, project, c.Worktree, c.Source, cfg.Setup)
		if out.Setup.State == "started" {
			mark.SetupRun = w.now().UTC()
		}
	}
	if err := w.git.WriteMark(ctx, c.Worktree.Path, mark); err != nil {
		w.logf("worktree %s: write its mark: %v", c.Worktree.Path, err)
	}
	out.Worktree = w.toAPI(ctx, c.Worktree)
	return out, nil
}

// SetupWorktree starts a linked worktree's setup script now (worktree.setup). The call is the
// user's yes: the worktree's scripts may run from now on, teardown included.
func (w *worktrees) SetupWorktree(ctx context.Context, client string, p api.WorktreeSetupParams) (api.WorktreeSetup, error) {
	project, dir, err := w.resolve(ctx, p.WorktreeProject)
	if err != nil {
		return api.WorktreeSetup{}, err
	}
	defer w.lock(project)()
	wt, err := w.git.Find(ctx, dir, p.Path)
	if err != nil {
		return api.WorktreeSetup{}, worktreeErr(err)
	}
	if wt.Main {
		return api.WorktreeSetup{}, control.Errorf(api.ErrorKindInvalid, "%s is the main working tree; setup runs in linked worktrees", wt.Path)
	}
	if wt.Prunable {
		return api.WorktreeSetup{}, control.Errorf(api.ErrorKindNotFound, "the folder of %s is gone", wt.Path)
	}
	cfg, err := worktree.ReadConfig(wt.Path)
	if err != nil {
		return api.WorktreeSetup{State: "error", Reason: err.Error()}, nil
	}
	mark, ok, err := w.git.ReadMark(ctx, wt.Path)
	if err != nil {
		w.logf("worktree %s: read its mark: %v", wt.Path, err)
	}
	if !ok {
		mark = worktree.Mark{Source: w.mainPath(ctx, dir), Created: w.now().UTC()}
	}
	mark.Approved, mark.Digest = true, cfg.Digest() // the yes covers the scripts as they are now
	res := api.WorktreeSetup{State: "none"}
	if cfg.Setup != "" {
		res = w.startSetup(ctx, project, wt, mark.Source, cfg.Setup)
		if res.State == "started" {
			mark.SetupRun = w.now().UTC()
		}
	}
	if err := w.git.WriteMark(ctx, wt.Path, mark); err != nil {
		w.logf("worktree %s: write its mark: %v", wt.Path, err)
	}
	return res, nil
}

// mainPath returns the repository's main working tree, or dir when it cannot be read.
func (w *worktrees) mainPath(ctx context.Context, dir string) string {
	if list, err := w.git.List(ctx, dir); err == nil && len(list) > 0 && list[0].Main {
		return list[0].Path
	}
	return dir
}

// startSetup runs a setup script as a session of the project, in the worktree, so it outlives a
// gff-hab restart and its output shows in the terminal view.
func (w *worktrees) startSetup(ctx context.Context, project string, wt worktree.Worktree, source, script string) api.WorktreeSetup {
	session, pane, err := w.a.CreateSession(ctx, api.SessionCreateParams{
		Name:    w.a.freeName("setup-" + filepath.Base(wt.Path)),
		Project: project,
		Argv:    worktree.ScriptArgv(script),
		Cwd:     wt.Path,
		Env:     worktree.Env(source, wt.Path, wt.Branch),
	})
	if err != nil {
		return api.WorktreeSetup{State: "error", Reason: err.Error()}
	}
	return api.WorktreeSetup{State: "started", Session: session, Pane: pane}
}

// RemoveWorktree runs the teardown script of a worktree this hab made and approved, then removes
// the worktree (worktree.remove). Uncommitted or untracked changes need force. The branch stays.
func (w *worktrees) RemoveWorktree(ctx context.Context, p api.WorktreeRemoveParams) (api.WorktreeRemoved, error) {
	project, dir, err := w.resolve(ctx, p.WorktreeProject)
	if err != nil {
		return api.WorktreeRemoved{}, err
	}
	defer w.lock(project)()
	if canonDir(dir) == canonDir(p.Path) {
		dir = w.mainPath(ctx, dir) // git runs from a folder that stays
	}
	wt, err := w.git.CheckRemove(ctx, dir, p.Path, p.Force)
	if err != nil {
		return api.WorktreeRemoved{}, worktreeErr(err)
	}
	out := api.WorktreeRemoved{Teardown: "none"}
	if !wt.Prunable {
		mark, ok, _ := w.git.ReadMark(ctx, wt.Path)
		out.Managed = ok
		cfg, cfgErr := worktree.ReadConfig(wt.Path)
		ran := false
		switch {
		case cfgErr != nil:
			out.Teardown, out.TeardownOutput = "failed", cfgErr.Error()
		case cfg.Teardown == "":
		case !ok || !mark.Approved:
			out.Teardown, out.TeardownOutput = "skipped", "the user never said yes to this worktree's scripts"
		case !mark.Allows(cfg):
			out.Teardown, out.TeardownOutput = "skipped", "the scripts changed since the user said yes"
		default:
			ran = true
			tctx, cancel := context.WithTimeout(ctx, teardownTime)
			code, tail, err := worktree.RunScript(tctx, wt.Path, cfg.Teardown, worktree.Env(mark.Source, wt.Path, wt.Branch))
			cancel()
			switch {
			case err != nil:
				out.Teardown, out.TeardownOutput = "failed", tail+"\n"+err.Error()
			case code != 0:
				out.Teardown, out.TeardownExit, out.TeardownOutput = "failed", &code, tail
			default:
				out.Teardown, out.TeardownExit = "ran", &code
			}
			if out.Teardown == "failed" {
				w.logf("worktree %s: teardown failed; removing it anyway", wt.Path)
			}
		}
		if out.Teardown == "skipped" {
			w.logf("worktree %s: teardown skipped: %s", wt.Path, out.TeardownOutput)
		}
		// The worktree was clean before teardown; a teardown that left files (a log, say) would
		// make git refuse halfway. Say so now, with the worktree whole.
		if ran && !p.Force {
			files, err := w.git.DirtyFiles(ctx, wt.Path)
			if err != nil {
				return api.WorktreeRemoved{}, worktreeErr(err)
			}
			if len(files) > 0 {
				if len(files) > 5 {
					files = append(files[:5], "...")
				}
				return api.WorktreeRemoved{}, control.Errorf(api.ErrorKindConflict,
					"the teardown script %s and left uncommitted or untracked files in %s (%s); the worktree stays; remove it with force",
					teardownWord(out.Teardown), wt.Path, strings.Join(files, ", "))
			}
		}
	}
	if err := w.git.Remove(ctx, dir, wt, p.Force); err != nil {
		return api.WorktreeRemoved{}, worktreeErr(err)
	}
	if w.a.refresh != nil {
		w.a.refresh(project)
	}
	return out, nil
}

// teardownWord says how a teardown ended, for an error message.
func teardownWord(s api.TeardownState) string {
	if s == "ran" {
		return "ran"
	}
	return "failed"
}

// worktreeCmd runs `gff-hab worktree list|create|setup|remove`: one worktree.* call, its result
// printed as one JSON line.
func worktreeCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("worktree needs list, create, setup or remove")
	}
	verb := args[0]
	fs := flag.NewFlagSet("worktree "+verb, flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	project := fs.String("project", "", "project id")
	dir := fs.String("dir", "", "a folder in the repository, on the hab's host (default: the current folder, without -project or -via)")
	timeout := fs.Duration("timeout", 6*time.Minute, "give up after this long")
	var branch, base, path, setup *string
	var force *bool
	switch verb {
	case "list":
	case "create":
		branch = fs.String("branch", "", "branch to check out or make (default: a new wt/<word>-<word>-<hex>)")
		base = fs.String("base", "", "where a new branch starts (default: the source checkout's HEAD)")
		path = fs.String("path", "", "folder for the worktree (default: <worktree root>/<repository>/<branch>)")
		setup = fs.String("setup", "auto", "auto, run or skip the setup script")
	case "setup":
	case "remove":
		force = fs.Bool("force", false, "remove it even with uncommitted or untracked changes, which are lost")
	default:
		return fmt.Errorf("worktree %s: want list, create, setup or remove", verb)
	}
	fs.Parse(args[1:])
	target := api.WorktreeProject{Project: *project, ProjectDir: *dir}
	if target.Project == "" && target.ProjectDir == "" {
		if *via != "" {
			return errors.New("give -project or -dir with -via")
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		target.ProjectDir = wd
	}
	// A worktree path from the command line may be relative to this folder.
	wtPath := func() (string, error) {
		if fs.NArg() != 1 {
			return "", fmt.Errorf("worktree %s needs the worktree's folder", verb)
		}
		p := fs.Arg(0)
		if !filepath.IsAbs(p) && *via == "" {
			return filepath.Abs(p)
		}
		return p, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	var res any
	switch verb {
	case "list":
		res, err = cl.WorktreeList(ctx, api.WorktreeListParams{WorktreeProject: target})
	case "create":
		res, err = cl.WorktreeCreate(ctx, api.WorktreeCreateParams{
			WorktreeProject: target, Branch: *branch, Base: *base, Path: *path, Setup: api.SetupMode(*setup),
		})
	case "setup":
		p, perr := wtPath()
		if perr != nil {
			return perr
		}
		res, err = cl.WorktreeSetup(ctx, api.WorktreeSetupParams{WorktreeProject: target, Path: p})
	case "remove":
		p, perr := wtPath()
		if perr != nil {
			return perr
		}
		res, err = cl.WorktreeRemove(ctx, api.WorktreeRemoveParams{WorktreeProject: target, Path: p, Force: *force})
	}
	if err != nil {
		return err
	}
	return printJSON(res)
}

// canonDir resolves symbolic links in a folder path, for comparing two paths.
func canonDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// worktreeErr maps an internal/worktree error to its API kind.
func worktreeErr(err error) error {
	var ce *control.Error
	if errors.As(err, &ce) {
		return err
	}
	for _, k := range []struct {
		err  error
		kind api.ErrorKind
	}{
		{worktree.ErrNotFound, api.ErrorKindNotFound},
		{worktree.ErrConflict, api.ErrorKindConflict},
		{worktree.ErrInvalid, api.ErrorKindInvalid},
	} {
		if errors.Is(err, k.err) {
			return control.Errorf(k.kind, "%v", err)
		}
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", fmt.Errorf("git: %w", err))
}
