package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// `gff round` is `gff-hab round` (docs/specs/rounds.md 13): the round commands live in gff-hab,
// beside its pane door and peer list, and gff runs that command in its own place with the same
// arguments, environment and stdio. A design call: one implementation of the verbs, which the
// agent kit's MCP round tools share.

// habNames are where gff looks for gff-hab, beside its own executable: the plain binary (a
// Linux install, `gff-hab install`'s bin folder), then the macOS helper bundle (Homebrew's
// libexec holds gff beside GFF Hab.app).
var habNames = []string{"gff-hab", filepath.Join("GFF Hab.app", "Contents", "MacOS", "gff-hab")}

// findHab returns the gff-hab to run: beside exe (as given, then with links resolved), else the
// first on PATH.
func findHab(exe string, lookPath func(string) (string, error)) (string, error) {
	dirs := []string{}
	if exe != "" {
		dirs = append(dirs, filepath.Dir(exe))
		if r, err := filepath.EvalSymlinks(exe); err == nil && filepath.Dir(r) != dirs[0] {
			dirs = append(dirs, filepath.Dir(r))
		}
	}
	for _, d := range dirs {
		for _, n := range habNames {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	p, err := lookPath("gff-hab")
	if err != nil {
		return "", errors.New("gff round runs gff-hab round, and no gff-hab is beside gff or on PATH")
	}
	return p, nil
}

// roundMain replaces gff with `gff-hab round <args>`; it returns only when that cannot start.
func roundMain(args []string, stderr io.Writer) int {
	exe, _ := os.Executable()
	hab, err := findHab(exe, exec.LookPath)
	if err == nil {
		err = syscall.Exec(hab, append([]string{"gff-hab", "round"}, args...), os.Environ())
	}
	fmt.Fprintf(stderr, "gff round: %v\n", err)
	return 1
}
