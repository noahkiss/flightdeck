// Package version answers the version request that gff, gff-hab and gff-hab-holder share.
//
// Each command holds its own `var version = "dev"`, which a release build sets at link time with
// -ldflags "-X main.version=<version>" (docs/specs/ci-release.md 7.3). The version comes from the
// release tag without its leading v: 1.2.3, 1.2.3-rc.1 or 1.2.3-dry.1.
package version

import (
	"fmt"
	"io"
)

// Requested reports whether a command's arguments (os.Args[1:]) ask for its version:
// `version`, `-version` or `--version` as the first argument.
func Requested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "-version", "--version":
		return true
	}
	return false
}

// Print writes "<name> <version>" and a newline to w.
func Print(w io.Writer, name, version string) {
	fmt.Fprintf(w, "%s %s\n", name, version)
}
