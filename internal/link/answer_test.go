package link

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
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
	sample := func() bool {
		p.mu.Lock()
		maxN, maxQ = max(maxN, p.ansN), max(maxQ, len(p.inq))
		dropped = dropped || p.ansDropped > 0
		p.mu.Unlock()
		return dropped
	}
	// Under load, starting the producer can consume the sampling window. Wait
	// for saturation, then retain the full stress interval and its heap bound.
	waitFor(t, "the query flood to reach the answer bound", sample)
	end := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(end) {
		sample()
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

// TestQueueInputsCancelGroup: when the caller gives up on a queued write, the writes queued
// after it in the same QueueInputs go with it, even before their own waits run: a message's
// Enter never goes out without its paste.
func TestQueueInputsCancelGroup(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(84)
	spawn(t, l, u, "stty -echo; echo ready; cat")
	waitFor(t, "the program", func() bool { return screenHas(t, l, u, "ready") })
	p := l.pane(u)

	p.mu.Lock()
	p.inBusy = true // a stuck writer: nothing drains the queue
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	waits := l.QueueInputs(ctx, u, []byte("paste"), []byte("enter\n"))
	if err := waits[0](); err != context.DeadlineExceeded {
		t.Fatalf("first write to a stuck queue: %v", err)
	}
	cancel()

	p.mu.Lock()
	p.inBusy = false
	p.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Input(ctx, u, []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the later input", func() bool { return screenHas(t, l, u, "after") })
	if screenHas(t, l, u, "enter") {
		t.Error("the write after a cancelled one was written")
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

// floodScript prints without end, always at the top left corner, so the screen never scrolls and
// the parser's scrollback stays empty.
const floodScript = `yes "$(printf '\033[Hx')" | tr -d '\n'`

// stallEventLoop stops the link's event loop until release, and waits until the holder client's
// event channel is full. From then on the client reads no frame from the holder: the holder's
// answers to requests wait behind output nobody reads.
func stallEventLoop(t *testing.T, l *Link) (release func()) {
	t.Helper()
	stop := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(stop) }) }
	t.Cleanup(release) // before the link's own cleanup closes it
	select {
	case l.tasks <- func() { <-stop }:
	case <-time.After(waitLong):
		t.Fatal("the event loop took no task")
	}
	waitFor(t, "a full event channel", func() bool {
		ev := l.client.Events()
		return len(ev) == cap(ev)
	})
	return release
}

// TestInputPipelined: a write to a pane goes out without waiting for the holder's answer to the
// write before it. The holder answers behind the pane's output, so under heavy output an answer
// comes long after its write landed. Here the answers never come while the event loop is
// stalled, yet both writes land.
func TestInputPipelined(t *testing.T) {
	hdir := shortDir(t)
	out := filepath.Join(shortDir(t), "seen")
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(85)
	spawn(t, l, u, `stty raw -echo
`+floodScript+` &
exec cat > `+out)
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "x") })
	release := stallEventLoop(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	seen := func(s string) func() bool {
		return func() bool { b, _ := os.ReadFile(out); return string(b) == s }
	}
	results := make(chan error, 2)
	go func() { results <- l.Input(ctx, u, []byte("one")) }()
	waitFor(t, "the first write", seen("one"))
	go func() { results <- l.Input(ctx, u, []byte("two")) }()
	waitFor(t, "the second write, while the first is unanswered", seen("onetwo"))
	release()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

// TestInputAbortedResent: writes sent behind a write the holder refuses fail on their offset, as
// aborted. The link sends them again, in order, at the offset the holder expects. The program
// reads no input, so the holder's input queue (1 MiB) fills.
func TestInputAbortedResent(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(86)
	spawn(t, l, u, "stty raw -echo; "+floodScript)
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "x") })
	start := time.Now()
	release := stallEventLoop(t, l)
	t.Logf("event loop stalled after %v", time.Since(start))

	// Queue three writes at once. All three go out before any answer is read: the first fits,
	// the second overflows the queue, the third is small but sent behind the refusal.
	p := l.pane(u)
	reqs := []*inputReq{
		{data: make([]byte, 900<<10), done: make(chan error, 1)},
		{data: make([]byte, 200<<10), done: make(chan error, 1)},
		{data: []byte("x"), done: make(chan error, 1)},
	}
	p.mu.Lock()
	for _, r := range reqs {
		p.queueInputLocked(l, r)
	}
	p.mu.Unlock()
	waitFor(t, "all three sent", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.inq) == 0
	})
	t.Logf("all sent after %v", time.Since(start))
	release()

	if err := <-reqs[0].done; err != nil {
		t.Errorf("first write: %v", err)
	}
	var re *holderclient.RequestError
	if err := <-reqs[1].done; !errors.As(err, &re) || re.Code != hp.CodeQueueFull {
		t.Errorf("second write: %v, want queue full", err)
	}
	if err := <-reqs[2].done; err != nil {
		t.Errorf("third write, sent again: %v", err)
	}
	t.Logf("all answered after %v", time.Since(start))
	p.mu.Lock()
	resent := p.resent
	p.mu.Unlock()
	if resent != 1 {
		t.Errorf("%d writes sent again; want the aborted third one", resent)
	}
	// The client's input offset follows the holder's: the next write lands at once.
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Input(ctx, u, []byte("y")); err != nil {
		t.Errorf("a later write: %v", err)
	}
}

// TestInputDeadline: an aborted write whose deadline passed before it could be sent again fails
// instead, so input never lands long after its caller gave up. Same writes as
// TestInputAbortedResent.
func TestInputDeadline(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(87)
	spawn(t, l, u, "stty raw -echo; "+floodScript)
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "x") })
	release := stallEventLoop(t, l)

	p := l.pane(u)
	reqs := []*inputReq{
		{data: make([]byte, 900<<10), done: make(chan error, 1)},
		{data: make([]byte, 200<<10), done: make(chan error, 1)},
		{data: []byte("x"), done: make(chan error, 1)},
	}
	p.mu.Lock()
	for _, r := range reqs {
		p.queueInputLocked(l, r)
	}
	p.mu.Unlock()
	waitFor(t, "all three sent", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.inq) == 0
	})
	// The third write's time runs out while it waits for the answers.
	p.mu.Lock()
	reqs[2].deadline = time.Now()
	p.mu.Unlock()
	release()

	if err := <-reqs[0].done; err != nil {
		t.Errorf("first write: %v", err)
	}
	var re *holderclient.RequestError
	if err := <-reqs[1].done; !errors.As(err, &re) || re.Code != hp.CodeQueueFull {
		t.Errorf("second write: %v, want queue full", err)
	}
	if err := <-reqs[2].done; !errors.Is(err, holderclient.ErrInputAborted) || !strings.Contains(err.Error(), "not written within") {
		t.Errorf("third write, past its deadline: %v; want it failed, not sent again", err)
	}
	p.mu.Lock()
	resent := p.resent
	p.mu.Unlock()
	if resent != 0 {
		t.Errorf("%d writes sent again", resent)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Input(ctx, u, []byte("y")); err != nil {
		t.Errorf("a later write: %v", err)
	}
}

// TestInputOtherWriter: another writer moves the holder's input count while this link has writes
// in flight.
// The first is refused for its offset and moves the client's offset to the holder's count; the
// second, sent in the same gen, is aborted; the third's offset happens to match the holder's
// count, so the holder takes it. The link must not send the first two again after the third,
// and the client's offset must count the third, so the next write needs no retry.
func TestInputOtherWriter(t *testing.T) {
	hdir := shortDir(t)
	out := filepath.Join(shortDir(t), "seen")
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(88)
	spawn(t, l, u, `stty raw -echo
`+floodScript+` &
exec cat > `+out)
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "x") })
	seen := func(s string) func() bool {
		return func() bool { b, _ := os.ReadFile(out); return string(b) == s }
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	release := stallEventLoop(t, l)
	// The other writer: an input frame sent with Send on the same holder connection, which the
	// client's offset does not count. The holder takes one connection at a time, so a second
	// connection cannot write while this one is open.
	hpane := l.client.Pane(u)
	if err := hpane.Holder.Send(&hp.Input{Handle: hpane.Handle, Token: hpane.Holder.Token(), Offset: 0, Data: []byte("ZZ")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the other writer's input", seen("ZZ"))

	p := l.pane(u)
	reqs := []*inputReq{
		{data: []byte("a"), done: make(chan error, 1)},
		{data: []byte("b"), done: make(chan error, 1)},
		{data: []byte("c"), done: make(chan error, 1)},
	}
	p.mu.Lock()
	for _, r := range reqs {
		p.queueInputLocked(l, r)
	}
	p.mu.Unlock()
	waitFor(t, "the third write, at the holder's count", seen("ZZc"))
	release()

	var re *holderclient.RequestError
	if err := <-reqs[0].done; !errors.As(err, &re) || re.Code != hp.CodeInputOffset {
		t.Errorf("first write: %v, want a wrong offset", err)
	}
	if err := <-reqs[1].done; !errors.Is(err, holderclient.ErrInputAborted) {
		t.Errorf("second write: %v, want aborted", err)
	}
	if err := <-reqs[2].done; err != nil {
		t.Errorf("third write: %v", err)
	}
	if err := l.Input(ctx, u, []byte("d")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the next write", func() bool { b, _ := os.ReadFile(out); return strings.HasSuffix(string(b), "d") })
	if b, _ := os.ReadFile(out); string(b) != "ZZcd" {
		t.Errorf("the program read %q, want %q", b, "ZZcd")
	}
	p.mu.Lock()
	resent := p.resent
	p.mu.Unlock()
	if resent != 0 {
		t.Errorf("%d writes sent again; want none", resent)
	}
}

// writeSignal wraps a control client's connection and reports each write, so a test can send
// calls in a known order without waiting for their replies.
type writeSignal struct {
	net.Conn
	wrote chan struct{}
}

func (w *writeSignal) Write(b []byte) (int, error) {
	n, err := w.Conn.Write(b)
	w.wrote <- struct{}{}
	return n, err
}

// linkInput serves pane.input for one pane from a link, as gff-hab does.
type linkInput struct {
	l *Link
	u hp.UUID
}

func (i linkInput) QueueInput(ctx context.Context, _, _ string, data []byte) func() error {
	return i.l.QueueInput(ctx, i.u, data)
}

// TestInputLaneStalled: keystrokes sent as separate pane.input calls, without waiting for
// replies, all reach the program in order while no holder answer is read. A control lane that
// waited for each call's result before queueing the next would write only the first.
func TestInputLaneStalled(t *testing.T) {
	hdir := shortDir(t)
	out := filepath.Join(shortDir(t), "seen")
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(89)
	spawn(t, l, u, `stty raw -echo
`+floodScript+` &
exec cat > `+out)
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "x") })

	srv := control.New(control.Config{Build: "test", Host: api.HostInfo{ID: "h"}, DeviceID: "SHA256:test",
		Source: emptySource{}, Input: linkInput{l, u}})
	t.Cleanup(srv.Close)
	a, b := net.Pipe()
	go srv.ServeConn(a)
	ws := &writeSignal{Conn: b, wrote: make(chan struct{}, 16)}
	c := control.NewClient(ws)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	<-ws.wrote

	release := stallEventLoop(t, l)
	start := time.Now()
	results := make(chan error, 3)
	for _, k := range []string{"l", "s", "\r"} {
		go func() { results <- c.Call(ctx, "pane.input", api.InputParams{Pane: "p", Text: k}, nil) }()
		<-ws.wrote
	}
	waitFor(t, "all three keys, while no answer is read", func() bool {
		b, _ := os.ReadFile(out)
		return string(b) == "ls\r"
	})
	t.Logf("landed after %v", time.Since(start))
	release()
	for range 3 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

// emptySource is a control.Source with nothing in it.
type emptySource struct{}

func (emptySource) Snapshot(api.HostInfo) api.Structure { return api.Structure{} }
