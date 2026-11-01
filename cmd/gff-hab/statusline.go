package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// maxStatusInput bounds the status-line input read from stdin.
const maxStatusInput = 1 << 20

// statuslineCmd is what Claude Code's statusLine setting runs:
// `gff-hab statusline claude [-- command...]` with the status-line input on stdin. It posts the
// input to gff-hab, which reads the account's usage windows from it (harness-adapters.md,
// Usage), and runs the owner's own status-line command, if one follows --, with the same input.
// That command's output, errors and exit code are the status line's; with none, it prints
// nothing. The post runs beside the command and is capped at hook.Budget; outside a pane
// gff-hab owns it posts nothing. `-print claude` prints the setting instead.
func statuslineCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	print := fs.String("print", "", "print the statusLine setting for this harness (claude) and exit")
	command := fs.String("command", "gff-hab", "with -print: how the harness should run gff-hab")
	wrap := fs.String("wrap", "", "with -print: your own status-line command, run with the same input")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
		return 2
	}
	if *print != "" {
		if *print != detect.KindClaude {
			fmt.Fprintf(stderr, "gff-hab: no status line for harness %q; known: claude\n", *print)
			return 2
		}
		cfg, err := hook.StatusLineConfig(*command, *wrap)
		if err != nil {
			fmt.Fprintf(stderr, "gff-hab: %v\n", err)
			return 2
		}
		fmt.Fprint(stdout, cfg)
		return 0
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "gff-hab statusline: name the harness: claude")
		return 2
	}
	harness, rest := rest[0], rest[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	input, _ := io.ReadAll(io.LimitReader(stdin, maxStatusInput))

	posted := make(chan struct{})
	go func() {
		defer close(posted)
		if harness != detect.KindClaude {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), hook.Budget)
		defer cancel()
		err := hook.Run(ctx, hook.Invocation{Harness: harness, Event: hook.StatusLineEvent, Stdin: bytes.NewReader(input), Getenv: os.Getenv})
		if err != nil && !errors.Is(err, hook.ErrNoHab) && os.Getenv("GFF_HOOK_DEBUG") != "" {
			fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
		}
	}()
	code := 0
	if len(rest) > 0 {
		// One shell string, as Claude Code runs its own statusLine command; several words are
		// joined with spaces first.
		cmd := exec.Command("/bin/sh", "-c", strings.Join(rest, " "))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), stdout, stderr
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = max(ee.ExitCode(), 1) // -1 for a signal
			} else {
				fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
				code = 1
			}
		}
	}
	select {
	case <-posted:
	case <-time.After(hook.Budget + 50*time.Millisecond):
	}
	return code
}
