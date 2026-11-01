package gitstatus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Change is one entry of git status: a path relative to the repository's top folder, in slash
// form, and how it changed.
type Change struct {
	Path string
	// Kind is added, modified, deleted, renamed or untracked.
	Kind string
	// Staged says the change is in the index only; HEAD has no version of an added file.
	Staged bool
}

// maxStatus bounds the git status output read; past it the list is cut.
const maxStatus = 1 << 20

// Changes reads git status of the repository that holds dir: its top folder and the changed
// files, untracked ones included. A dir outside a repository gives "" and no changes. truncated
// says the output was cut at its bound.
func (r Reader) Changes(ctx context.Context, dir string) (top string, changes []Change, truncated bool, err error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", nil, false, nil
	}
	top, err = r.git(ctx, dir, "rev-parse", "--show-toplevel")
	if errors.Is(err, errNotRepo) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	out, truncated, err := r.gitRaw(ctx, top, maxStatus, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return "", nil, false, err
	}
	fields := bytes.Split(out, []byte{0})
	if truncated && len(fields) > 0 {
		fields = fields[:len(fields)-1] // the last entry may be cut
	}
	for _, f := range fields {
		if len(f) < 4 || f[2] != ' ' {
			continue
		}
		x, y, path := f[0], f[1], string(f[3:])
		c := Change{Path: path}
		switch {
		case x == '?' && y == '?':
			c.Kind = "untracked"
		case x == '!':
			continue
		case x == 'A':
			c.Kind, c.Staged = "added", true
		case x == 'D' || y == 'D':
			c.Kind = "deleted"
		case x == 'R' || x == 'C':
			c.Kind = "renamed"
		default:
			c.Kind = "modified"
		}
		changes = append(changes, c)
	}
	return top, changes, truncated, nil
}

// Diff returns the diff of one file against HEAD, in unified format, read in top, the
// repository's top folder. rel is relative to top, in slash form. The output is cut at max
// bytes. An empty diff means git sees no change against HEAD.
func (r Reader) Diff(ctx context.Context, top, rel string, max int) (diff string, truncated bool, err error) {
	out, truncated, err := r.gitRaw(ctx, top, max, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", "--", ":(literal)"+rel)
	if err != nil {
		return "", false, err
	}
	return string(out), truncated, nil
}

// gitRaw runs one read-only git command in dir and returns its output untrimmed, at most max
// bytes; truncated says it was cut.
func (r Reader) gitRaw(ctx context.Context, dir string, max int, args ...string) ([]byte, bool, error) {
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	full := append([]string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), r.Env...)
	cmd.WaitDelay = 200 * time.Millisecond
	out := &capped{max: max}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			return nil, false, &gitError{args: args, code: ee.ExitCode(), stderr: strings.TrimSpace(errb.String())}
		}
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.buf.Bytes(), out.cut, nil
}

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.cut = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}
