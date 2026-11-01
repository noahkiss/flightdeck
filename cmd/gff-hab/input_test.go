package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

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

// TestTypingDuringQueryFlood: "ls" and Enter typed as three pane.input calls, each sent without
// waiting for the reply to the one before, land together while the program floods device
// status queries. The holder answers each write behind the pane's heavy output, so the line
// lands before the reply to its first key; a key that waited for the reply to the key before
// would land only after it.
func TestTypingDuringQueryFlood(t *testing.T) {
	e := newHabEnv(t, 0)
	pane := e.spawn(e.control(), `stty -icanon -echo
yes "$(printf '\033[5n')" | tr -d '\n' &
touch "$DIR/flooding"
grep -a -m1 "" | tr -dc "ls\\n" > "$DIR/seen"
kill $!
sleep 30`)
	waitUntil(t, "the flood", func() bool {
		_, err := os.Stat(filepath.Join(e.work, "flooding"))
		return err == nil
	})

	nc, err := net.Dial("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	ws := &writeSignal{Conn: nc, wrote: make(chan struct{}, 16)}
	c := control.NewClient(ws)
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	<-ws.wrote

	// Wait until the holder's answers lag behind the output: probe with a key the program
	// ignores until one reply takes 200 ms.
	for {
		t0 := time.Now()
		if err := c.Call(ctx, "pane.input", api.InputParams{Pane: pane, Text: "x"}, nil); err != nil {
			t.Fatal(err)
		}
		<-ws.wrote
		if lag := time.Since(t0); lag >= 200*time.Millisecond {
			t.Logf("a reply lags %v", lag)
			break
		}
	}

	start := time.Now()
	type result struct {
		at  time.Time
		err error
	}
	results := make([]chan result, 3)
	for i, k := range []string{"l", "s", "\r"} {
		results[i] = make(chan result, 1)
		go func() {
			err := c.Call(ctx, "pane.input", api.InputParams{Pane: pane, Text: k}, nil)
			results[i] <- result{time.Now(), err}
		}()
		<-ws.wrote // the next key goes out after this one, not after its reply
	}
	// The terminal turns Enter into a newline, which ends grep's line. The line holds answers
	// to queries and the probes too, maybe between the keys; tr keeps the keys.
	for {
		if b, _ := os.ReadFile(filepath.Join(e.work, "seen")); string(b) == "ls\n" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("timed out waiting for the typed line")
		}
		time.Sleep(2 * time.Millisecond)
	}
	landed := time.Now()
	t.Logf("typed line landed after %v", landed.Sub(start))
	for i := range results {
		r := <-results[i]
		if r.err != nil {
			t.Error(r.err)
		}
		if i == 0 {
			t.Logf("first reply after %v", r.at.Sub(start))
			if !landed.Before(r.at) {
				t.Errorf("the typed line landed %v after the reply to its first key; want before", landed.Sub(r.at))
			}
		}
	}
	t.Logf("all replies after %v", time.Since(start))
}
