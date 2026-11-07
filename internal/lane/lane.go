// Package lane lands one agent group item on its target branch (groups.md 7.3): fetch, rebase
// in the lane's own worktree, the project's checks, the push of the rebased branch, the
// project's CI command, and a fast-forward push to the target. It runs git and the project's
// commands; the group service decides what each outcome does to the record.
package lane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/worktree"
)

// Outcome is how a lane run ended.
type Outcome string

const (
	// Landed: the commit is on the target.
	Landed Outcome = "landed"
	// Back: the item goes back to its lead; Step names where it stopped.
	Back Outcome = "back"
	// Stopped: the remote refused a push for a rule, not a race; the lane stops for the
	// repository (groups.md 7.4).
	Stopped Outcome = "stopped"
	// Unavailable: the lane could not run now (no remote, a fetch that failed); it tries again
	// later and the item stays ready.
	Unavailable Outcome = "unavailable"
)

// Steps a lane run names.
const (
	StepFetch  = "fetch"
	StepRebase = "rebase"
	StepChecks = "checks"
	StepPush   = "push"
	StepCI     = "ci"
	StepLand   = "land"
	StepMoving = "moving"
)

// Attempt is one try of the lane on an item: the target commit it rebased on and the rebased
// head it pushed.
type Attempt struct {
	Target  string
	Rebased string
}

// Job is one item to land.
type Job struct {
	Source   string // a folder of the project's repository on this hab
	Dir      string // the lane's own worktree; made when missing
	Identity string // the project's remote identity
	Target   string // the target branch; "" is the remote's default branch
	Branch   string // the item's branch
	Land     string // the commit to land: the submitted head, or the review head after a pass with fixes
	// Upstreams are the heads the item this one stacks on has had (groups.md 7.2): the newest
	// that the item's commit holds is where its own commits start, and the ones before it drop
	// out of the rebase. Empty for an item that stacks on none. When the item holds none of
	// them, its branch started on a head the base no longer has: it goes back.
	Upstreams []string
	// Prior are the rebased heads earlier attempts pushed: one already on the target means the
	// land push went through before a restart.
	Prior       []string
	Tries       int // [groups] lane_tries
	OutputLines int // [groups] fix_output_lines
	// Heavy takes the heavy-run lock for the checks and returns its release (groups.md 8). Nil:
	// no lock.
	Heavy func(ctx context.Context) (func(), error)
	// Saved records the attempt after the rebased push and before CI; an error stops the run as
	// unavailable, since a restart could not tell the land went through.
	Saved func(Attempt) error
	Git   worktree.Git
}

// Result is what a run did.
type Result struct {
	Outcome Outcome
	Step    string
	Output  string // the first OutputLines lines of the failing step's output
	Commit  string // landed: the commit now on the target
	Target  string // the target branch's name
	NoCI    bool   // the target's flightdeck.json names no lane.ci: it landed on its checks alone
	Tries   int
}

// Config is the lane part of flightdeck.json, read from the target branch (groups.md 7.3 step 3).
type Config struct {
	Checks       string
	CI           string
	BuildFolders []string
}

// ParseConfig reads the lane keys of a flightdeck.json. Unknown keys are ignored.
func ParseConfig(b []byte) (Config, error) {
	var f struct {
		Lane struct {
			Checks       script   `json:"checks"`
			CI           script   `json:"ci"`
			BuildFolders []string `json:"build_folders"`
		} `json:"lane"`
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return Config{}, nil
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return Config{}, fmt.Errorf("flightdeck.json: %w", err)
	}
	return Config{Checks: strings.TrimSpace(string(f.Lane.Checks)), CI: strings.TrimSpace(string(f.Lane.CI)), BuildFolders: f.Lane.BuildFolders}, nil
}

type script string

func (s *script) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = script(one)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("must be a string or a list of strings")
	}
	*s = script(strings.Join(list, "\n"))
	return nil
}

// TargetConfig reads flightdeck.json's lane keys as committed at rev.
func TargetConfig(ctx context.Context, g worktree.Git, dir, rev string) (Config, error) {
	r := runner{git: g}
	out, err := r.git1(ctx, dir, "show", rev+":"+worktree.ConfigFile)
	if err != nil {
		if strings.Contains(out, "does not exist") || strings.Contains(out, "exists on disk, but not in") {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read %s at %.12s: %s", worktree.ConfigFile, rev, firstLine(out))
	}
	return ParseConfig([]byte(out))
}

// Run lands the job's commit, or says why not.
func Run(ctx context.Context, j Job) Result {
	r := runner{git: j.Git}
	if j.Tries < 1 {
		j.Tries = 1
	}
	res := Result{Target: j.Target}
	unavailable := func(step, format string, a ...any) Result {
		res.Outcome, res.Step, res.Output = Unavailable, step, fmt.Sprintf(format, a...)
		return res
	}
	if j.Identity == "" {
		return unavailable(StepFetch, "the project has no remote: the lane lands through the remote")
	}
	remote := j.Git.RemoteFor(ctx, j.Source, j.Identity)
	if remote == "" {
		return unavailable(StepFetch, "no remote of %s is the project's %s", j.Source, j.Identity)
	}
	for try := 1; try <= j.Tries; try++ {
		res.Tries = try
		// 1. Fetch the item's branch and the target.
		if err := j.Git.Fetch(ctx, j.Source, remote); err != nil {
			return unavailable(StepFetch, "fetch %s: %v", remote, err)
		}
		if res.Target == "" {
			t, err := r.defaultBranch(ctx, j.Source, remote)
			if err != nil {
				return unavailable(StepFetch, "%v", err)
			}
			res.Target = t
		}
		tref := "refs/remotes/" + remote + "/" + res.Target
		base, err := r.git1(ctx, j.Source, "rev-parse", "--verify", "-q", tref+"^{commit}")
		if err != nil {
			return unavailable(StepFetch, "the target %s is not on %s", res.Target, remote)
		}
		for _, p := range j.Prior {
			if p != "" && r.ancestor(ctx, j.Source, p, base) {
				res.Outcome, res.Commit = Landed, p
				return res
			}
		}
		if _, err := r.git1(ctx, j.Source, "cat-file", "-e", j.Land+"^{commit}"); err != nil {
			res.Outcome, res.Step = Back, StepFetch
			res.Output = fmt.Sprintf("commit %.12s is not in the repository on the lane's hab after a fetch of %s: push branch %s and submit again", j.Land, remote, j.Branch)
			return res
		}
		if r.ancestor(ctx, j.Source, j.Land, base) {
			res.Outcome, res.Commit = Landed, j.Land // the target holds it already
			return res
		}
		if err := r.prepare(ctx, j.Source, j.Dir, base); err != nil {
			return unavailable(StepRebase, "the lane's worktree %s: %v", j.Dir, err)
		}
		// 2. Rebase the item on the target.
		if _, err := r.git1(ctx, j.Dir, "checkout", "-q", "--detach", j.Land); err != nil {
			return unavailable(StepRebase, "check out %.12s: %v", j.Land, err)
		}
		args := []string{"rebase", base}
		if len(j.Upstreams) > 0 {
			up := ""
			for _, c := range j.Upstreams {
				if c == "" || c == j.Land || !r.ancestor(ctx, j.Dir, c, j.Land) {
					continue
				}
				if up == "" || r.ancestor(ctx, j.Dir, up, c) {
					up = c
				}
			}
			if up == "" {
				res.Outcome, res.Step = Back, StepRebase
				res.Output = fmt.Sprintf("branch %s started on a head of the item it stacks on that the item no longer has: rebase it on %s and submit again", j.Branch, res.Target)
				return res
			}
			args = []string{"rebase", "--onto", base, up}
		}
		if out, err := r.commit(ctx, j.Dir, args...); err != nil {
			r.git1(ctx, j.Dir, "rebase", "--abort")
			res.Outcome, res.Step, res.Output = Back, StepRebase, head(out, j.OutputLines)
			return res
		}
		rebased, err := r.git1(ctx, j.Dir, "rev-parse", "HEAD")
		if err != nil {
			return unavailable(StepRebase, "%v", err)
		}
		// 3. The checks, from the target's flightdeck.json, never the item's.
		cfg, err := TargetConfig(ctx, j.Git, j.Dir, base)
		if err != nil {
			res.Outcome, res.Step, res.Output = Stopped, StepChecks, err.Error()
			return res
		}
		env := []string{"GFF_LANE_BRANCH=" + j.Branch, "GFF_LANE_COMMIT=" + rebased, "GFF_LANE_TARGET=" + res.Target}
		if cfg.Checks != "" {
			release := func() {}
			if j.Heavy != nil {
				if release, err = j.Heavy(ctx); err != nil {
					return unavailable(StepChecks, "the heavy-run lock: %v", err)
				}
			}
			code, out, err := runScript(ctx, j.Dir, cfg.Checks, nil, env)
			release()
			if ctx.Err() != nil {
				return unavailable(StepChecks, "stopped: %v", ctx.Err())
			}
			if err != nil || code != 0 {
				res.Outcome, res.Step, res.Output = Back, StepChecks, head(exitLine(out, code, err), j.OutputLines)
				return res
			}
		}
		// 4. Push the rebased branch, and save the rebased head before CI.
		lease, _ := r.git1(ctx, j.Source, "rev-parse", "--verify", "-q", "refs/remotes/"+remote+"/"+j.Branch)
		out, err := r.git1(ctx, j.Dir, "push", "--porcelain", "--force-with-lease=refs/heads/"+j.Branch+":"+lease, remote, rebased+":refs/heads/"+j.Branch)
		if err != nil {
			switch pushRefusal(out) {
			case refusedRule:
				res.Outcome, res.Step, res.Output = Stopped, StepPush, head(out, j.OutputLines)
			case refusedRace:
				res.Outcome, res.Step, res.Output = Back, StepPush, head(out, j.OutputLines)
			default:
				return unavailable(StepPush, "push %s: %s", j.Branch, firstLine(out))
			}
			return res
		}
		if j.Saved != nil {
			if err := j.Saved(Attempt{Target: base, Rebased: rebased}); err != nil {
				return unavailable(StepPush, "save the lane attempt: %v", err)
			}
		}
		j.Prior = append(j.Prior, rebased)
		// 5. CI: the project's own command, with the branch and the commit.
		res.NoCI = cfg.CI == ""
		if cfg.CI != "" {
			code, out, err := runScript(ctx, j.Dir, cfg.CI, []string{j.Branch, rebased}, env)
			if ctx.Err() != nil {
				return unavailable(StepCI, "stopped: %v", ctx.Err())
			}
			if err != nil || code != 0 {
				res.Outcome, res.Step, res.Output = Back, StepCI, head(exitLine(out, code, err), j.OutputLines)
				return res
			}
		}
		// 6. Land: a fast-forward push to the target. Once sent it is not cut short: a push
		// killed half way would leave the lane unsure whether the item landed.
		if ctx.Err() != nil {
			return unavailable(StepLand, "stopped: %v", ctx.Err())
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		out, err = r.git1(pctx, j.Dir, "push", "--porcelain", remote, rebased+":refs/heads/"+res.Target)
		pcancel()
		if err == nil {
			res.Outcome, res.Commit = Landed, rebased
			return res
		}
		switch pushRefusal(out) {
		case refusedRule:
			res.Outcome, res.Step, res.Output = Stopped, StepLand, head(out, j.OutputLines)
			return res
		case refusedRace:
			continue // the target moved: start again at step 1
		default:
			// Maybe it landed: the next run's fetch tells, by the saved rebased head.
			return unavailable(StepLand, "push to %s: %s", res.Target, firstLine(out))
		}
	}
	res.Outcome, res.Step = Back, StepMoving
	res.Output = fmt.Sprintf("main keeps moving: the target %s moved during each of %d tries", res.Target, j.Tries)
	return res
}

type refusal int

const (
	refusedOther refusal = iota
	refusedRace
	refusedRule
)

// pushRefusal tells a race (the ref moved) from a rule (a protected branch, no permission). A
// server that lost a race on its own ref lock is a race too, not a rule.
func pushRefusal(out string) refusal {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "[rejected]") && (strings.Contains(low, "non-fast-forward") ||
		strings.Contains(low, "fetch first") || strings.Contains(low, "stale info")),
		strings.Contains(low, "[remote rejected]") && (strings.Contains(low, "cannot lock ref") ||
			strings.Contains(low, "incorrect old value") || strings.Contains(low, "failed to update ref") ||
			strings.Contains(low, "failed to lock")):
		return refusedRace
	case strings.Contains(low, "[remote rejected]"), strings.Contains(low, "protected branch"),
		strings.Contains(low, "permission"), strings.Contains(low, "denied"):
		return refusedRule
	}
	return refusedOther
}

type runner struct{ git worktree.Git }

// git1 runs git in dir and returns stdout and stderr together, trimmed.
func (r runner) git1(ctx context.Context, dir string, args ...string) (string, error) {
	return r.gitEnv(ctx, dir, nil, args...)
}

func (r runner) gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	bin := r.git.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_EDITOR=true"), r.git.Env...), env...)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return strings.TrimSpace(string(out)), err
}

// commit runs a git command that writes commits: with a committer of its own when the
// repository names none.
func (r runner) commit(ctx context.Context, dir string, args ...string) (string, error) {
	var env []string
	if v, _ := r.git1(ctx, dir, "config", "user.email"); v == "" {
		env = []string{"GIT_COMMITTER_NAME=Flightdeck lane", "GIT_COMMITTER_EMAIL=lane@flightdeck.invalid"}
	}
	return r.gitEnv(ctx, dir, env, args...)
}

func (r runner) ancestor(ctx context.Context, dir, commit, rev string) bool {
	_, err := r.git1(ctx, dir, "merge-base", "--is-ancestor", commit, rev)
	return err == nil
}

// defaultBranch is the remote's default branch.
func (r runner) defaultBranch(ctx context.Context, dir, remote string) (string, error) {
	if ref, err := r.git1(ctx, dir, "symbolic-ref", "-q", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/"); ok && b != "" {
			return b, nil
		}
	}
	out, err := r.git1(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				if b, _, ok := strings.Cut(rest, "\t"); ok && b != "" {
					return b, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%s names no default branch: set the plan's lane.target", remote)
}

// prepare makes the lane's worktree, or cleans it after an earlier run, detached at base.
func (r runner) prepare(ctx context.Context, src, dir, base string) error {
	if _, err := os.Stat(dir); err == nil {
		common, err1 := r.git1(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		want, err2 := r.git1(ctx, src, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err1 == nil && err2 == nil && filepath.Clean(common) == filepath.Clean(want) {
			r.git1(ctx, dir, "rebase", "--abort")
			if _, err := r.git1(ctx, dir, "reset", "-q", "--hard"); err != nil {
				return err
			}
			if _, err := r.git1(ctx, dir, "clean", "-q", "-fd"); err != nil {
				return err
			}
			return nil
		}
		// Not a worktree of this repository any more: made again.
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	r.git1(ctx, src, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if out, err := r.git1(ctx, src, "worktree", "add", "-q", "--detach", "--", dir, base); err != nil {
		return fmt.Errorf("%s", firstLine(out))
	}
	return nil
}

// head returns the first n lines of s; n 0 gives none.
func head(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.SplitAfterN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimRight(strings.Join(lines, ""), "\n")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func exitLine(out string, code int, err error) string {
	switch {
	case err != nil:
		return strings.TrimRight(out, "\n") + "\n" + err.Error()
	default:
		return strings.TrimRight(out, "\n") + fmt.Sprintf("\nexit status %d", code)
	}
}

// maxOutput bounds the output a check or CI run keeps: its start, which the lead gets.
const maxOutput = 256 << 10

type headBuffer struct{ b []byte }

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// runScript runs a project command through sh, stopping at the first command that fails, with
// args as $1 and on. It returns the exit code (-1 when it did not run to an exit) and the start
// of its output.
func runScript(ctx context.Context, dir, s string, args, env []string) (int, string, error) {
	argv := append([]string{"-c", "set -e\n" + s, "gff-lane"}, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out headBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Stdin = bytes.NewReader(nil)
	cmd.WaitDelay = time.Second
	worktree.OwnGroup(cmd)
	err := cmd.Run()
	if err == nil {
		return 0, string(out.b), nil
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return cmd.ProcessState.ExitCode(), string(out.b), nil
	}
	return -1, string(out.b), err
}
