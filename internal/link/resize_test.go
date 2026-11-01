package link

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// pongHold sits between the link and a real holder and passes every frame through, except
// around a resize. The holder answers a resize only when it refuses it; the link learns the
// resize is done from the pong to the ping it sends right after (holderclient's do). pongHold
// holds that pong until release, while later output passes. That is the order in which the
// holder answers after the program's redraw, which a busy machine produces at random; here it
// happens every time. With refuse set, pongHold keeps each resize from the holder and answers
// it with an exited error itself, as the holder does for a pane whose terminal is closed.
type pongHold struct {
	refuse  bool
	mu      sync.Mutex
	resized bool            // a resize went out; the next ping is its fence
	fences  map[uint32]bool // tokens of the pings that follow a resize
	held    chan struct{}   // closed when a fence's pong is held
	release chan struct{}
	once    sync.Once
}

// startPongHold serves instance id's holder, registered in hdir, under pdir: a registry file
// there names the proxy's socket. A link opened on pdir talks to the holder through it.
func startPongHold(t *testing.T, hdir, pdir, id string, refuse bool) *pongHold {
	t.Helper()
	e, err := holderreg.Read(holderreg.Path(hdir, id))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", holderreg.SocketPath(pdir, id))
	if err != nil {
		t.Fatal(err)
	}
	ph := &pongHold{refuse: refuse, fences: map[uint32]bool{}, held: make(chan struct{}), release: make(chan struct{})}
	if refuse {
		ph.Release()
	}
	var conns sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		ph.Release()
		conns.Wait()
	})
	go func() {
		for {
			up, err := ln.Accept()
			if err != nil {
				return
			}
			down, err := net.Dial("unix", e.Socket)
			if err != nil {
				up.Close()
				continue
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				ph.serve(up, down)
			}()
		}
	}()
	pe := e
	pe.Socket = holderreg.SocketPath(pdir, id)
	if err := holderreg.Write(pdir, pe); err != nil {
		t.Fatal(err)
	}
	return ph
}

// Release lets the held pongs go to the link.
func (ph *pongHold) Release() { ph.once.Do(func() { close(ph.release) }) }

// readRaw reads one frame as it is on the wire.
func readRaw(r io.Reader) (k hp.Kind, pane hp.Handle, tok uint32, b []byte, err error) {
	b = make([]byte, hp.HeaderSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, 0, 0, nil, err
	}
	n := binary.BigEndian.Uint32(b[10:14])
	if n > hp.MaxPayload {
		return 0, 0, 0, nil, hp.ErrTooLarge
	}
	b = append(b, make([]byte, n)...)
	if _, err := io.ReadFull(r, b[hp.HeaderSize:]); err != nil {
		return 0, 0, 0, nil, err
	}
	k = hp.Kind(binary.BigEndian.Uint16(b[0:2]))
	pane = hp.Handle(binary.BigEndian.Uint32(b[2:6]))
	return k, pane, binary.BigEndian.Uint32(b[6:10]), b, nil
}

func (ph *pongHold) serve(link, holder net.Conn) {
	defer link.Close()
	defer holder.Close()
	var wmu sync.Mutex // writes to the link: frames passing, refusals and held pongs let go
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := link.Write(b)
		return err
	}
	go func() { // link to holder: find each resize and the ping after it
		defer holder.Close()
		for {
			k, pane, tok, b, err := readRaw(link)
			if err != nil {
				return
			}
			ph.mu.Lock()
			switch {
			case k == hp.KindResize:
				ph.resized = true
			case k == hp.KindPing && ph.resized:
				ph.resized = false
				ph.fences[tok] = true
			}
			ph.mu.Unlock()
			if k == hp.KindResize && ph.refuse {
				e, err := hp.AppendFrame(nil, &hp.Error{Handle: pane, Token: tok, Ref: hp.KindResize,
					Code: hp.CodeExited, Message: "pane terminal is closed"}, hp.Version1)
				if err != nil || write(e) != nil {
					return
				}
				continue
			}
			if _, err := holder.Write(b); err != nil {
				return
			}
		}
	}()
	var held sync.WaitGroup
	defer held.Wait()
	for {
		k, _, tok, b, err := readRaw(holder)
		if err != nil {
			return
		}
		ph.mu.Lock()
		hold := k == hp.KindPong && ph.fences[tok]
		delete(ph.fences, tok)
		ph.mu.Unlock()
		if !hold {
			if write(b) != nil {
				return
			}
			continue
		}
		select {
		case <-ph.held:
		default:
			close(ph.held)
		}
		held.Add(1)
		go func() {
			defer held.Done()
			<-ph.release
			write(b)
		}()
	}
}

// redrawScript draws, at start and on every SIGWINCH, as a full-screen program redraws: the
// number of the draw in the top left cell, and an R in the bottom right cell of whatever size
// the terminal has.
const redrawScript = `n=0
draw() { n=$((n+1)); set -- $(stty size); printf '\033[2J\033[Hdraw %d\033[%d;%dHR' "$n" "$1" "$2"; }
trap draw WINCH
draw
while :; do sleep 0.02; done`

// cornerR reports whether the last screen row ends in R at column cols.
func cornerR(sc Screen, cols int) bool {
	lines := strings.Split(strings.TrimRight(sc.Text, "\n"), "\n")
	return lines[len(lines)-1] == strings.Repeat(" ", cols-1)+"R"
}

// TestResizeRedrawParsedAtNewSize: a program that redraws at once on SIGWINCH gets its redraw
// parsed at the new size, even when the holder's answer to the resize arrives after the redraw.
// The parser takes the size when the link sends the resize, not when the holder answers.
func TestResizeRedrawParsedAtNewSize(t *testing.T) {
	hdir, pdir := shortDir(t), shortDir(t)
	startHolder(t, hdir, "one", 0)
	ph := startPongHold(t, hdir, pdir, "one", false)
	l := openLink(t, Config{HolderDir: pdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(90)
	spawn(t, l, u, redrawScript)
	sc := func() Screen {
		s, err := l.Screen(u)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitFor(t, "the first draw", func() bool { return cornerR(sc(), 80) })

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Resize(ctx, u, 100, 30) }()
	select {
	case <-ph.held:
	case <-time.After(waitLong):
		t.Fatal("the holder never answered the resize")
	}
	// The answer waits; the redraw reaches the parser before it.
	waitFor(t, "the redraw", func() bool { return strings.Contains(sc().Text, "draw 2") })
	ph.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := sc()
	if s.Cols != 100 || s.Rows != 30 || !cornerR(s, 100) {
		t.Fatalf("screen %dx%d after the resize, want the R in the bottom right of 100x30:\n%s", s.Cols, s.Rows, s.Text)
	}
}

// TestResizeRefusedPutsParserBack: when the holder refuses a resize, the parser goes back to
// the size it had, and every subscriber's last resize frame names that size.
func TestResizeRefusedPutsParserBack(t *testing.T) {
	hdir, pdir := shortDir(t), shortDir(t)
	startHolder(t, hdir, "one", 0)
	startPongHold(t, hdir, pdir, "one", true)
	l := openLink(t, Config{HolderDir: pdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(91)
	spawn(t, l, u, "printf ready; exec sleep 30")
	waitFor(t, "output", func() bool { return screenHas(t, l, u, "ready") })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	sub, err := l.Subscribe(ctx, u, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	err = l.Resize(ctx, u, 100, 30)
	var re *holderclient.RequestError
	if !errors.As(err, &re) || re.Code != hp.CodeExited {
		t.Fatalf("resize: %v, want the holder's refusal", err)
	}
	if st, _ := paneStatus(l, u); st.Cols != 80 || st.Rows != 24 {
		t.Fatalf("pane is %dx%d after a refused resize, want 80x24", st.Cols, st.Rows)
	}
	// Every resize frame is queued by now; the last one names the size the pane kept.
	var last *Chunk
	for {
		select {
		case ch, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription ended: %v", sub.Err())
			}
			if ch.Kind == ChunkResize {
				last = &ch
			}
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if last != nil && (last.Cols != 80 || last.Rows != 24) {
		t.Fatalf("last resize frame %dx%d, want 80x24", last.Cols, last.Rows)
	}
}
