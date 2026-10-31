package link

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// answerScript reads the answer to a cursor position query in raw mode and prints it back,
// with the escape spelled out, then echoes one line of input the same way.
const answerScript = `stty raw -echo
printf 'ab\033[6n'
a=$(dd bs=1 count=6 2>/dev/null | od -An -c | tr -d ' \n')
printf '\r\ngot:%s\r\n' "$a"
b=$(dd bs=1 count=3 2>/dev/null)
printf 'typed:%s\r\n' "$b"
stty size
sleep 30`

// TestAnswersQueries: the link answers a live cursor position query once, from the parser's
// cursor just after the query; Input reaches the program in order after it; Resize changes the
// PTY and the parser, and tells subscribers.
func TestAnswersQueries(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(80)
	spawn(t, l, u, answerScript)
	waitFor(t, "the answer", func() bool { return screenHas(t, l, u, "got:033[1;3R") })

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	sub, err := l.Subscribe(ctx, u, FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := l.Resize(ctx, u, 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := l.Input(ctx, u, []byte("xyz")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the typed line and the new size", func() bool {
		return screenHas(t, l, u, "typed:xyz") && screenHas(t, l, u, "30 100")
	})
	resized := false
	for !resized {
		select {
		case ch := <-sub.C:
			resized = ch.Kind == ChunkResize && ch.Cols == 100 && ch.Rows == 30
		case <-ctx.Done():
			t.Fatal("no resize chunk")
		}
	}
	if err := l.Input(ctx, uuidN(81), []byte("x")); err != ErrNoPane {
		t.Errorf("input to an unknown pane: %v", err)
	}
	if err := l.Resize(ctx, u, 0, 10); err == nil {
		t.Error("a zero size was accepted")
	}
}

// TestQueryFlood: a program that floods device status queries gets a bounded answer queue, not
// one that grows with the flood, and client input during the flood still reaches it promptly.
// The program reads its input, so the holder never refuses it.
func TestQueryFlood(t *testing.T) {
	hdir := shortDir(t)
	out := filepath.Join(shortDir(t), "seen")
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(82)
	spawn(t, l, u, `stty raw -echo
yes "$(printf '\033[5n')" | tr -d '\n' &
grep -a -m1 -o MARK > `+out+`
kill $!
sleep 30`)
	p := l.pane(u)

	// Sample the queue while the flood runs.
	var maxN, maxQ int
	dropped := false
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	end := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(end) {
		p.mu.Lock()
		maxN, maxQ = max(maxN, p.ansN), max(maxQ, len(p.inq))
		dropped = dropped || p.ansDropped > 0
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if maxN > maxAnswers || maxQ > maxAnswers {
		t.Errorf("answer queue reached %d answers, %d writes; the bound is %d", maxN, maxQ, maxAnswers)
	}
	if !dropped {
		t.Error("no answer was dropped: the flood did not reach the bound")
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grow := int64(after.HeapAlloc) - int64(before.HeapAlloc); grow > 256<<20 {
		t.Errorf("the heap grew by %d MiB during the flood", grow>>20)
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- l.Input(ctx, u, []byte("MARK\n")) }()
	waitFor(t, "the program to read the input", func() bool {
		b, _ := os.ReadFile(out)
		return string(b) == "MARK\n"
	})
	t.Logf("input landed after %v", time.Since(start))
	if took := time.Since(start); took > scaleTimeout(2*time.Second) {
		t.Errorf("input during the flood took %v to land", took)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("input returned after %v", time.Since(start))
}

// TestInputTimeoutNeverWritten: input whose caller gave up while it waited in the queue is never
// written later, so a client that retries types it once.
func TestInputTimeoutNeverWritten(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(83)
	spawn(t, l, u, "stty -echo; echo ready; cat")
	waitFor(t, "the program", func() bool { return screenHas(t, l, u, "ready") })
	p := l.pane(u)

	// Hold the queue as a stuck writer would: nothing drains it.
	p.mu.Lock()
	p.inBusy = true
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := l.Input(ctx, u, []byte("never\n"))
	cancel()
	if err != context.DeadlineExceeded {
		t.Fatalf("input to a stuck queue: %v", err)
	}

	// The writer comes back; the next input goes through and the timed-out one does not.
	p.mu.Lock()
	p.inBusy = false
	p.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Input(ctx, u, []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second input", func() bool { return screenHas(t, l, u, "after") })
	if screenHas(t, l, u, "never") {
		t.Error("the timed-out input was written")
	}
}

// TestAnswersDroppedWhileRefused: once the holder refuses a pane's input as queue full (the
// program reads none), the link drops answers to its queries instead of queueing them.
func TestAnswersDroppedWhileRefused(t *testing.T) {
	hdir := shortDir(t)
	flag := filepath.Join(shortDir(t), "ask")
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(84)
	spawn(t, l, u, `stty raw -echo
echo ready
while [ ! -e `+flag+` ]; do sleep 0.02; done
printf '\033[5n'
echo asked
sleep 30`)
	waitFor(t, "the program", func() bool { return screenHas(t, l, u, "ready") })

	// Fill the holder's input queue (1 MiB) until it refuses.
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	chunk := make([]byte, 60<<10)
	for i := 0; ; i++ {
		if i > 40 {
			t.Fatal("the holder never refused input")
		}
		var re *holderclient.RequestError
		if err := l.Input(ctx, u, chunk); errors.As(err, &re) && re.Code == hp.CodeQueueFull {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	p := l.pane(u)
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the query", func() bool { return screenHas(t, l, u, "asked") })
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ansRefused || p.ansDropped != 1 || p.ansN != 0 || len(p.inq) != 0 {
		t.Errorf("refused %v, dropped %d, %d answers and %d writes queued; want the answer dropped",
			p.ansRefused, p.ansDropped, p.ansN, len(p.inq))
	}
}
