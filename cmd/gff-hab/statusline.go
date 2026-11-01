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
	"os/signal"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// maxStatusInput bounds the status-line input read from stdin.
const maxStatusInput = 1 << 20

// statusPostBudget caps the status line's post to gff-hab, dial and answer. A local post takes a
// few milliseconds; Claude Code refreshes the status line many times a turn and waits for it.
const statusPostBudget = 200 * time.Millisecond

// statusTimeout is the default cap on the owner's status-line command.
const statusTimeout = 10 * time.Second

// statuslineCmd is what Claude Code's statusLine setting runs:
// `gff-hab statusline claude [-- command...]` with the status-line input on stdin. It posts the
// input to gff-hab, which reads the account's usage windows from it (harness-adapters.md,
// Usage), and runs the owner's own status-line command, if one follows --, with the same input.
// That command's output, errors and exit code are the status line's; with none, it prints
// nothing. The post runs beside the command and is capped at statusPostBudget; outside a pane
// gff-hab owns it posts nothing. `-print claude` prints the setting instead.
func statuslineCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	print := fs.String("print", "", "print the statusLine setting for this harness (claude) and exit")
	command := fs.String("command", "gff-hab", "with -print: how the harness should run gff-hab")
	wrap := fs.String("wrap", "", "with -print: your own status-line command, run with the same input")
	timeout := fs.Duration("timeout", statusTimeout, "cap on the status-line command; past it the command is killed")
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
	harness := rest[0]
	// Flags may follow the harness too; -- ends them.
	if err := fs.Parse(rest[1:]); err != nil {
		fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
		return 2
	}
	rest = fs.Args()
	input, _ := io.ReadAll(io.LimitReader(stdin, maxStatusInput))

	// The post is a snapshot Claude Code sends again on the next refresh, so losing one costs
	// nothing; a hung gff-hab must never hold the status line. Its budget runs from here.
	deadline := time.Now().Add(statusPostBudget)
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		if harness != detect.KindClaude {
			return
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		err := hook.Run(ctx, hook.Invocation{Harness: harness, Event: hook.StatusLineEvent, Stdin: bytes.NewReader(input), Getenv: os.Getenv})
		if err != nil && !errors.Is(err, hook.ErrNoHab) && os.Getenv("GFF_HOOK_DEBUG") != "" {
			fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
		}
	}()
	code := 0
	if len(rest) > 0 {
		code = runStatusCommand(rest, input, *timeout, stdout, stderr)
	}
	select {
	case <-posted:
	case <-time.After(time.Until(deadline) + 20*time.Millisecond):
	}
	return code
}

// runStatusCommand runs the owner's status-line command with input on stdin and returns its exit
// code. One word is a shell string, as Claude Code runs its own statusLine command; several
// words are the argv, run as is. The command runs in its own process group: SIGTERM, SIGINT and
// SIGHUP to gff-hab go to the whole group, and the group is killed a second later, or when the
// command runs past timeout. A cancelled status line leaves no process behind.
func runStatusCommand(argv []string, input []byte, timeout time.Duration, stdout, stderr io.Writer) int {
	if len(argv) == 1 {
		argv = []string{"/bin/sh", "-c", argv[0]}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
		return 127
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigs:
			syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
			select {
			case <-time.After(time.Second):
				cancel() // kills the group
			case <-done:
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return max(ee.ExitCode(), 1) // -1 for a signal
	}
	fmt.Fprintf(stderr, "gff-hab statusline: %v\n", err)
	return 1
}
