package link

import (
	"context"
	"testing"
	"time"
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
