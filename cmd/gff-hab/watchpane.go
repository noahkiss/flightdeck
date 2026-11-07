package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// `gff-hab watch-pane -round <id> [limits] -- <program> [args...]` is what a watcher's pane runs
// (rounds.md 4.4), as opencode-pane wraps opencode. It starts the program with its stdout on a
// pipe and its stderr on the pane, reads the event lines, and posts them to gff-hab through the
// pane's $GFF_HAB endpoint (round.watch_event). The pane shows one line per event and, at most
// once per 10 s, the lines it dropped. While gff-hab does not answer it keeps up to
// -buffer-lines lines or -buffer-bytes, drops the oldest past that, and sends the rest when
// gff-hab answers again. It exits with the program's status.

// Wrapper timing.
const (
	watchDropEvery   = 10 * time.Second // the fixed pace of the drop line (rounds.md 4.7)
	watchPageLines   = 200
	watchPageBytes   = 768 << 10 // under the pane route's 1 MiB body
	watchRetryFirst  = 250 * time.Millisecond
	watchRetryMax    = 2 * time.Second
	watchCallTimeout = 30 * time.Second
	watchDrainWait   = 10 * time.Second // after the program exits, for gff-hab to take the rest
)

// watchPane holds the wrapper's queue of lines and its drop count.
type watchPane struct {
	round string
	addr  string
	out   io.Writer // the pane

	mu       sync.Mutex
	cond     *sync.Cond
	q        []string
	qBytes   int
	maxLines int
	maxBytes int
	ended    bool // the program's stdout closed
	dropped  int  // lines dropped here since the last page sent
	// Drop notes for the pane, paced to one line per watchDropEvery.
	noteN    int
	noteWhy  string
	noteLast time.Time

	// The rate bucket (rounds.md 4.4): rate events a minute, a burst of burst.
	rate, burst float64
	tokens      float64
	filled      time.Time
	now         func() time.Time
}

func watchPaneCmd(args []string) int {
	fs := flag.NewFlagSet("watch-pane", flag.ContinueOnError)
	round := fs.String("round", "", "the round id")
	def := rounds.DefaultWatchLimits()
	rate := fs.Int("rate", def.Rate, "events a minute")
	burst := fs.Int("burst", def.Burst, "the rate's burst")
	bufLines := fs.Int("buffer-lines", def.BufferLines, "lines kept while gff-hab does not answer")
	bufBytes := fs.Int("buffer-bytes", def.BufferBytes, "bytes kept while gff-hab does not answer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	addr := os.Getenv(hook.Env)
	if *round == "" || len(argv) == 0 || argv[0] == "" || addr == "" {
		fmt.Fprintf(os.Stderr, "gff-hab watch-pane: needs -round, a program after --, and $%s; gff-hab sets them when it starts a watcher\n", hook.Env)
		return 2
	}
	w := &watchPane{round: *round, addr: addr, out: os.Stdout, maxLines: *bufLines, maxBytes: *bufBytes,
		rate: float64(max(*rate, 1)), burst: float64(max(*burst, 1)), now: time.Now}
	w.cond = sync.NewCond(&w.mu)
	w.tokens, w.filled = w.burst, w.now()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab watch-pane: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gff-hab watch-pane: start %s: %v\n", argv[0], err)
		return 127
	}
	// A hang-up or a stop reaches the program; the wrapper stays to send what it holds.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	sent := make(chan struct{})
	go func() {
		w.send()
		close(sent)
	}()
	w.read(stdout)
	err = cmd.Wait()
	signal.Stop(sigs)
	select {
	case <-sent:
	case <-time.After(watchDrainWait):
		w.mu.Lock()
		if n := len(w.q); n > 0 {
			fmt.Fprintf(w.out, "watch-pane: gff-hab did not answer; %d lines were not sent\n", n)
		}
		w.mu.Unlock()
	}
	w.flushNote(true)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return 128 + int(st.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

// read reads the program's stdout line by line until it closes. A line over the 64 KiB ceiling,
// or one over the rate, is dropped here; the rest join the queue.
func (w *watchPane) read(r io.Reader) {
	br := bufio.NewReaderSize(r, rounds.MaxWatchLine+1)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Past the ceiling: drop the line, with the rest of it up to its newline.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			w.drop(fmt.Sprintf("line over %d KiB", rounds.MaxWatchLine>>10))
			if err != nil {
				break
			}
			continue
		}
		if len(line) > 0 {
			w.add(strings.TrimRight(string(line), "\r\n"))
		}
		if err != nil {
			break
		}
	}
	w.mu.Lock()
	w.ended = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// add queues one line after the rate check; past the buffer the oldest lines go.
func (w *watchPane) add(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.tokens = min(w.burst, w.tokens+now.Sub(w.filled).Minutes()*w.rate)
	w.filled = now
	if w.tokens < 1 {
		w.dropLocked("over the rate")
		return
	}
	w.tokens--
	w.q = append(w.q, line)
	w.qBytes += len(line)
	over := 0
	for len(w.q) > 0 && (len(w.q) > w.maxLines || w.qBytes > w.maxBytes) {
		w.qBytes -= len(w.q[0])
		w.q = w.q[1:]
		over++
	}
	for i := 0; i < over; i++ {
		w.dropLocked("past the buffer while gff-hab did not answer")
	}
	w.cond.Broadcast()
}

func (w *watchPane) drop(why string) {
	w.mu.Lock()
	w.dropLocked(why)
	w.mu.Unlock()
}

func (w *watchPane) dropLocked(why string) {
	w.dropped++
	w.noteLocked(why)
}

// noteLocked counts a drop for the pane's drop line.
func (w *watchPane) noteLocked(why string) {
	w.noteN++
	w.noteWhy = why
	w.flushNoteLocked(false)
}

// flushNoteLocked prints the drop line when watchDropEvery passed since the last, or when force.
func (w *watchPane) flushNoteLocked(force bool) {
	if w.noteN == 0 || (!force && w.now().Sub(w.noteLast) < watchDropEvery) {
		return
	}
	fmt.Fprintf(w.out, "watch-pane: dropped %d lines; the last: %s\n", w.noteN, paneSafe(w.noteWhy))
	w.noteN, w.noteLast = 0, w.now()
}

func (w *watchPane) flushNote(force bool) {
	w.mu.Lock()
	w.flushNoteLocked(force)
	w.mu.Unlock()
}

// send posts the queue a page at a time until the program's stdout closed and the queue is
// empty. A gff-hab that does not answer is tried again; the lines wait.
func (w *watchPane) send() {
	delay := watchRetryFirst
	for {
		w.mu.Lock()
		for len(w.q) == 0 && !w.ended {
			w.cond.Wait()
		}
		if len(w.q) == 0 && w.ended {
			w.mu.Unlock()
			return
		}
		n, size := 0, 0
		for n < len(w.q) && n < watchPageLines && (n == 0 || size+len(w.q[n]) <= watchPageBytes) {
			size += len(w.q[n])
			n++
		}
		page := append([]string(nil), w.q[:n]...)
		dropped := w.dropped
		w.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), watchCallTimeout)
		var res api.RoundWatchEventResult
		err := hook.Call(ctx, w.addr, "round.watch_event", api.RoundWatchEventParams{Round: w.round, Lines: page, Dropped: dropped}, &res)
		cancel()
		if err != nil && watchRetryable(err) {
			time.Sleep(delay)
			delay = min(2*delay, watchRetryMax)
			continue
		}
		delay = watchRetryFirst
		w.mu.Lock()
		w.q = w.q[n:]
		for _, l := range page {
			w.qBytes -= len(l)
		}
		w.dropped -= dropped
		if err != nil {
			// gff-hab refused the page (the round is gone, or this pane is not its watcher's):
			// its lines are dropped and said so.
			fmt.Fprintf(w.out, "watch-pane: gff-hab refused %d lines: %s\n", len(page), paneSafe(err.Error()))
			w.mu.Unlock()
			continue
		}
		for _, l := range res.Lines {
			switch {
			case l.Taken:
				fmt.Fprintf(w.out, "event: %s\n", paneSafe(l.Title))
			case l.Drop != "" && !l.Quiet:
				w.noteLocked(l.Drop)
			}
		}
		w.mu.Unlock()
	}
}

// watchRetryable says whether a failed call should be tried again: gff-hab is down, restarting,
// or does not know the pane's token yet. An API error is gff-hab's answer, not tried again.
func watchRetryable(err error) bool {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind == api.ErrorKindUnavailable
	}
	var se *hook.StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusNotFound || se.Code >= 500
	}
	return true
}

// paneSafe keeps a line from outside (a mail's subject) from driving the pane's terminal: every
// control character becomes U+FFFD.
func paneSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '�'
		}
		return r
	}, s)
}
