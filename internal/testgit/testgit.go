// Package testgit makes every git a test binary runs, and every git its child processes run,
// ignore the user's global and system git config. A developer's config can set a hooks path, a
// filter or a default branch that changes what the test sees: a global post-checkout hook from
// git-lfs exits 2 when git-lfs is not on PATH, and then git worktree add fails.
package testgit

import "os"

// Identity is the author and committer of the commits tests make. Tests that need another one
// set GIT_AUTHOR_* and GIT_COMMITTER_* on their own command, which wins over this.
const (
	Name  = "Test"
	Email = "test@example.invalid"
)

// Hermetic sets git's environment for the whole test binary and everything it starts. Call it
// first in TestMain. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM point at /dev/null, and
// GIT_CONFIG_NOSYSTEM covers a git older than 2.32. The identity variables replace what
// user.name and user.email in the global config gave.
func Hermetic() {
	for k, v := range Env() {
		os.Setenv(k, v)
	}
}

// Env is the environment Hermetic sets.
func Env() map[string]string {
	return map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     Name,
		"GIT_AUTHOR_EMAIL":    Email,
		"GIT_COMMITTER_NAME":  Name,
		"GIT_COMMITTER_EMAIL": Email,
	}
}
