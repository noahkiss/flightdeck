package worktree

import "os/exec"

// OwnGroup starts cmd in a process group of its own, where the platform has them, so that
// cancelling it ends the command's children too.
func OwnGroup(cmd *exec.Cmd) { ownGroup(cmd) }
