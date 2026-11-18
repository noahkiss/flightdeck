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
// modifyOtherKeys, in each of the pane's cursor and keypad modes.
func TestLegacyKeyGoldens(t *testing.T) {
	normal, appCursor, appKeypad := legacyModes{}, legacyModes{appCursor: true}, legacyModes{appKeypad: true}
	for _, c := range []struct {
		k    vt.Key
		m    legacyModes
		want string
	}{
		{vt.Key{Code: 'a', Text: "a"}, normal, "a"},
		{vt.Key{Code: 'a', Mods: 1}, normal, "A"},
		{vt.Key{Code: 'c', Mods: 4}, normal, "\x03"},
		{vt.Key{Code: 'c', Mods: 5, Shifted: 'C'}, normal, "\x03"},
		{vt.Key{Code: 'i', Mods: 4}, normal, "\t"},
		{vt.Key{Code: '[', Mods: 4}, normal, "\x1b"},
		{vt.Key{Code: ' ', Mods: 4}, normal, "\x00"},
		{vt.Key{Code: '/', Mods: 4}, normal, "\x1f"},
		{vt.Key{Code: '1', Mods: 4}, normal, "1"},
		// Ctrl over the shifted key (review finding 3): Ctrl+_ and Ctrl+?.
		{vt.Key{Code: '-', Shifted: '_', Mods: 5}, normal, "\x1f"},
		{vt.Key{Code: '/', Shifted: '?', Mods: 5}, normal, "\x7f"},
		// Ctrl with a key that has no control form types the key (finding 8).
		{vt.Key{Code: 'é', Mods: 4}, normal, "é"},
		{vt.Key{Code: 'x', Mods: 2}, normal, "\x1bx"},
		{vt.Key{Code: 'x', Mods: 3}, normal, "\x1bX"},
		{vt.Key{Code: 'x', Mods: 6}, normal, "\x1b\x18"},
		// Alt over the shifted key (finding 2): Alt+Shift+. is Alt+>, Alt+Shift+1 Alt+!.
		{vt.Key{Code: '.', Shifted: '>', Mods: 3}, normal, "\x1b>"},
		{vt.Key{Code: '1', Shifted: '!', Mods: 3}, normal, "\x1b!"},
		// Caps Lock upper-cases a letter, as a legacy terminal's text does (finding 7).
		{vt.Key{Code: 'a', Mods: 2 | 64}, normal, "\x1bA"},
		{vt.Key{Code: 'a', Mods: 1 | 2 | 64}, normal, "\x1ba"},
		{vt.Key{Code: 27}, normal, "\x1b"},
		{vt.Key{Code: 27, Mods: 2}, normal, "\x1b\x1b"},
		{vt.Key{Code: 13, Mods: 1}, normal, "\r"},
		{vt.Key{Code: 13, Mods: 2}, normal, "\x1b\r"},
		{vt.Key{Code: 9, Mods: 1}, normal, "\x1b[Z"},
		{vt.Key{Code: 127}, normal, "\x7f"},
		{vt.Key{Code: 127, Mods: 4}, normal, "\x08"},
		{vt.Key{Code: 127, Mods: 2}, normal, "\x1b\x7f"},
		// The keypad by its modes and modifiers (finding 6).
		{vt.Key{Code: 57399}, normal, "0"},
		{vt.Key{Code: 57399}, appKeypad, "\x1bOp"},
		{vt.Key{Code: 57414}, normal, "\r"},
		{vt.Key{Code: 57414}, appKeypad, "\x1bOM"},
		{vt.Key{Code: 57413, Mods: 2}, normal, "\x1b+"},
		{vt.Key{Code: 57417}, normal, "\x1b[D"},
		{vt.Key{Code: 57417}, appCursor, "\x1bOD"},
		{vt.Key{Code: 57419, Mods: 4}, normal, "\x1b[1;5A"},
		{vt.Key{Code: 57419, Mods: 2}, appCursor, "\x1b[1;3A"},
		{vt.Key{Code: 57426}, appCursor, "\x1b[3~"},
		{vt.Key{Code: 57426, Mods: 4}, normal, "\x1b[3;5~"},
		{vt.Key{Code: 'a', Event: 3}, normal, ""},
		{vt.Key{Code: 57441, Mods: 1}, normal, ""},
		{vt.Key{Code: 233, Text: "é"}, normal, "é"},
	} {
		if got := string(legacyKey(c.k, c.m)); got != c.want {
			t.Errorf("%+v %+v: %q, want %q", c.k, c.m, got, c.want)
		}
	}
}

// Cursor and function keys the host sends in kitty's form reach a legacy reader in xterm's
// (review finding 1): SS3 for unmodified cursor keys under DECCKM, SS3 P to S for F1 to F4, and
// CSI 1;m R for a modified F3.
func TestLegacyCSIGoldens(t *testing.T) {
	normal, app := legacyModes{}, legacyModes{appCursor: true}
	for _, c := range []struct {
		in   string
		m    legacyModes
		want string
	}{
		{"\x1b[A", normal, "\x1b[A"},
		{"\x1b[A", app, "\x1bOA"},
		{"\x1b[1;5A", app, "\x1b[1;5A"},
		{"\x1b[H", app, "\x1bOH"},
		{"\x1b[F", app, "\x1bOF"},
		{"\x1b[D", normal, "\x1b[D"},
		{"\x1b[P", normal, "\x1bOP"},
		{"\x1b[Q", app, "\x1bOQ"},
		{"\x1b[S", normal, "\x1bOS"},
		{"\x1b[1;2P", normal, "\x1b[1;2P"},
		{"\x1b[13~", normal, "\x1bOR"},
		{"\x1b[13;5~", normal, "\x1b[1;5R"},
		{"\x1b[15~", normal, "\x1b[15~"},
		{"\x1b[5;2~", normal, "\x1b[5;2~"},
	} {
		if got := string(legacyCSI([]byte(c.in), c.m)); got != c.want {
			t.Errorf("%q %+v: %q, want %q", c.in, c.m, got, c.want)
		}
	}
}

// The re-encoder turns the host's CSI u key events into the reader's encoding, gives a text key
// its text and the shifted key, rewrites the other CSI sequences only for a reader that asks,
// passes a bracketed paste as it came, and holds a key event a read cut. Leaving the composed
// screen drops what it held (finding 9).
func TestKittyInTranslate(t *testing.T) {
	rec := keyCodec{key: func(k vt.Key) []byte {
		return []byte(fmt.Sprintf("<%d:%d/%d/%q>", k.Code, k.Shifted, k.Mods, k.Text))
	}}
	var x kittyIn
	got := string(x.translate([]byte("a\x1b[99;5u\x1b[A\x1b[200~\x1b[99;5u\x1b[201~\x1b[97;2"), rec))
	if want := "a<99:0/4/\"\">\x1b[A\x1b[200~\x1b[99;5u\x1b[201~"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := string(x.translate([]byte("u\x1b[46:62;4u"), rec)); got != `<97:0/1/"A"><46:62/3/"">` {
		t.Fatalf("the held event and a shifted key: %q", got)
	}
	// Caps Lock alone upper-cases a letter's text; with Shift it does not.
	if got := string(x.translate([]byte("\x1b[97;65u\x1b[97;66u"), rec)); got != `<97:0/64/"A"><97:0/65/"a">` {
		t.Fatalf("Caps Lock: %q", got)
	}
	if got := string(x.translate([]byte("\x1b[97;65u"), legacyCodec(legacyModes{}))); got != "A" {
		t.Fatalf("Caps Lock to a legacy reader: %q", got)
	}
	all := rec
	all.all = true
	if got := string(x.translate([]byte("aB\r\x1b"), all)); got != `<97:0/0/"a"><98:0/1/"B"><13:0/0/"">`+"\x1b" {
		t.Fatalf("every key as an event: %q", got)
	}
	legacy := legacyCodec(legacyModes{appCursor: true})
	if got := string(x.translate([]byte("\x1b[57399u\x1b[1;5A\x1b[A\x1b[13~\x1b[46:62;4u"), legacy)); got != "0\x1b[1;5A\x1bOA\x1bOR\x1b>" {
		t.Fatalf("legacy: %q", got)
	}
	c := &client{}
	c.kin.translate([]byte("\x1b[200~cut paste"), legacy)
	c.popKitty()
	if c.kin.paste || len(c.kin.held) > 0 {
		t.Fatalf("popKitty kept the re-encoder's state: %+v", c.kin)
	}
}

// gff takes the host's answers to its detection out of the input, whole or cut by a read, until
// the DA1 answer ends the detection; it never searches a bracketed paste, and takes nothing
// after the detection (review findings 4 and 5).
func TestKittyReplies(t *testing.T) {
	now := time.Unix(100, 0)
	c := &client{cfg: Config{Now: func() time.Time { return now }, Logf: t.Logf}, kittyAsked: now, kittyPending: true}
	if got := string(c.takeKittyReplies([]byte("\x1b[200~a\x1b[?5ub\x1b[201~"))); got != "\x1b[200~a\x1b[?5ub\x1b[201~" || c.hostKitty {
		t.Fatalf("a paste lost an answer-shaped string: %q, host kitty %v", got, c.hostKitty)
	}
	if got := string(c.takeKittyReplies([]byte("x\x1b[?"))); got != "x" {
		t.Fatalf("cut answer: %q", got)
	}
	if got := string(c.takeKittyReplies([]byte("1u\x1b[A\x1b[?62;"))); got != "\x1b[A" || !c.hostKitty || !c.kittyPending {
		t.Fatalf("rest of the answer: %q, host kitty %v, pending %v", got, c.hostKitty, c.kittyPending)
	}
	if got := string(c.takeKittyReplies([]byte("22c\x1b[?1u\x1b["))); got != "\x1b[?1u\x1b[" || c.kittyPending {
		t.Fatalf("after DA1: %q, pending %v", got, c.kittyPending)
	}

	// A host that answers only DA1 has no protocol.
	c = &client{cfg: Config{Now: func() time.Time { return now }, Logf: t.Logf}, kittyAsked: now, kittyPending: true}
	if got := string(c.takeKittyReplies([]byte("\x1b[?64;1;9cz"))); got != "z" || c.hostKitty || c.kittyPending {
		t.Fatalf("DA1 alone: %q, host kitty %v, pending %v", got, c.hostKitty, c.kittyPending)
	}

	// Past kittyDetectWait (a slow ssh), whole answers are still taken until DA1 and a late
	// CSI ? u still turns the protocol on; only the hold of a cut read stops, and what was held
	// goes on with the next call, as the loop makes when its hold timer ends.
	c = &client{cfg: Config{Now: func() time.Time { return now }, Logf: t.Logf}, kittyAsked: now, kittyPending: true}
	if got := string(c.takeKittyReplies([]byte("a\x1b"))); got != "a" {
		t.Fatalf("a lone Esc in the window: %q", got)
	}
	now = now.Add(2 * kittyDetectWait)
	if got := string(c.takeKittyReplies(nil)); got != "\x1b" {
		t.Fatalf("the held Esc after the window: %q", got)
	}
	if got := string(c.takeKittyReplies([]byte("\x1b[?1ub\x1b["))); got != "b\x1b[" || !c.hostKitty || !c.kittyPending {
		t.Fatalf("a late answer: %q, host kitty %v, pending %v", got, c.hostKitty, c.kittyPending)
	}
	if got := string(c.takeKittyReplies([]byte("\x1b[?62;22cz"))); got != "z" || c.kittyPending {
		t.Fatalf("a late DA1: %q, pending %v", got, c.kittyPending)
	}
	c = &client{cfg: Config{Now: func() time.Time { return now }, Logf: t.Logf}, kittyAsked: now, kittyPending: true}
	c.takeKittyReplies([]byte("\x1b"))
	c.kittyHoldEnded = true // the loop's timer, with a clock that does not move
	if got := string(c.takeKittyReplies(nil)); got != "\x1b" {
		t.Fatalf("the hold timer did not release the Esc: %q", got)
	}

	// At exit gff drains the answers still to come, and stops waiting after kittyDrainWait.
	c = &client{cfg: Config{Now: time.Now, Logf: t.Logf}, kittyAsked: time.Now(), kittyPending: true}
	in := make(chan []byte, 2)
	in <- []byte("\x1b[?0u")
	in <- []byte("\x1b[?62c")
	c.drainKittyAnswer(in)
	if c.kittyPending || len(in) != 0 {
		t.Fatalf("drain left pending %v, %d reads", c.kittyPending, len(in))
	}
	c.kittyPending = true
	start := time.Now()
	c.drainKittyAnswer(make(chan []byte))
	if d := time.Since(start); d < kittyDrainWait || d > 10*kittyDrainWait {
		t.Fatalf("drain with no answer took %v", d)
	}
}

// Against a real hab: gff asks the host for the kitty protocol. A host that answers only the
// DA1 query gets no push and no pop. A host that answers gets gff's flag pushed on the composed screen; keys it
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
			hst.kitty = answer
			g := startGFF(t, h, hst, SplitColumns, PaneSpec{ID: a}, PaneSpec{ID: b})
			hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "ready") && strings.Contains(s, "right") })
			if !bytes.Contains(hst.output(), []byte(kittyQuery)) {
				t.Fatal("gff did not ask the host for the kitty protocol")
			}
			keys := "x\x03yz"
			if answer {
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
