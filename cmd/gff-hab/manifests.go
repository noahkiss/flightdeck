package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
)

// manifestsDir is where refreshed detection manifests live: manifests/ in the state directory.
func manifestsDir(state string) (string, error) {
	dir, err := stateDir(state)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "manifests"), nil
}

func manifests(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return errors.New("manifests needs a subcommand: refresh or list")
	}
	switch args[0] {
	case "refresh":
		return manifestsRefresh(args[1:], stdout)
	case "list":
		return manifestsList(args[1:], stdout)
	}
	return fmt.Errorf("manifests: unknown subcommand %q", args[0])
}

// manifestsRefresh pulls the catalogue, prints a diff for every manifest it would replace, and
// writes them into the state directory unless -dry-run is given.
func manifestsRefresh(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manifests refresh", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	from := fs.String("from", detect.DefaultCatalog, "catalogue: an index.toml URL, a local index.toml, or a directory holding one")
	dry := fs.Bool("dry-run", false, "print the diff and write nothing")
	timeout := fs.Duration("timeout", 60*time.Second, "give up after this long")
	fs.Parse(args)
	dir, err := manifestsDir(*state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := detect.Refresh(ctx, detect.RefreshOptions{From: *from, Dir: dir, DryRun: *dry})
	for _, c := range res.Changes {
		fmt.Fprint(stdout, c.Diff)
	}
	for _, p := range res.Skipped {
		fmt.Fprintf(stdout, "skipped %s\n", p)
	}
	verb := "updated"
	if *dry {
		verb = "would update"
	}
	for _, c := range res.Changes {
		old := c.OldVersion + " (" + c.OldFrom + ")"
		if c.OldVersion == "" {
			old = "none"
		}
		fmt.Fprintf(stdout, "%s %s: %s -> %s\n", verb, c.ID, old, c.NewVersion)
	}
	fmt.Fprintf(stdout, "%d to update, %d unchanged, %d skipped", len(res.Changes), len(res.Unchanged), len(res.Skipped))
	if *dry {
		fmt.Fprint(stdout, "; dry run, nothing written")
	} else if len(res.Changes) > 0 && err == nil {
		fmt.Fprintf(stdout, "; written to %s", dir)
	}
	fmt.Fprintln(stdout)
	return err
}

// manifestsList prints the active manifest for each agent and any manifest that could not be
// used. It exits 1 when there is such a problem, so a scheduled check can alert on drift.
func manifestsList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manifests list", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	fs.Parse(args)
	dir, err := manifestsDir(*state)
	if err != nil {
		return err
	}
	s := detect.Load(dir)
	fmt.Fprintf(stdout, "id\tversion\tfrom\tpath\n%s", s)
	for _, p := range s.Problems {
		fmt.Fprintf(stdout, "problem\t%s\n", p)
	}
	if len(s.Problems) > 0 {
		return fmt.Errorf("%d manifests could not be used", len(s.Problems))
	}
	return nil
}

// logManifestDrift loads the detection manifests at start and logs any that cannot be used.
func logManifestDrift(state string, logf func(string, ...any)) {
	dir := filepath.Join(state, "manifests")
	s := detect.Load(dir)
	if _, err := os.Stat(dir); err == nil {
		logf("detection manifests: %d active, data directory %s", len(s.Entries()), dir)
	}
	for _, p := range s.Problems {
		logf("detection manifest drift: %s", p)
	}
}
