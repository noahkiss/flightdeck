//go:build !unix

package servechild

import "os/exec"

// ownSession does nothing here: the pane caller check runs on Unix only.
func ownSession(*exec.Cmd) {}
