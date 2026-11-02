// Command notices writes NOTICE and apple/App/Acknowledgements.txt from third_party/notices.toml,
// go.mod with the module cache, and apple/FlightdeckKit/Package.resolved. With -check it writes
// nothing and exits 1 when either file differs from what it would write.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/notices"
)

func main() {
	root := flag.String("root", ".", "the repository root")
	check := flag.Bool("check", false, "write nothing; exit 1 when a file is stale")
	goCmd := flag.String("go", "go", "the go command")
	flag.Parse()

	in := notices.Inputs{
		Root:       *root,
		GoBinaries: notices.BinaryNames(notices.ReleaseBinaries),
		GoModules: func() ([]notices.GoModule, error) {
			return notices.ListGoModules(*root, *goCmd, notices.ReleaseBinaries, notices.ReleasePlatforms)
		},
	}
	goroot, err := notices.GoRoot(*root, *goCmd)
	if err != nil {
		fail(err)
	}
	in.GoRoot = goroot

	stale := false
	for _, out := range []struct{ set, path string }{
		{notices.SetRelease, notices.ReleaseOutput},
		{notices.SetApp, notices.AppOutput},
	} {
		data, err := notices.Generate(out.set, in)
		if err != nil {
			fail(err)
		}
		path := filepath.Join(*root, filepath.FromSlash(out.path))
		old, _ := os.ReadFile(path)
		if bytes.Equal(old, data) {
			continue
		}
		if *check {
			fmt.Fprintf(os.Stderr, "%s is stale: run make notices\n", out.path)
			stale = true
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("wrote %s\n", out.path)
	}
	if stale {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "notices: %v\n", err)
	os.Exit(1)
}
