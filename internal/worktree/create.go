package worktree

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// testHookAfterCheck, when set, runs between the foreign check and git worktree add.
var testHookAfterCheck func()

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
	if testHookAfterCheck != nil {
		testHookAfterCheck()
	}

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
	// git worktree add resolved the start point again; a fetch in between may have moved it.
	// The foreign check covered sp.Commit only.
	if head, err := g.run(ctx, path, "rev-parse", "HEAD"); err != nil || head != sp.Commit {
		foreign = fmt.Sprintf("the start point moved to %.12s after the check of %.12s", head, sp.Commit)
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
// It compares segments without regard to case, so origin/PR/7 counts too.
func isPullRef(ref string) bool {
	for _, seg := range strings.Split(ref, "/") {
		switch strings.ToLower(seg) {
		case "pull", "pr", "pull-request", "pull-requests", "merge-requests", "mr", "changes":
			return true
		}
	}
	return false
}

// refspec is one fetch refspec of a remote, without its leading +.
type refspec struct{ src, dst string }

// fetchSpecs reads remote.<r>.fetch. Negative refspecs (^...) are left out.
func (g Git) fetchSpecs(ctx context.Context, dir, remote string) []refspec {
	out, err := g.run(ctx, dir, "config", "--get-all", "remote."+remote+".fetch")
	if err != nil {
		return nil
	}
	var specs []refspec
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimPrefix(strings.TrimSpace(line), "+")
		if s == "" || strings.HasPrefix(s, "^") {
			continue
		}
		src, dst, ok := strings.Cut(s, ":")
		if !ok {
			continue
		}
		specs = append(specs, refspec{src, dst})
	}
	return specs
}

// globMatch matches ref against a refspec side with at most one *.
func globMatch(pattern, ref string) bool {
	before, after, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == ref
	}
	return strings.HasPrefix(ref, before) && strings.HasSuffix(ref, after) && len(ref) >= len(before)+len(after)
}

// branchRef reports whether a remote-tracking ref holds a branch of the remote by the remote's
// configured refspecs: some refspec from refs/heads/ maps onto it, and no refspec from anywhere
// else (refs/pull/, refs/changes/, refs/*) does. The default +refs/heads/*:refs/remotes/origin/*
// also matches origin/pr/7, so the second clause is what keeps a pull request refspec's refs out.
func branchRef(ref string, specs []refspec) bool {
	heads := false
	for _, s := range specs {
		if !globMatch(s.dst, ref) {
			continue
		}
		if !strings.HasPrefix(s.src, "refs/heads/") {
			return false
		}
		heads = true
	}
	return heads
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

// trustedRefs lists the branches of the project's remotes. Two layers decide: the remote's
// fetch refspecs (branchRef), so a refspec such as
// +refs/pull/*/head:refs/remotes/origin/pull-request/* keeps its refs out whatever their names;
// then the name, for a one-off fetch into the remote's namespace: a ref whose name below
// refs/remotes/<remote>/ has a pull request segment is left out.
func (g Git) trustedRefs(ctx context.Context, dir string, own []string) []string {
	var out []string
	for _, r := range own {
		specs := g.fetchSpecs(ctx, dir, r)
		refs, err := g.run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/"+r+"/")
		if err != nil {
			continue
		}
		for _, ref := range strings.Fields(refs) {
			if branchRef(ref, specs) && !isPullRef(strings.TrimPrefix(ref, "refs/remotes/"+r+"/")) {
				out = append(out, ref)
			}
		}
	}
	return out
}

// maxLocalCommits bounds the commits not on the project's remote that committedHere checks.
const maxLocalCommits = 200

// maxAmendDepth bounds how far committedHere follows a chain of amends, rebases and
// cherry-picks back.
const maxAmendDepth = 8

// reflogIndex is what the reflogs say about commits made here.
type reflogIndex struct {
	made map[string]bool // commits a "commit:", "commit (initial):" or "commit (merge):" entry made
	// from maps a commit an amend, rebase or cherry-pick entry made to the commits it was made
	// from: for an amend the commit it replaced, for a rebase or cherry-pick its source.
	from map[string][]string
	// The rebase and cherry-pick entries, whose sources are found only when the walk reaches
	// them (resolvePicks): the commits a pick entry made, each commit's author key, and the
	// commits the reflogs name by author key.
	picks    map[string]bool
	author   map[string]string
	byAuthor map[string][]string
	resolved map[string]bool
}

// isPick reports whether a reflog subject is an entry that copies a commit: a rebase step that
// keeps its changes, as `git rebase` or `git pull --rebase` writes it with any arguments
// ("rebase (pick):", "rebase -i (pick):", "pull -q --rebase origin main (pick):"), or a
// cherry-pick. Squash and fixup steps change the changes, so they are not picks.
func isPick(subject string) bool {
	action, _, ok := strings.Cut(subject, ": ")
	if !ok {
		return false
	}
	if action == "cherry-pick" {
		return true
	}
	if !strings.HasPrefix(action, "rebase") && !strings.HasPrefix(action, "pull") {
		return false
	}
	return strings.HasSuffix(action, "(pick)") || strings.HasSuffix(action, "(reword)") || strings.HasSuffix(action, "(edit)")
}

// commitFields reads parents, author name, email and raw date of each commit, keyed by commit.
func (g Git) commitFields(ctx context.Context, dir string, commits []string) map[string][]string {
	out, err := g.runIn(ctx, dir, []byte(strings.Join(commits, "\n")+"\n"),
		"log", "--no-walk=unsorted", "--stdin", "--date=raw", "--format=%H%x00%P%x00%an%x00%ae%x00%ad")
	fields := map[string][]string{}
	if err != nil {
		return fields // false alarms only: no pair is kept
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Split(line, "\x00"); len(f) == 5 {
			fields[f[0]] = f[1:]
		}
	}
	return fields
}

// patchIDs reads the patch id of each commit: the same id means the same changes. The diff comes
// from plumbing, so user config (textconv, external diff, context size, renames) cannot change
// it; --full-index puts full blob ids on a binary file's index line; --verbatim keeps
// whitespace, since in a script a space can change the meaning. A git without --verbatim
// fails the call: no ids, so false alarms only. A merge or a root commit has no id.
func (g Git) patchIDs(ctx context.Context, dir string, commits []string) map[string]string {
	ids := map[string]string{}
	diffs, err := g.runIn(ctx, dir, []byte(strings.Join(commits, "\n")+"\n"),
		"diff-tree", "--stdin", "-p", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "-U3")
	if err != nil {
		return ids
	}
	out, err := g.runIn(ctx, dir, diffs, "patch-id", "--verbatim")
	if err != nil {
		return ids
	}
	for _, line := range strings.Split(string(out), "\n") {
		if id, commit, ok := strings.Cut(line, " "); ok {
			ids[commit] = id
		}
	}
	return ids
}

// readReflogs reads the reflogs that git commit writes: each worktree's HEAD and the local
// branches. Remote-tracking reflogs, which can be large, are not read. Within one ref's log
// (newest first) an entry's old value is the next entry's commit.
func (g Git) readReflogs(ctx context.Context, dir string) reflogIndex {
	idx := reflogIndex{made: map[string]bool{}, from: map[string][]string{}, picks: map[string]bool{},
		author: map[string]string{}, byAuthor: map[string][]string{}, resolved: map[string]bool{}}
	refs := []string{"--branches", "HEAD", "main-worktree/HEAD"}
	if common, err := g.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		if entries, err := os.ReadDir(filepath.Join(common, "worktrees")); err == nil {
			for _, e := range entries {
				refs = append(refs, "worktrees/"+e.Name()+"/HEAD")
			}
		}
	}
	log, err := g.run(ctx, dir, append([]string{"reflog", "show", "--format=%gD %H %gs"}, refs...)...)
	if err != nil { // a worktree with no log, say: read what every repository has
		if log, err = g.run(ctx, dir, "reflog", "show", "--format=%gD %H %gs", "--branches", "HEAD"); err != nil {
			return idx
		}
	}
	// git interleaves several refs' entries by date, so pair within each ref, by the n of
	// name@{n}: entry n's old value is entry n+1's commit.
	type entry struct{ hash, subject string }
	logs := map[string]map[int]entry{}
	for _, line := range strings.Split(log, "\n") {
		sel, rest, _ := strings.Cut(line, " ")
		hash, subject, _ := strings.Cut(rest, " ")
		i := strings.LastIndex(sel, "@{")
		if i <= 0 || !strings.HasSuffix(sel, "}") {
			continue
		}
		n, err := strconv.Atoi(sel[i+2 : len(sel)-1])
		if err != nil {
			continue
		}
		if logs[sel[:i]] == nil {
			logs[sel[:i]] = map[int]entry{}
		}
		logs[sel[:i]][n] = entry{hash, subject}
	}
	type pair struct{ amended, replaced string }
	var pairs []pair
	seenHash := map[string]bool{}
	var all []string // every commit the reflogs name: the sources a pick may come from
	for _, log := range logs {
		for n, e := range log {
			if !seenHash[e.hash] {
				seenHash[e.hash] = true
				all = append(all, e.hash)
			}
			switch {
			case strings.HasPrefix(e.subject, "commit (amend):"):
				if old, ok := log[n+1]; ok && old.hash != e.hash {
					pairs = append(pairs, pair{e.hash, old.hash})
				}
			case strings.HasPrefix(e.subject, "commit:"), strings.HasPrefix(e.subject, "commit (initial):"),
				strings.HasPrefix(e.subject, "commit (merge):"):
				idx.made[e.hash] = true
			case isPick(e.subject):
				idx.picks[e.hash] = true
			}
		}
	}
	if len(pairs) == 0 && len(idx.picks) == 0 {
		return idx
	}
	// gc expires an entry whose commit became unreachable (30 days by default), so the entry
	// before an amend may be gone, and the next one older a different commit. Keep a pair only
	// when the two commits agree on what git commit --amend keeps: parents and author.
	fields := g.commitFields(ctx, dir, all)
	for _, p := range pairs {
		a, ok := fields[p.amended]
		if ok && slices.Equal(a, fields[p.replaced]) {
			idx.add(p.amended, p.replaced)
		}
	}
	for _, c := range all {
		if f, ok := fields[c]; ok {
			key := strings.Join(f[1:], "\x00")
			idx.author[c] = key
			idx.byAuthor[key] = append(idx.byAuthor[key], c)
		}
	}
	return idx
}

// add records src as a commit c was made from.
func (idx reflogIndex) add(c, src string) {
	if c != src && !slices.Contains(idx.from[c], src) {
		idx.from[c] = append(idx.from[c], src)
	}
}

// resolvePicks finds the sources of the pick entries among commits, the ones the walk reached and
// no commit entry made. A rebase or cherry-pick entry names only the copy. Its source is a commit
// the reflogs name with the same author name, email and date (which both keep) and the same
// patch id: the same changes, so the copy brings in nothing its source did not. One patch-id
// batch per call; each pick is resolved once.
func (g Git) resolvePicks(ctx context.Context, dir string, idx reflogIndex, commits []string) {
	var picks []string
	need := map[string]bool{}
	for _, c := range commits {
		if !idx.picks[c] || idx.made[c] || idx.resolved[c] || idx.author[c] == "" {
			continue
		}
		idx.resolved[c] = true
		picks = append(picks, c)
		need[c] = true
		for _, src := range idx.byAuthor[idx.author[c]] {
			need[src] = true
		}
	}
	if len(picks) == 0 {
		return
	}
	ids := g.patchIDs(ctx, dir, slices.Collect(maps.Keys(need)))
	for _, p := range picks {
		for _, src := range idx.byAuthor[idx.author[p]] {
			if ids[p] != "" && ids[src] == ids[p] {
				idx.add(p, src)
			}
		}
	}
}

// committedHere reports whether every commit in commit's history that no ref in base holds was
// made in this repository by git commit: each has a "commit:", "commit (initial):" or
// "commit (merge):" entry in a reflog, or an amend, rebase or cherry-pick entry whose replaced
// commit or source is itself own by this same rule (followed back at most maxAmendDepth
// steps). So amending, rebasing or cherry-picking a fetched pull request does not make it own.
// Only local git writes the reflog, so a fetched pull request cannot claim an entry. Entries
// from am, squash and fixup do not count. No reflog entry, an entry past its expiry, or more
// than maxLocalCommits commits: false.
func (g Git) committedHere(ctx context.Context, dir, commit string, base []string) bool {
	return g.ownHistory(ctx, dir, commit, base, g.readReflogs(ctx, dir), map[string]bool{}, maxAmendDepth)
}

// ownHistory is committedHere for one commit. seen memoizes the answer per commit; a commit is
// marked false while its own check runs, so a cycle of amends ends at once and the work is
// linear in the amend entries.
func (g Git) ownHistory(ctx context.Context, dir, commit string, base []string, idx reflogIndex, seen map[string]bool, depth int) bool {
	if own, ok := seen[commit]; ok {
		return own
	}
	seen[commit] = false
	own := g.ownHistoryOnce(ctx, dir, commit, base, idx, seen, depth)
	seen[commit] = own
	return own
}

func (g Git) ownHistoryOnce(ctx context.Context, dir, commit string, base []string, idx reflogIndex, seen map[string]bool, depth int) bool {
	var stdin strings.Builder
	for _, b := range base {
		stdin.WriteString("^" + b + "\n")
	}
	out, err := g.runIn(ctx, dir, []byte(stdin.String()), "rev-list", fmt.Sprintf("--max-count=%d", maxLocalCommits+1), commit, "--stdin")
	if err != nil {
		return false
	}
	local := strings.Fields(string(out))
	if len(local) > maxLocalCommits {
		return false
	}
	if depth > 0 {
		g.resolvePicks(ctx, dir, idx, local)
	}
	for _, c := range local {
		if idx.made[c] {
			continue
		}
		replacedOwn := false
		if depth > 0 {
			for _, old := range idx.from[c] {
				if g.ownHistory(ctx, dir, old, base, idx, seen, depth-1) {
					replacedOwn = true
					break
				}
			}
		}
		if !replacedOwn {
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
