package worktree

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// BranchPrefix starts the name of a branch Create makes up when the caller gives none.
const BranchPrefix = "wt/"

// CreateOptions says what worktree to make.
type CreateOptions struct {
	// Source is a folder in the repository. Its working tree is the source checkout: the base
	// defaults to its HEAD, and .worktreeinclude and the ignored files come from it.
	Source string
	// Root is the folder that holds new worktrees: <Root>/<repository folder>/<branch slug>.
	// Unused when Path is set.
	Root string
	// Branch is checked out when it exists as a local branch, or as a branch of exactly one
	// remote when Base is empty; otherwise it is made from Base. Empty: a new branch named
	// wt/<word>-<word>-<hex>.
	Branch string
	// Base is where a new branch starts: a branch, a remote branch, a tag or a commit. Empty
	// means the source checkout's HEAD. Create never fetches.
	Base string
	// Path is the new worktree's folder, absolute. It must not exist.
	Path string
	// Remote is the remote that identifies the project, in any form; "" when the project has
	// none. A start point from another remote marks the worktree foreign.
	Remote string
}

// Created is a new worktree.
type Created struct {
	Worktree  Worktree
	Source    string   // top folder of the source checkout
	NewBranch bool     // Create made the branch
	Copied    []string // files copied by .worktreeinclude, relative to the worktree
	// Foreign says why the code in the worktree may come from someone else, so its setup waits
	// for a yes; "" when it is the project's own.
	Foreign string
}

// Create makes a worktree, then copies the ignored files .worktreeinclude names from the source
// checkout. A copy failure removes the worktree again, and the branch when Create made it. It
// runs no script.
func (g Git) Create(ctx context.Context, o CreateOptions) (Created, error) {
	src, err := g.Top(ctx, o.Source)
	if err != nil {
		return Created{}, err
	}
	list, err := g.List(ctx, src)
	if err != nil {
		return Created{}, err
	}
	if len(list) == 0 {
		return Created{}, fmt.Errorf("%w: %s has no working tree", ErrInvalid, src)
	}

	branch := o.Branch
	if branch == "" {
		if branch, err = g.freeBranch(ctx, src); err != nil {
			return Created{}, err
		}
	}
	if strings.HasPrefix(branch, "-") {
		return Created{}, fmt.Errorf("%w: branch %q starts with a dash", ErrInvalid, branch)
	}
	if _, err := g.run(ctx, src, "check-ref-format", "--branch", branch); err != nil {
		if exitCode(err) > 0 {
			return Created{}, fmt.Errorf("%w: %q is not a valid branch name", ErrInvalid, branch)
		}
		return Created{}, err
	}
	if strings.HasPrefix(o.Base, "-") {
		return Created{}, fmt.Errorf("%w: base %q starts with a dash", ErrInvalid, o.Base)
	}

	path := o.Path
	if path == "" {
		if o.Root == "" || !filepath.IsAbs(o.Root) {
			return Created{}, fmt.Errorf("%w: the worktree root %q is not an absolute path", ErrInvalid, o.Root)
		}
		path = filepath.Join(o.Root, filepath.Base(list[0].Path), Slug(branch))
	}
	if !filepath.IsAbs(path) {
		return Created{}, fmt.Errorf("%w: path %q is not absolute", ErrInvalid, path)
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return Created{}, fmt.Errorf("%w: %s exists already", ErrConflict, path)
	}
	for _, w := range list {
		if w.Branch == branch {
			return Created{}, fmt.Errorf("%w: branch %s is checked out in %s already", ErrConflict, branch, w.Path)
		}
	}

	// Pick the start point and the git worktree add arguments.
	var args []string
	var sp startPoint // where the code comes from, for the foreign check
	newBranch := false
	switch local := g.refExists(ctx, src, "refs/heads/"+branch); {
	case local && o.Base != "":
		return Created{}, fmt.Errorf("%w: branch %s exists; give no base to check it out", ErrConflict, branch)
	case local:
		args = []string{"worktree", "add", "--", path, branch}
		sp.Branch = branch
		sp.Ref, _ = g.run(ctx, src, "rev-parse", "--symbolic-full-name", branch+"@{upstream}")
		sp.Commit, err = g.run(ctx, src, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return Created{}, err
		}
	default:
		base := o.Base
		track := false
		if base == "" {
			if r := g.remoteBranches(ctx, src, branch); len(r) == 1 {
				base, track = r[0], true
			} else if len(r) > 1 {
				return Created{}, fmt.Errorf("%w: branch %s exists on several remotes; give a base", ErrConflict, branch)
			}
		}
		if base == "" {
			base = "HEAD"
		}
		if sp.Commit, err = g.run(ctx, src, "rev-parse", "-q", "--verify", base+"^{commit}"); err != nil {
			if exitCode(err) > 0 {
				return Created{}, fmt.Errorf("%w: base %s is not a commit in this repository", ErrNotFound, base)
			}
			return Created{}, err
		}
		sp.Ref, _ = g.run(ctx, src, "rev-parse", "--symbolic-full-name", base)
		trackFlag := "--no-track"
		if track {
			trackFlag = "--track"
		}
		args = []string{"worktree", "add", trackFlag, "-b", branch, "--", path, base}
		newBranch = true
	}
	foreign := g.foreign(ctx, src, sp, o.Remote)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Created{}, fmt.Errorf("make %s: %w", filepath.Dir(path), err)
	}
	if _, err := g.run(ctx, src, args...); err != nil {
		var ge *gitError
		if errors.As(err, &ge) && strings.Contains(ge.stderr, "already") {
			return Created{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return Created{}, err
	}

	copied, err := g.copyIncludes(ctx, src, path)
	if err != nil {
		g.undo(context.WithoutCancel(ctx), src, path, branch, newBranch)
		return Created{}, fmt.Errorf("copy the files .worktreeinclude names (the worktree was removed again): %w", err)
	}
	w, err := g.Find(ctx, src, path)
	if err != nil {
		return Created{}, err
	}
	return Created{Worktree: w, Source: src, NewBranch: newBranch, Copied: copied, Foreign: foreign}, nil
}

// undo removes a worktree Create just made, and its branch when Create made that too.
func (g Git) undo(ctx context.Context, src, path, branch string, newBranch bool) {
	_, _ = g.run(ctx, src, "worktree", "remove", "--force", "--", path)
	if _, err := os.Lstat(path); err == nil {
		_ = os.RemoveAll(path)
		_ = g.pruneOne(ctx, src, path)
	}
	if newBranch {
		_, _ = g.run(ctx, src, "branch", "-D", "--", branch)
	}
}

// refExists reports whether a full ref name exists.
func (g Git) refExists(ctx context.Context, dir, ref string) bool {
	_, err := g.run(ctx, dir, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// remoteBranches returns the remote branches named <remote>/<branch>, one per remote that has it.
func (g Git) remoteBranches(ctx context.Context, dir, branch string) []string {
	remotes, err := g.run(ctx, dir, "remote")
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range strings.Fields(remotes) {
		if g.refExists(ctx, dir, "refs/remotes/"+r+"/"+branch) {
			out = append(out, r+"/"+branch)
		}
	}
	return out
}

// startPoint is where a worktree's code comes from, for the foreign check.
type startPoint struct {
	Ref    string // full ref name of the start point; "" for a commit id, FETCH_HEAD and the like
	Branch string // the local branch checked out as it is; "" for a new branch
	Commit string // the commit the worktree starts at
}

// isPullRef reports whether a ref name has a pull request segment.
func isPullRef(ref string) bool {
	for _, seg := range strings.Split(ref, "/") {
		if seg == "pull" || seg == "pr" || seg == "merge-requests" || seg == "pull-requests" {
			return true
		}
	}
	return false
}

// foreign says why the code at a start point may come from someone other than the project, or ""
// when it is the project's own. False alarms cost the user one yes; a miss runs a stranger's
// script, so the check leans to foreign:
//   - a start ref, or a local branch's configured upstream (branch.<b>.merge), that names a pull
//     request is foreign, as gh pr checkout and a fetch of pull/N/head leave it;
//   - a local branch whose branch.<b>.remote is a remote, or a URL, other than the project's is
//     foreign;
//   - otherwise the start commit is the project's own only when a branch of the project's remote
//     (refs/remotes/<remote>/ of each remote whose URL is the project's) contains it. A project
//     with no remote has nothing to check against: its source checkout's HEAD stands in.
func (g Git) foreign(ctx context.Context, dir string, sp startPoint, identity string) string {
	if isPullRef(sp.Ref) {
		return fmt.Sprintf("%s looks like a pull request ref", sp.Ref)
	}
	own := g.ownRemotes(ctx, dir, identity)
	if rest, ok := strings.CutPrefix(sp.Ref, "refs/remotes/"); ok {
		mine := false
		for _, r := range own {
			mine = mine || strings.HasPrefix(rest, r+"/")
		}
		if !mine {
			return fmt.Sprintf("%s is a branch of another remote, not the project's", sp.Ref)
		}
	}
	if sp.Branch != "" {
		merge, _ := g.run(ctx, dir, "config", "--get", "branch."+sp.Branch+".merge")
		if isPullRef(merge) {
			return fmt.Sprintf("branch %s follows %s, a pull request", sp.Branch, merge)
		}
		if remote, _ := g.run(ctx, dir, "config", "--get", "branch."+sp.Branch+".remote"); remote != "" && remote != "." {
			if !slices.Contains(own, remote) && (identity == "" || structure.NormalizeRemote(remote) != structure.NormalizeRemote(identity)) {
				return fmt.Sprintf("branch %s follows remote %s, not the project's remote", sp.Branch, remote)
			}
		}
	}
	if identity == "" {
		if g.contains(ctx, dir, sp.Commit, "HEAD") || g.committedHere(ctx, dir, sp.Commit, []string{"HEAD"}) {
			return ""
		}
		return fmt.Sprintf("commit %.12s is not in the source checkout's history", sp.Commit)
	}
	trusted := g.trustedRefs(ctx, dir, own)
	for _, r := range own {
		out, err := g.run(ctx, dir, "for-each-ref", "--contains", sp.Commit, "--format=%(refname)", "refs/remotes/"+r+"/")
		if err != nil {
			continue
		}
		for _, ref := range strings.Fields(out) {
			if slices.Contains(trusted, ref) {
				return ""
			}
		}
	}
	if g.committedHere(ctx, dir, sp.Commit, trusted) {
		return ""
	}
	return fmt.Sprintf("commit %.12s is not on a branch of the project's remote", sp.Commit)
}

// trustedRefs lists the branches of the project's remotes, leaving out every ref whose name
// below refs/remotes/<remote>/ has a pull request segment: a refspec such as
// +refs/pull/*/head:refs/remotes/origin/pr/* puts strangers' commits there.
func (g Git) trustedRefs(ctx context.Context, dir string, own []string) []string {
	var out []string
	for _, r := range own {
		refs, err := g.run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/"+r+"/")
		if err != nil {
			continue
		}
		for _, ref := range strings.Fields(refs) {
			if !isPullRef(strings.TrimPrefix(ref, "refs/remotes/"+r+"/")) {
				out = append(out, ref)
			}
		}
	}
	return out
}

// maxLocalCommits bounds the commits not on the project's remote that committedHere checks.
const maxLocalCommits = 200

// committedHere reports whether every commit in commit's history that no ref in base holds was
// made in this repository by git commit: each has a commit entry in a reflog ("commit:",
// "commit (amend):", "commit (initial):", "commit (merge):"). Only local git writes the reflog,
// so a fetched pull request cannot claim one. Entries from rebase, cherry-pick and am do not
// count. No reflog, an entry past its expiry, or more than maxLocalCommits commits: false.
func (g Git) committedHere(ctx context.Context, dir, commit string, base []string) bool {
	var stdin strings.Builder
	for _, b := range base {
		stdin.WriteString("^" + b + "\n")
	}
	out, err := g.runIn(ctx, dir, []byte(stdin.String()), "rev-list", fmt.Sprintf("--max-count=%d", maxLocalCommits+1), commit, "--stdin")
	if err != nil {
		return false
	}
	local := strings.Fields(string(out))
	if len(local) == 0 || len(local) > maxLocalCommits {
		return len(local) == 0
	}
	made := map[string]bool{}
	for _, args := range [][]string{{"HEAD"}, {"--all"}} {
		log, err := g.run(ctx, dir, append([]string{"reflog", "show", "--format=%H %gs"}, args...)...)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(log, "\n") {
			hash, subject, ok := strings.Cut(line, " ")
			if ok && (strings.HasPrefix(subject, "commit:") || strings.HasPrefix(subject, "commit (")) {
				made[hash] = true
			}
		}
	}
	for _, c := range local {
		if !made[c] {
			return false
		}
	}
	return true
}

// ownRemotes returns the names of the remotes whose URL is the project's remote.
func (g Git) ownRemotes(ctx context.Context, dir, identity string) []string {
	if identity == "" {
		return nil
	}
	remotes, err := g.run(ctx, dir, "remote")
	if err != nil {
		return nil
	}
	var own []string
	for _, r := range strings.Fields(remotes) {
		url, _ := g.run(ctx, dir, "remote", "get-url", r)
		if url != "" && structure.NormalizeRemote(url) == structure.NormalizeRemote(identity) {
			own = append(own, r)
		}
	}
	return own
}

// contains reports whether commit is an ancestor of (or is) the commit at rev.
func (g Git) contains(ctx context.Context, dir, commit, rev string) bool {
	_, err := g.run(ctx, dir, "merge-base", "--is-ancestor", commit, rev)
	return err == nil
}

// freeBranch makes up a branch name that no local branch holds.
func (g Git) freeBranch(ctx context.Context, dir string) (string, error) {
	adjectives := []string{"amber", "brisk", "cedar", "dusty", "eager", "gentle", "hazel", "ivory",
		"jade", "keen", "lunar", "mellow", "nimble", "opal", "plum", "rustic"}
	nouns := []string{"anchor", "beacon", "canyon", "delta", "ember", "falcon", "glacier", "harbor",
		"island", "jetty", "kestrel", "lagoon", "mesa", "nebula", "orbit", "prairie"}
	for range 16 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		n := binary.BigEndian.Uint64(b[:])
		name := fmt.Sprintf("%s%s-%s-%04x", BranchPrefix, adjectives[n%16], nouns[(n/16)%16], (n>>8)&0xffff)
		if !g.refExists(ctx, dir, "refs/heads/"+name) {
			return name, nil
		}
	}
	return "", errors.New("no free branch name after 16 tries")
}

// Slug turns a branch name into a folder name: lower-case letters and digits, with one dash for
// each run of anything else.
func Slug(branch string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(branch) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "worktree"
	}
	return s
}
