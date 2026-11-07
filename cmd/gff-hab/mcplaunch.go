package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/noahkiss/flightdeck/internal/mcpreg"
)

// mcpLaunchCmd runs `gff-hab mcp launch [-config file] <name>`: what a harness runs for a
// stdio server with secret or command values (docs/specs/instructions-mcp-flight-rules.md
// 8.5). It reads the registry itself, so it works from a plain terminal with no gff-hab
// running, resolves each value in this process, and execs the server with them in its
// environment. No value is printed, logged or written.
func mcpLaunchCmd(args []string) error {
	fs := flag.NewFlagSet("mcp launch", flag.ExitOnError)
	config := fs.String("config", os.Getenv("GFF_CONFIG"), "Flightdeck config file with an [mcp] table (default: $GFF_CONFIG)")
	fs.Parse(args)
	name, cfg, err := mcpResolveArgs("mcp launch", fs, *config)
	if err != nil {
		return err
	}
	r := &mcpreg.Resolver{Config: cfg}
	l, err := r.Launch(context.Background(), name, os.Environ())
	if err != nil {
		return err
	}
	if l.Dir != "" {
		if err := os.Chdir(l.Dir); err != nil {
			return fmt.Errorf("mcp launch: server %s: %v", name, err)
		}
	}
	err = syscall.Exec(l.Path, l.Argv, l.Env)
	return fmt.Errorf("mcp launch: server %s: exec %s: %v", name, l.Argv[0], err)
}

// mcpHeaderCmd runs `gff-hab mcp header [-config file] <name>`: the header helper a harness
// runs for an http server with secret or command headers (8.5). It prints one JSON object of
// those headers on stdout, which the harness reads; nothing else holds them.
func mcpHeaderCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mcp header", flag.ExitOnError)
	config := fs.String("config", os.Getenv("GFF_CONFIG"), "Flightdeck config file with an [mcp] table (default: $GFF_CONFIG)")
	fs.Parse(args)
	name, cfg, err := mcpResolveArgs("mcp header", fs, *config)
	if err != nil {
		return err
	}
	r := &mcpreg.Resolver{Config: cfg}
	h, err := r.Headers(context.Background(), name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(b, '\n'))
	clear(b)
	return err
}

func mcpResolveArgs(verb string, fs *flag.FlagSet, config string) (string, mcpreg.Config, error) {
	if fs.NArg() != 1 {
		return "", mcpreg.Config{}, fmt.Errorf("%s: name one server", verb)
	}
	if config == "" {
		return "", mcpreg.Config{}, errors.New(verb + ": no config file; pass -config or set $GFF_CONFIG")
	}
	cfg, err := mcpreg.LoadConfig(config)
	if err != nil {
		return "", mcpreg.Config{}, err
	}
	return fs.Arg(0), cfg, nil
}
