package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/mcpreg"
)

// mcpRenderCmd runs `gff-hab mcp render`: the MCP registry rendered into each harness's config,
// a dry run by default, one harness written with -write
// (docs/specs/instructions-mcp-flight-rules.md 8.4).
func mcpRenderCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mcp render", flag.ExitOnError)
	config := fs.String("config", os.Getenv("GFF_CONFIG"), "Flightdeck config file with an [mcp] table (default: $GFF_CONFIG)")
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	hostID := fs.String("host-id", os.Getenv("GFF_HAB_HOST_ID"), "host id for the servers' hosts lists (default: $GFF_HAB_HOST_ID, else the system host name)")
	fs.Bool("dry-run", true, "print each harness's plan and diff and write nothing (the default)")
	write := fs.String("write", "", "write this one harness's config (claude, codex or opencode); it must be in the config's write list")
	claudeBin := fs.String("claude-bin", "claude", "the claude program whose own writer changes ~/.claude.json")
	launcher := fs.String("launcher", "gff-hab", "the gff-hab program a server with secrets runs through (gff-hab mcp launch and mcp header)")
	var force stringList
	fs.Var(&force, "force", "replace or remove this owned server though it was edited by hand (repeatable)")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("mcp render: unexpected argument %q", fs.Arg(0))
	}
	dryRunGiven := false
	fs.Visit(func(f *flag.Flag) { dryRunGiven = dryRunGiven || f.Name == "dry-run" })
	if dryRunGiven && *write != "" {
		return errors.New("mcp render: -dry-run and -write together; drop one")
	}
	if *config == "" {
		return errors.New("mcp render: no config file; pass -config or set $GFF_CONFIG")
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
	cfg, err := mcpreg.LoadConfig(*config)
	if err != nil {
		return err
	}
	cfgPath, err := filepath.Abs(*config)
	if err != nil {
		return err
	}
	r := &mcpreg.Renderer{Config: cfg, Env: mcpreg.EnvFrom(os.Getenv, home, dir), Host: *hostID,
		Force: force, Claude: mcpreg.ExecClaude(*claudeBin),
		Launcher: mcpreg.Launcher{Program: *launcher, Config: cfgPath}}
	if *write == "" {
		for _, p := range r.PlanAll() {
			p.Report(stdout)
		}
		fmt.Fprintln(stdout, "dry run, nothing written")
		return nil
	}
	p, err := r.Write(context.Background(), mcpreg.Harness(*write))
	if p.Harness != "" {
		p.Report(stdout)
	}
	if err != nil {
		return err
	}
	switch p.Status {
	case mcpreg.UpToDate, mcpreg.Nothing:
		fmt.Fprintf(stdout, "%s: nothing to write\n", p.Path)
	default:
		fmt.Fprintf(stdout, "wrote %s\n", p.Path)
	}
	return nil
}
