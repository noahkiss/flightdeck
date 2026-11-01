package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// instructionsCmd runs `gff-hab instructions render`: a dry run by default, one named target
// written with -write (docs/specs/instructions-mcp-flight-rules.md 6.5).
func instructionsCmd(args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] != "render" {
		return errors.New("instructions needs a subcommand: render")
	}
	fs := flag.NewFlagSet("instructions render", flag.ExitOnError)
	config := fs.String("config", os.Getenv("GFF_CONFIG"), "Flightdeck config file with an [instructions] table (default: $GFF_CONFIG)")
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	hostID := fs.String("host-id", os.Getenv("GFF_HAB_HOST_ID"), "host id for the sources' hosts lists (default: $GFF_HAB_HOST_ID, else the system host name)")
	fs.Bool("dry-run", true, "print a diff per target and write nothing (the default)")
	write := fs.String("write", "", "write this one target (claude, codex, opencode or pi); it must be in the config's write list")
	fs.Parse(args[1:])
	dryRunGiven := false
	fs.Visit(func(f *flag.Flag) { dryRunGiven = dryRunGiven || f.Name == "dry-run" })
	if dryRunGiven && *write != "" {
		return errors.New("instructions render: -dry-run and -write together; drop one")
	}
	if *config == "" {
		return errors.New("instructions render: no config file; pass -config or set $GFF_CONFIG")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	if *hostID == "" {
		if *hostID, err = os.Hostname(); err != nil {
			return err
		}
	}
	cfg, err := instructions.LoadConfig(*config, home)
	if err != nil {
		return err
	}
	r := &instructions.Renderer{Config: cfg, Env: instructions.EnvFrom(os.Getenv, home, dir), Host: *hostID}

	if *write == "" {
		for _, p := range r.PlanAll() {
			p.Report(stdout)
		}
		fmt.Fprintln(stdout, "dry run, nothing written")
		return nil
	}
	p, err := r.Write(instructions.Harness(*write))
	if p.Path != "" {
		p.Report(stdout)
	}
	if err != nil {
		return err
	}
	if p.Status == instructions.UpToDate {
		fmt.Fprintf(stdout, "%s is up to date; nothing written\n", p.Path)
	} else {
		fmt.Fprintf(stdout, "wrote %s\n", p.Path)
	}
	return nil
}
