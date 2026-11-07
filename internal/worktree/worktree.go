// Package worktree creates, lists and removes git worktrees for a project, copies the ignored
// files a project's .worktreeinclude names into a new worktree, and reads the setup and teardown
// scripts from its flightdeck.json (docs/specs/hab-logic-api.md 3.10). It runs the git binary;
// gff-hab decides when setup may run and runs it in a pane.
//
// It never removes a worktree that has uncommitted or untracked files unless the caller forces
// it, never removes the main working tree, and never deletes a branch.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Errors a caller maps to API error kinds. Each returned error wraps one of them, or none for a
// git failure.
var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Git runs the git binary. The zero value runs "git" from PATH with the process environment.
type Git struct {
	Bin string   // "" means "git" from PATH
	Env []string // entries added after the process environment; a later entry wins
}

// Worktree is one working tree of a repository, as `git worktree list --porcelain` reports it.
type Worktree struct {
	Path     string
	Branch   string // short name; "" on a detached HEAD
	Head     string // full commit id; "" in an empty repository
	Main     bool   // the repository's main working tree
	Locked   bool
	Prunable bool // git reports the folder gone
}

// gitError is a failed git run with its stderr.
type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), msg)
}

// exitCode returns the exit code of a failed git run, or -1 for another error.
func exitCode(err error) int {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.code
	}
	return -1
}

func (g Git) cmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat"), g.Env...)
	return cmd
}

// run runs git in dir and returns its stdout with the trailing newline cut.
func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.runIn(ctx, dir, nil, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// runIn runs git in dir with stdin and returns its raw stdout.
func (g Git) runIn(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := g.cmd(ctx, dir, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), &gitError{args: args, code: ee.ExitCode(), stderr: errb.String()}
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// Top returns the top folder of the working tree that holds dir.
func (g Git) Top(ctx context.Context, dir string) (string, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", ErrInvalid, dir)
	}
	top, err := g.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if exitCode(err) > 0 {
			return "", fmt.Errorf("%w: %s is not inside a git working tree", ErrInvalid, dir)
		}
		return "", err
	}
	return top, nil
}

// List returns the working trees of the repository that holds dir, the main one first. A bare
// repository's own entry is left out.
func (g Git) List(ctx context.Context, dir string) ([]Worktree, error) {
	if _, err := g.Top(ctx, dir); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseList(out), nil
}

// parseList reads `git worktree list --porcelain`: blocks split by blank lines, the first block
// the main working tree.
func parseList(out string) []Worktree {
	var list []Worktree
	first := true
	for _, block := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var w Worktree
		bare := false
		for _, line := range strings.Split(block, "\n") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				w.Path = val
			case "HEAD":
				if strings.Trim(val, "0") != "" {
					w.Head = val
				}
			case "branch":
				w.Branch = strings.TrimPrefix(val, "refs/heads/")
			case "bare":
				bare = true
			case "locked":
				w.Locked = true
			case "prunable":
				w.Prunable = true
			}
		}
		w.Main = first
		first = false
		if !bare && w.Path != "" {
			list = append(list, w)
		}
	}
	return list
}

// Find returns the working tree of the repository that holds repo whose folder is path.
// Folders compare after resolving symbolic links, so /tmp and /private/tmp match on macOS.
func (g Git) Find(ctx context.Context, repo, path string) (Worktree, error) {
	list, err := g.List(ctx, repo)
	if err != nil {
		return Worktree{}, err
	}
	want := canon(path)
	for _, w := range list {
		if canon(w.Path) == want {
			return w, nil
		}
	}
	return Worktree{}, fmt.Errorf("%w: %s is not a worktree of this repository", ErrNotFound, path)
}

// canon resolves symbolic links in p, or cleans it when it does not exist.
func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	// A missing folder: resolve its nearest existing parent.
	dir, base := filepath.Split(filepath.Clean(p))
	if dir != "" && dir != p {
		if r, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
			return filepath.Join(r, base)
		}
	}
	return filepath.Clean(p)
}

// Dirty reports whether the working tree at path has changes git would lose on removal: modified,
// staged or untracked files. Ignored files do not count.
func (g Git) Dirty(ctx context.Context, path string) (bool, error) {
	files, err := g.DirtyFiles(ctx, path)
	return len(files) > 0, err
}

// DirtyFiles lists what Dirty counts, one `git status --porcelain` line each.
func (g Git) DirtyFiles(ctx context.Context, path string) ([]string, error) {
	out, err := g.run(ctx, path, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// GitDir returns the absolute private git folder of the working tree at path: .git/worktrees/<name>
// for a linked worktree.
func (g Git) GitDir(ctx context.Context, path string) (string, error) {
	return g.run(ctx, path, "rev-parse", "--absolute-git-dir")
}

// Resolve returns the full commit id rev names in the repository that holds dir.
func (g Git) Resolve(ctx context.Context, dir, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("%w: %q is not a revision", ErrInvalid, rev)
	}
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		if exitCode(err) > 0 {
			return "", fmt.Errorf("%w: no commit %s in %s", ErrNotFound, rev, dir)
		}
		return "", err
	}
	return out, nil
}

// IsAncestor reports whether commit is rev or an ancestor of it, in the repository that holds
// dir.
func (g Git) IsAncestor(ctx context.Context, dir, commit, rev string) bool {
	return g.contains(ctx, dir, commit, rev)
}
