package gffclient

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/vt"
)

// Legacy goldens: a key gff decoded from the host's kitty form reaches a pane that never
// pushed kitty flags, and gff's own sidebar and prompts, as xterm sends it with no
// modifyOtherKeys.
func TestLegacyKeyGoldens(t *testing.T) {
	for _, c := range []struct {
		k         vt.Key
		appCursor bool
		want      string
	}{
		{vt.Key{Code: 'a', Text: "a"}, false, "a"},
		{vt.Key{Code: 'a', Mods: 1}, false, "A"},
		{vt.Key{Code: 'c', Mods: 4}, false, "\x03"},
		{vt.Key{Code: 'c', Mods: 5}, false, "\x03"},
		{vt.Key{Code: 'i', Mods: 4}, false, "\t"},
		{vt.Key{Code: '[', Mods: 4}, false, "\x1b"},
		{vt.Key{Code: ' ', Mods: 4}, false, "\x00"},
		{vt.Key{Code: '/', Mods: 4}, false, "\x1f"},
		{vt.Key{Code: '1', Mods: 4}, false, "1"},
		{vt.Key{Code: 'x', Mods: 2}, false, "\x1bx"},
		{vt.Key{Code: 'x', Mods: 3}, false, "\x1bX"},
		{vt.Key{Code: 'x', Mods: 6}, false, "\x1b\x18"},
		{vt.Key{Code: 27}, false, "\x1b"},
		{vt.Key{Code: 27, Mods: 2}, false, "\x1b\x1b"},
		{vt.Key{Code: 13, Mods: 1}, false, "\r"},
		{vt.Key{Code: 13, Mods: 2}, false, "\x1b\r"},
		{vt.Key{Code: 9, Mods: 1}, false, "\x1b[Z"},
		{vt.Key{Code: 127}, false, "\x7f"},
		{vt.Key{Code: 127, Mods: 4}, false, "\x08"},
		{vt.Key{Code: 127, Mods: 2}, false, "\x1b\x7f"},
		{vt.Key{Code: 57399}, false, "0"},
		{vt.Key{Code: 57414}, false, "\r"},
		{vt.Key{Code: 57417}, false, "\x1b[D"},
		{vt.Key{Code: 57417}, true, "\x1bOD"},
		{vt.Key{Code: 57426}, true, "\x1b[3~"},
		{vt.Key{Code: 'a', Event: 3}, false, ""},
		{vt.Key{Code: 57441, Mods: 1}, false, ""},
		{vt.Key{Code: 233, Text: "é"}, false, "é"},
	} {
		if got := string(legacyKey(c.k, c.appCursor)); got != c.want {
			t.Errorf("%+v (app cursor %v): %q, want %q", c.k, c.appCursor, got, c.want)
		}
	}
}

// The re-encoder turns the host's CSI u key events into the reader's encoding, gives a text key
// its text, passes legacy sequences and a bracketed paste as they came, and holds a key event
// a read cut.
func TestKittyInTranslate(t *testing.T) {
	var keys []vt.Key
	rec := func(k vt.Key) []byte {
		keys = append(keys, k)
		return []byte(fmt.Sprintf("<%d/%d/%q>", k.Code, k.Mods, k.Text))
	}
	var x kittyIn
	got := string(x.translate([]byte("a\x1b[99;5u\x1b[A\x1b[200~\x1b[99;5u\x1b[201~\x1b[97;2"), rec, false))
	if want := "a<99/4/\"\">\x1b[A\x1b[200~\x1b[99;5u\x1b[201~"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := string(x.translate([]byte("u"), rec, false)); got != `<97/1/"A">` {
		t.Fatalf("the held event: %q", got)
	}
	if got := string(x.translate([]byte("aB\r\x1b"), rec, true)); got != `<97/0/"a"><98/1/"B"><13/0/"">`+"\x1b" {
		t.Fatalf("every key as an event: %q", got)
	}
	if got := string(x.translate([]byte("\x1b[57399u\x1b[1;5A"), func(k vt.Key) []byte { return legacyKey(k, false) }, false)); got != "0\x1b[1;5A" {
		t.Fatalf("legacy: %q", got)
	}
}

// gff takes the host's answer to its query out of the input, whole or cut by a read, and never
// takes anything else.
func TestKittyReplies(t *testing.T) {
	now := time.Unix(100, 0)
	c := &client{cfg: Config{Now: func() time.Time { return now }, Logf: t.Logf}, kittyAsked: now}
	if got := string(c.takeKittyReplies([]byte("a\x1b[?0ub"))); got != "ab" || !c.hostKitty {
		t.Fatalf("whole answer: %q, host kitty %v", got, c.hostKitty)
	}
	c.hostKitty = false
	if got := string(c.takeKittyReplies([]byte("x\x1b[?"))); got != "x" {
		t.Fatalf("cut answer: %q", got)
	}
	if got := string(c.takeKittyReplies([]byte("1u\x1b[A\x1b[1;5u"))); got != "\x1b[A\x1b[1;5u" || !c.hostKitty {
		t.Fatalf("rest of the answer: %q, host kitty %v", got, c.hostKitty)
	}
	now = now.Add(2 * kittyReplyWait)
	if got := string(c.takeKittyReplies([]byte("\x1b["))); got != "\x1b[" {
		t.Fatalf("long after the query a cut sequence is held: %q", got)
	}
}

// Against a real hab: gff asks the host for the kitty protocol. A host that never answers gets
// no push and no pop. A host that answers gets gff's flag pushed on the composed screen; keys it
// then sends in kitty form reach a legacy program as legacy bytes, Ctrl+O d still detaches, and
// gff pops its flag before it leaves the composed screen.
func TestKittyHost(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(fmt.Sprintf("answer %v", answer), func(t *testing.T) {
			h := startHab(t)
			other := h.other()
			a := spawn(t, other, "stty raw -echo; echo ready; head -c 4 | od -An -tx1; exec sleep 600", 40, 10)
			b := spawn(t, other, "echo right; exec sleep 600", 40, 10)
			hst := newHost(t, 80, 24, false)
			g := startGFF(t, h, hst, SplitColumns, PaneSpec{ID: a}, PaneSpec{ID: b})
			hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "ready") && strings.Contains(s, "right") })
			if !bytes.Contains(hst.output(), []byte(kittyQuery)) {
				t.Fatal("gff did not ask the host for the kitty protocol")
			}
			keys := "x\x03yz"
			if answer {
				hst.typ("\x1b[?0u")
				waitUntil(t, "the push", func() bool { return bytes.Contains(hst.output(), []byte(kittyPush)) })
				keys = "x\x1b[99;5u\x1b[27u\x1b[121;3u" // x, Ctrl+C, Esc, Alt+y
			}
			hst.typ(keys)
			want := "78 03 79 7a"
			if answer {
				want = "78 03 1b 1b"
			}
			hst.waitScreen("the program's bytes", func(s string) bool { return strings.Contains(strings.Join(strings.Fields(s), " "), want) })
			if answer {
				hst.typ("\x1b[111;5ud")
			} else {
				hst.typ("\x0fd")
			}
			select {
			case err := <-g.done:
				g.done <- err
				if err != ErrDetached {
					t.Fatalf("Run = %v, want ErrDetached", err)
				}
			case <-time.After(waitLong()):
				t.Fatal("Run did not return after detach")
			}
			out := hst.output()
			if !answer {
				if bytes.Contains(out, []byte("\x1b[>")) || bytes.Contains(out, []byte(kittyPop)) {
					t.Errorf("a host with no kitty protocol got a push or pop: %q", out)
				}
				return
			}
			if i, j := bytes.Index(out, []byte(enterCompose)), bytes.Index(out, []byte(kittyPush)); i < 0 || j < i {
				t.Errorf("the push came before the composed screen: compose at %d, push at %d", i, j)
			}
			if !bytes.HasSuffix(out, []byte(kittyPop+leaveCompose)) {
				t.Errorf("gff did not pop its flag as it left: ends %q", out[max(len(out)-40, 0):])
			}
		})
	}
}
