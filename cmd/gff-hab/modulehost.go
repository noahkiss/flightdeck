package main

import (
	"fmt"
	"os"

	"github.com/noahkiss/flightdeck/internal/modhost"
)

// moduleHostCmd is the module host process (modules.md 4, 7.4): gff-hab starts it as
// "gff-hab module-host" through a modhost.Host and speaks the module pipe on its stdin and
// stdout. It ends when stdin closes.
func moduleHostCmd(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: gff-hab module-host (gff-hab starts it; it reads the module pipe on stdin)")
		return 2
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(os.Stderr, "gff-hab module-host: gff-hab starts this; it reads the module pipe on stdin, not a terminal")
		return 2
	}
	if err := modhost.ServeChild(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab module-host: %v\n", err)
		return 1
	}
	return 0
}
