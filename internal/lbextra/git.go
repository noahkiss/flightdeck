package lbextra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// GitEvery is the least time between two export commits (13).
const GitEvery = time.Hour

// The export's commits carry this author, so they read as the hab's and need no git identity.
const (
	gitName  = "Flightdeck Logbook"
	gitEmail = "logbook@flightdeck.invalid"
)

// GitExport writes the Markdown tree into a local repository and commits it (13). One way:
// edits in the repository are overwritten at the next export, never read back.
type GitExport struct {
	Dir string
	// Git is the git program; empty is "git" on PATH.
	Git string
}

// GitResult is what one export did.
type GitResult struct {
	TreeResult
	Committed bool
}

// Export writes the tree and commits it when anything changed. It makes the folder and its
// repository when they are missing.
func (g GitExport) Export(ctx context.Context, st *logbook.Store, now time.Time) (GitResult, error) {
	var res GitResult
	if err := os.MkdirAll(g.Dir, 0o700); err != nil {
		return res, err
	}
	if _, err := os.Stat(g.Dir + "/.git"); errors.Is(err, os.ErrNotExist) {
		if err := checkTree(g.Dir); err != nil {
			return res, err
		}
		if _, err := g.run(ctx, "init", "-q"); err != nil {
			return res, err
		}
	}
	tr, err := WriteTree(ctx, st, g.Dir)
	res.TreeResult = tr
	if err != nil {
		return res, err
	}
	if _, err := g.run(ctx, "add", "-A", "--", "."); err != nil {
		return res, err
	}
	if _, err := g.run(ctx, "diff", "--cached", "--quiet"); err == nil {
		return res, nil
	} else if !isExit(err, 1) {
		return res, err
	}
	msg := fmt.Sprintf("Logbook export: %d records", tr.Records)
	if _, err := g.run(ctx, "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", msg,
		"--date", now.UTC().Format(time.RFC3339)); err != nil {
		return res, err
	}
	res.Committed = true
	return res, nil
}

// LastCommit is the time of the repository's last commit, or zero with none.
func (g GitExport) LastCommit(ctx context.Context) time.Time {
	out, err := g.run(ctx, "log", "-1", "--format=%at")
	if err != nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (g GitExport) run(ctx context.Context, args ...string) (string, error) {
	bin := g.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", g.Dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+gitName, "GIT_AUTHOR_EMAIL="+gitEmail,
		"GIT_COMMITTER_NAME="+gitName, "GIT_COMMITTER_EMAIL="+gitEmail, "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), &gitError{args: args, code: ee.ExitCode(), stderr: strings.TrimSpace(stderr.String())}
		}
		return string(out), err
	}
	return string(out), nil
}

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
