package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var execCommand = exec.CommandContext

// pruneOne drops the entry of the one worktree whose folder is path from the repository's
// worktree list: its private folder under <common dir>/worktrees. Unlike git worktree prune, it
// leaves other entries alone, such as a worktree on a volume that is not mounted now.
func (g Git) pruneOne(ctx context.Context, repo, path string) error {
	common, err := g.run(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return err
	}
	want := canon(path)
	for _, e := range entries {
		dir := filepath.Join(common, "worktrees", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "gitdir"))
		if err != nil {
			continue
		}
		if canon(filepath.Dir(strings.TrimSpace(string(b)))) == want {
			return os.RemoveAll(dir)
		}
	}
	return fmt.Errorf("%w: no worktree entry for %s", ErrNotFound, path)
}

// CheckRemove finds the worktree at path in the repository that holds repo and says whether it
// may be removed. The main working tree and a locked worktree never may; a worktree with
// uncommitted or untracked files may only with force. A worktree whose folder is gone always may.
func (g Git) CheckRemove(ctx context.Context, repo, path string, force bool) (Worktree, error) {
	w, err := g.Find(ctx, repo, path)
	if err != nil {
		return Worktree{}, err
	}
	switch {
	case w.Main:
		return w, fmt.Errorf("%w: %s is the repository's main working tree; it is never removed", ErrInvalid, w.Path)
	case w.Locked:
		return w, fmt.Errorf("%w: %s is locked; unlock it with git worktree unlock first", ErrConflict, w.Path)
	}
	if _, err := os.Stat(w.Path); errors.Is(err, fs.ErrNotExist) {
		return w, nil
	}
	if !force {
		dirty, err := g.Dirty(ctx, w.Path)
		if err != nil {
			return w, err
		}
		if dirty {
			return w, fmt.Errorf("%w: %s has uncommitted or untracked changes; force removes them", ErrConflict, w.Path)
		}
	}
	return w, nil
}

// Remove removes the linked worktree w (from CheckRemove) of the repository that holds repo. Its
// branch stays. A worktree whose folder is gone is pruned. Without force, git refuses a worktree
// that changed since the check.
func (g Git) Remove(ctx context.Context, repo string, w Worktree, force bool) error {
	if w.Main {
		return fmt.Errorf("%w: %s is the repository's main working tree", ErrInvalid, w.Path)
	}
	// Run git from the main working tree: repo may be the worktree that goes.
	if list, err := g.List(ctx, repo); err == nil && len(list) > 0 && list[0].Main {
		if _, err := os.Stat(list[0].Path); err == nil {
			repo = list[0].Path
		}
	}
	if _, err := os.Stat(w.Path); errors.Is(err, fs.ErrNotExist) {
		return g.pruneOne(ctx, repo, w.Path)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := g.run(ctx, repo, append(args, "--", w.Path)...); err != nil {
		if !force && exitCode(err) > 0 {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return err
	}
	return nil
}
