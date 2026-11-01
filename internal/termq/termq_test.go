package termq

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// scan feeds p in the given chunk sizes and returns the text and the queries.
func scan(p []byte, chunks []int) (text []byte, qs []Segment, offs []uint64) {
	s := NewScanner(100)
	emit := func(seg Segment) {
		if seg.Query != nil {
			qs = append(qs, Segment{Off: seg.Off, Data: append([]byte(nil), seg.Data...), Query: seg.Query})
			return
		}
		offs = append(offs, seg.Off)
		text = append(text, seg.Data...)
	}
	for _, n := range chunks {
		n = min(n, len(p))
		s.Feed(p[:n], emit)
		p = p[n:]
	}
	if len(p) > 0 {
		s.Feed(p, emit)
	}
	return text, qs, offs
}

func TestQueries(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		ans  string
	}{
		{"\x1b[5n", DSR, "\x1b[0n"},
		{"\x1b[6n", CPR, "\x1b[3;5R"},
		{"\x1b[?6n", DECXCPR, "\x1b[?3;5;1R"},
		{"\x1b[c", DA1, "\x1b[?62;22c"},
		{"\x1b[0c", DA1, "\x1b[?62;22c"},
		{"\x1b[>c", DA2, "\x1b[>1;10;0c"},
		{"\x1b[=c", DA3, "\x1bP!|00000000\x1b\\"},
		{"\x1b[>q", XTVERSION, "\x1bP>|flightdeck(test)\x1b\\"},
		{"\x1b[?1049$p", DECRQM, "\x1b[?1049;1$y"},
		{"\x1b[?2026$p", DECRQM, "\x1b[?2026;0$y"},
		{"\x1b[4$p", DECRQM, "\x1b[4;2$y"},
		{"\x1b[18t", WinSize, "\x1b[8;24;80t"},
		{"\x1b[?u", KittyKeys, "\x1b[?1u"},
		{"\x1b]11;?\x07", OSCColor, "\x1b]11;rgb:0000/0000/0000\x07"},
		{"\x1b]10;?;?\x1b\\", OSCColor, "\x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\\x1b]11;rgb:0000/0000/0000\x1b\\"},
		{"\x1b]4;1;?\x07", OSCPalette, "\x1b]4;1;rgb:cdcd/0000/0000\x07"},
		{"\x1bP$qm\x1b\\", DECRQSS, "\x1bP0$r\x1b\\"},
		{"\x1bP+q544e\x1b\\", XTGETTCAP, "\x1bP0+r\x1b\\"},
		{"\x1b]7501;?\x1b\\", ProgStatus, "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\"},
		{"\x1b]7501;?\x07", ProgStatus, "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x07"},
	}
	facts := Facts{
		Col: 4, Row: 2, Cols: 80, Rows: 24, KeyboardFlags: 1, Colors: DefaultColors(), Version: "test",
		Mode: func(n int, ansi bool) (bool, bool) {
			switch {
			case n == 1049 && !ansi:
				return true, true
			case n == 4 && ansi:
				return false, true
			}
			return false, false
		},
	}
	for _, c := range cases {
		in := "ab" + c.in + "cd"
		text, qs, _ := scan([]byte(in), nil)
		if string(text) != "abcd" || len(qs) != 1 {
			t.Errorf("%q: text %q, %d queries", c.in, text, len(qs))
			continue
		}
		q := qs[0]
		if q.Query.Kind != c.kind || string(q.Data) != c.in || q.Off != 102 {
			t.Errorf("%q: got kind %d data %q off %d", c.in, q.Query.Kind, q.Data, q.Off)
		}
		if got := string(Answer(*q.Query, facts)); got != c.ans {
			t.Errorf("%q: answer %q, want %q", c.in, got, c.ans)
		}
	}
}

func TestNotQueries(t *testing.T) {
	for _, in := range []string{
		"\x1b[31mred\x1b[0m",
		"\x1b]0;a title with \x1b[6n inside?\x07", // the ESC ends the title; see below
		"\x1b]2;title\x1b\\",
		"\x1b]11;#000000\x07", // sets, does not ask
		"\x1b]4;1;?;2;red\x07",
		"\x1b]52;c;?\x07",                          // clipboard read: left to the clients
		"\x1b]7501;state=working:app=claude\x1b\\", // a status report, not the query
		"\x1b]7501;?:states=idle\x07",              // a terminal's reply, not the query
		"\x1b]75011;?\x07",
		"\x1b[14t",
		"\x1b[>4;1m",
		"\x1b(B\x1b7\x1b8",
		"\x1bPqsixel\x1b\\",
		"\x1b[6\nn",
		"plain \x18 text",
	} {
		text, qs, _ := scan([]byte(in), nil)
		if in == "\x1b]0;a title with \x1b[6n inside?\x07" {
			// A parser leaves the OSC at the ESC and then reads a CPR: so does the scanner.
			if len(qs) != 1 || qs[0].Query.Kind != CPR {
				t.Errorf("%q: queries %v", in, qs)
			}
			continue
		}
		if string(text) != in || len(qs) != 0 {
			t.Errorf("%q: text %q, %d queries", in, text, len(qs))
		}
	}
}

// TestChunking: the result and every offset are the same however the stream is cut, and a
// scanner restarted at the end of any emitted text segment sees the same rest.
func TestChunking(t *testing.T) {
	seed := rand.Uint64()
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewPCG(seed, 1))
	pieces := []string{"hello ", "\x1b[6n", "\x1b[31m", "\x1b]0;t\x07", "\x1b]11;?\x1b\\", "\r\n",
		"\x1b[?2026$p", "\x1bP+q54\x1b\\", "\x1b", "\x1b[", "é", "\x1b]4;3;?\x07", "\x1b[>c"}
	for round := 0; round < 200; round++ {
		var in []byte
		for i := 0; i < 40; i++ {
			in = append(in, pieces[r.IntN(len(pieces))]...)
		}
		wantText, wantQ, wantOffs := scan(in, nil)
		var chunks []int
		for n := 0; n < len(in); {
			k := 1 + r.IntN(7)
			chunks = append(chunks, k)
			n += k
		}
		text, qs, _ := scan(in, chunks)
		if !bytes.Equal(text, wantText) || len(qs) != len(wantQ) {
			t.Fatalf("round %d: chunked scan differs: %q vs %q", round, text, wantText)
		}
		for i := range qs {
			if qs[i].Off != wantQ[i].Off || !bytes.Equal(qs[i].Data, wantQ[i].Data) {
				t.Fatalf("round %d: query %d differs", round, i)
			}
		}
		// Restart at the start of a text segment, as a resumed stream does.
		if len(wantOffs) > 1 {
			at := wantOffs[r.IntN(len(wantOffs))]
			rest, rq, _ := scanFrom(in, at)
			var tail []byte
			s := NewScanner(100)
			var nq int
			s.Feed(in, func(seg Segment) {
				if seg.Off < at {
					return
				}
				if seg.Query != nil {
					nq++
					return
				}
				tail = append(tail, seg.Data...)
			})
			if !bytes.Equal(rest, tail) || len(rq) != nq {
				t.Fatalf("round %d: restart at %d: %q vs %q", round, at, rest, tail)
			}
		}
	}
}

func scanFrom(in []byte, at uint64) ([]byte, []Segment, []uint64) {
	s := NewScanner(at)
	var text []byte
	var qs []Segment
	s.Feed(in[at-100:], func(seg Segment) {
		if seg.Query != nil {
			qs = append(qs, seg)
			return
		}
		text = append(text, seg.Data...)
	})
	return text, qs, nil
}

func TestHeld(t *testing.T) {
	s := NewScanner(0)
	var got []byte
	s.Feed([]byte("ab\x1b[6"), func(seg Segment) { got = append(got, seg.Data...) })
	if string(got) != "ab" || s.Held() != 3 {
		t.Fatalf("got %q held %d", got, s.Held())
	}
	var q *Query
	s.Feed([]byte("nc"), func(seg Segment) {
		if seg.Query != nil {
			q = seg.Query
			if seg.Off != 2 {
				t.Errorf("query off %d", seg.Off)
			}
			return
		}
		got = append(got, seg.Data...)
	})
	if q == nil || q.Kind != CPR || string(got) != "abc" {
		t.Fatalf("q %v got %q", q, got)
	}
}
