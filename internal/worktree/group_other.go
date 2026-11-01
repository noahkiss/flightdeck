//go:build !unix

package worktree

import "os/exec"

// ownGroup does nothing here: cancelling cmd kills only the script itself.
func ownGroup(*exec.Cmd) {}
