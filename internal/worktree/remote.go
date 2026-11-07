package worktree

import (
	"context"
	"fmt"
	"strings"
)

// RemoteFor returns the name of the repository's remote whose URL is the project's identity,
// or "" when none is (agent groups, groups.md 7.1, 12.7).
func (g Git) RemoteFor(ctx context.Context, dir, identity string) string {
	if own := g.ownRemotes(ctx, dir, identity); len(own) > 0 {
		return own[0]
	}
	return ""
}

// Fetch fetches every branch of remote into its remote-tracking refs, with no tags.
func (g Git) Fetch(ctx context.Context, dir, remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") {
		return fmt.Errorf("%w: remote %q", ErrInvalid, remote)
	}
	_, err := g.run(ctx, dir, "fetch", "--quiet", "--no-tags", remote)
	return err
}

// PushBranch pushes the local branch to the same name on remote, replacing what is there: a
// group branch belongs to its member (groups.md 7.1).
func (g Git) PushBranch(ctx context.Context, dir, remote, branch string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%w: remote %q, branch %q", ErrInvalid, remote, branch)
	}
	_, err := g.run(ctx, dir, "push", "--quiet", "--force", remote, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}
