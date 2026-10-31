// Package gitstatus reads a project's repository state for gff-hab: the current branch, the
// HEAD commit, and how far the branch is ahead of and behind its upstream
// (docs/specs/hab-logic-api.md 3.9). It runs the git binary in the project's folder with a
// short timeout and a clean environment. It never fetches and never writes to the repository:
// every command is a read, and GIT_OPTIONAL_LOCKS=0 keeps git off the index lock.
//
// A Watcher keeps the state of every project fresh: on request (debounced), on a slow timer, and
// when a cheap poll of .git/HEAD and the branch refs sees a change. It never blocks its caller.
package gitstatus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// Result is one read: the state, plus the files whose change means the state may have changed.
type Result struct {
	State structure.RepoState
	// Watch holds HEAD, the branch ref and the upstream ref (loose files; a missing one is fine),
	// and packed-refs. Empty outside a repository.
	Watch []string
}

// Reader runs git. The zero value runs "git" from PATH.
type Reader struct {
	Git string   // the git binary; "" means "git" from PATH
	Env []string // extra environment entries, after the clean base; tests use it
}

// errNotRepo marks a folder that is not inside a repository.
var errNotRepo = errors.New("not a git repository")

// Read reads the repository state of dir. A dir that does not exist or is not in a repository
// gives the zero state and no error. A git failure or a timeout (from ctx) is an error; the
// caller keeps its last values.
func (r Reader) Read(ctx context.Context, dir string) (Result, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return Result{}, nil
	}
	out, err := r.git(ctx, dir, "rev-parse", "--absolute-git-dir", "--git-common-dir")
	if errors.Is(err, errNotRepo) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		return Result{}, fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	gitDir, common := lines[0], lines[1]
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	res := Result{Watch: []string{filepath.Join(gitDir, "HEAD"), filepath.Join(common, "packed-refs")}}

	// The branch: a symbolic HEAD, even an unborn one in an empty repository. Exit 1 means a
	// detached HEAD, so no branch.
	ref, err := r.git(ctx, dir, "symbolic-ref", "-q", "HEAD")
	switch {
	case err == nil:
		res.State.Branch = strings.TrimPrefix(ref, "refs/heads/")
		res.Watch = append(res.Watch, filepath.Join(common, filepath.FromSlash(ref)))
	case !isExit(err, 1):
		return Result{}, err
	}

	// The HEAD commit. Exit 1 with -q --verify means no commit yet (an empty repository).
	head, err := r.git(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	switch {
	case err == nil:
		res.State.Head = head
	case isExit(err, 1):
		return res, nil
	default:
		return Result{}, err
	}
	if res.State.Branch == "" {
		return res, nil
	}

	// The upstream. git fails with exit 128 when the branch has none, or when its ref is gone.
	up, err := r.git(ctx, dir, "rev-parse", "--symbolic-full-name", "@{upstream}")
	switch {
	case err == nil:
		res.Watch = append(res.Watch, filepath.Join(common, filepath.FromSlash(up)))
	case isExit(err, 128):
		return res, nil
	default:
		return Result{}, err
	}
	counts, err := r.git(ctx, dir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	switch {
	case err == nil:
	case isExit(err, 128):
		return res, nil
	default:
		return Result{}, err
	}
	f := strings.Fields(counts)
	if len(f) != 2 {
		return Result{}, fmt.Errorf("git rev-list: unexpected output %q", counts)
	}
	ahead, err1 := strconv.Atoi(f[0])
	behind, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return Result{}, fmt.Errorf("git rev-list: unexpected output %q", counts)
	}
	res.State.Ahead, res.State.Behind = &ahead, &behind
	return res, nil
}

// gitError is a git run that exited non-zero.
type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.args, " "), e.code, e.stderr)
}

func isExit(err error, code int) bool {
	var ge *gitError
	return errors.As(err, &ge) && ge.code == code
}

// git runs one read-only git command in dir and returns its trimmed output.
func (r Reader) git(ctx context.Context, dir string, args ...string) (string, error) {
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	full := append([]string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), r.Env...)
	// A git that ignores the kill, or a child that holds its pipes open, must not hold us.
	cmd.WaitDelay = 200 * time.Millisecond
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			msg := strings.TrimSpace(errb.String())
			if ee.ExitCode() == 128 && strings.Contains(msg, "not a git repository") {
				return "", errNotRepo
			}
			return "", &gitError{args: args, code: ee.ExitCode(), stderr: msg}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out.String()), nil
}

// cleanEnv is the environment git runs with: only what finding git and the user's config
// needs, the C locale, no optional locks, no pager and no prompt. Any GIT_DIR, GIT_WORK_TREE or
// similar setting of the hab's own environment is left out.
func cleanEnv() []string {
	env := []string{
		"LC_ALL=C",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, k := range []string{"PATH", "HOME", "XDG_CONFIG_HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
