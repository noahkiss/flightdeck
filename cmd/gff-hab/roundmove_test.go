package main

// round move against fake habs (rounds.md 9, Built (stage 6)): the pause before the create, the
// rollback when the peer refuses, a lost answer settled by asking the peer, a context of its
// own after the question, a completed round refused, a name the peer already holds. Then the
// -host refusals that come before any connection.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// moveCall is one call a moveFake took: its method and whether its context was live.
type moveCall struct {
	method string
	live   bool
}

// moveFake answers calls from handlers by method, and records each call.
type moveFake struct {
	mu       sync.Mutex
	calls    []moveCall
	handlers map[string]func(ctx context.Context, params any) (any, error)
}

func (f *moveFake) Call(ctx context.Context, method string, params, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, moveCall{method, ctx.Err() == nil})
	h := f.handlers[method]
	f.mu.Unlock()
	if h == nil {
		return control.Errorf(api.ErrorKindInvalid, "fake: no %s", method)
	}
	v, err := h(ctx, params)
	if err != nil || result == nil || v == nil {
		return err
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

func (f *moveFake) methods() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []string
	for _, c := range f.calls {
		m = append(m, c.method)
	}
	return strings.Join(m, ",")
}

// deadCalls lists the calls made with a context that had already ended.
func (f *moveFake) deadCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []string
	for _, c := range f.calls {
		if !c.live {
			m = append(m, c.method)
		}
	}
	return m
}

func answer(v any) func(context.Context, any) (any, error) {
	return func(context.Context, any) (any, error) { return v, nil }
}

func fail(err error) func(context.Context, any) (any, error) {
	return func(context.Context, any) (any, error) { return nil, err }
}

var errDropped = errors.New("connection closed") // not a control.Error: the outcome is unknown

func movable() api.Round {
	return api.Round{ID: "r1", Revision: 3, NextSlot: "2026-11-03T09:00:00Z", RoundSpec: api.RoundSpec{
		Name: "nightly", Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "0 9 * * *", Zone: "UTC"},
		Command: []string{"true"}, Project: "p1", ProjectRemote: "example.com/team/app"}}
}

// moveHabs is a source with round r and a peer whose create and get do what is given.
func moveHabs(r api.Round, create, get func(context.Context, any) (any, error)) (from, to *moveFake) {
	from = &moveFake{handlers: map[string]func(context.Context, any) (any, error){
		"round.get":     answer(api.RoundResult{Round: r}),
		"round.pause":   answer(api.RoundResult{}),
		"round.unpause": answer(api.RoundResult{}),
		"round.delete":  answer(nil),
	}}
	to = &moveFake{handlers: map[string]func(context.Context, any) (any, error){"round.create": create, "round.get": get}}
	return from, to
}

func moved(r api.Round) api.Round {
	m := r
	m.ID, m.Project = "r9", "p9"
	return m
}

func runMove(t *testing.T, from, to *moveFake) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := moveRound(context.Background(), from, to, "nightly", "bee", true, false, strings.NewReader(""), &out)
	return out.String(), err
}

func TestRoundMove(t *testing.T) {
	notFound := fail(control.Errorf(api.ErrorKindNotFound, "no round nightly on this hab"))

	t.Run("moves after the pause", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, answer(api.RoundCreated{Round: moved(r)}), notFound)
		if out, err := runMove(t, from, to); err != nil || !strings.Contains(out, "moved nightly to bee") {
			t.Fatalf("%v %s", err, out)
		}
		if m := from.methods(); m != "round.get,round.pause,round.delete" {
			t.Errorf("source calls %s: the pause comes before the create", m)
		}
		if m := to.methods(); m != "round.create" {
			t.Errorf("peer calls %s", m)
		}
	})

	t.Run("a refused create unpauses", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, fail(control.Errorf(api.ErrorKindNotFound, "no project on this hab has the remote x")), notFound)
		out, err := runMove(t, from, to)
		if err == nil || !strings.Contains(err.Error(), "it stays here") {
			t.Fatalf("%v %s", err, out)
		}
		if m := from.methods(); m != "round.get,round.pause,round.unpause" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("a round paused before keeps its pause", func(t *testing.T) {
		r := movable()
		r.Paused = true
		from, to := moveHabs(r, fail(control.Errorf(api.ErrorKindInvalid, "no")), notFound)
		if _, err := runMove(t, from, to); err == nil {
			t.Fatal("moved")
		}
		if m := from.methods(); m != "round.get" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("a lost answer, the round there: the move finishes", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, fail(errDropped), answer(api.RoundResult{Round: moved(r)}))
		if out, err := runMove(t, from, to); err != nil || !strings.Contains(out, "moved nightly to bee") {
			t.Fatalf("%v %s", err, out)
		}
		if m := from.methods(); m != "round.get,round.pause,round.delete" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("a lost answer, no round there: unpaused", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, fail(errDropped), notFound)
		if _, err := runMove(t, from, to); err == nil || !strings.Contains(err.Error(), "it stays here") {
			t.Fatalf("%v", err)
		}
		if m := from.methods(); m != "round.get,round.pause,round.unpause" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("a lost answer, the peer silent: stays paused", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, fail(errDropped), fail(errDropped))
		_, err := runMove(t, from, to)
		if err == nil {
			t.Fatal("moved")
		}
		for _, w := range []string{"stays paused here", "gff-hab round show nightly -host bee", "gff-hab round rm nightly", "gff-hab round unpause nightly"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the error lacks %q: %v", w, err)
			}
		}
		if m := from.methods(); m != "round.get,round.pause" {
			t.Errorf("source calls %s: neither unpaused nor deleted", m)
		}
	})

	t.Run("a lost answer, another round by that name: unpaused", func(t *testing.T) {
		r := movable()
		other := moved(r)
		other.Command = []string{"make", "other"}
		from, to := moveHabs(r, fail(errDropped), answer(api.RoundResult{Round: other}))
		if _, err := runMove(t, from, to); err == nil {
			t.Fatal("moved")
		}
		if m := from.methods(); m != "round.get,round.pause" {
			t.Errorf("source calls %s: an unknown create keeps the pause", m)
		}
	})

	t.Run("the name taken on the peer", func(t *testing.T) {
		r := movable()
		r.Paused = true // an earlier move whose delete failed left it paused here
		from, to := moveHabs(r, fail(control.Errorf(api.ErrorKindConflict, "a round named nightly exists")), answer(api.RoundResult{Round: moved(r)}))
		_, err := runMove(t, from, to)
		if err == nil || strings.Contains(err.Error(), "it stays here") ||
			!strings.Contains(err.Error(), "a round nightly already exists on bee") || !strings.Contains(err.Error(), "gff-hab round rm nightly") {
			t.Fatalf("%v", err)
		}
		if m := from.methods(); m != "round.get" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("the name taken on the peer, an active round: unpaused", func(t *testing.T) {
		r := movable()
		from, to := moveHabs(r, fail(control.Errorf(api.ErrorKindConflict, "a round named nightly exists")), answer(api.RoundResult{Round: moved(r)}))
		if _, err := runMove(t, from, to); err == nil || !strings.Contains(err.Error(), "already exists on bee") {
			t.Fatalf("%v", err)
		}
		if m := from.methods(); m != "round.get,round.pause,round.unpause" {
			t.Errorf("source calls %s", m)
		}
	})

	t.Run("a completed round stays", func(t *testing.T) {
		r := movable()
		r.Completed, r.CompletedReason, r.MaxRuns = true, "max_runs", 3
		from, to := moveHabs(r, answer(api.RoundCreated{Round: moved(r)}), notFound)
		_, err := runMove(t, from, to)
		if err == nil || !strings.Contains(err.Error(), "completed") || !strings.Contains(err.Error(), "round edit") {
			t.Fatalf("%v", err)
		}
		if m := from.methods() + "|" + to.methods(); m != "round.get|" {
			t.Errorf("calls %s", m)
		}
	})

	t.Run("its own contexts after the question", func(t *testing.T) {
		defer func(c, s time.Duration) { moveCallTimeout, moveSettleTimeout = c, s }(moveCallTimeout, moveSettleTimeout)
		moveCallTimeout = 100 * time.Millisecond
		r := movable()
		// The create outlives the move's context; the rollback still runs.
		from, to := moveHabs(r, func(ctx context.Context, _ any) (any, error) { <-ctx.Done(); return nil, ctx.Err() }, notFound)
		ctx, cancel := context.WithCancel(context.Background())
		// The user answers after the context the CLI started with ended.
		in := readerFunc(func(p []byte) (int, error) { cancel(); return copy(p, "y\n"), io.EOF })
		var out bytes.Buffer
		err := moveRound(ctx, from, to, "nightly", "bee", false, false, in, &out)
		if err == nil || !strings.Contains(err.Error(), "it stays here") {
			t.Fatalf("%v %s", err, out.String())
		}
		if m := from.methods(); m != "round.get,round.pause,round.unpause" {
			t.Errorf("source calls %s", m)
		}
		if d := append(from.deadCalls(), to.deadCalls()...); len(d) > 0 {
			t.Errorf("calls made with an ended context: %v", d)
		}
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestRoundHostRefusals: -host from a pane and -host with -via are refused before any
// connection, with their own words.
func TestRoundHostRefusals(t *testing.T) {
	t.Setenv("GFF_HAB_SOCKET", "")
	t.Setenv(hook.Env, "")
	if err := roundCmd([]string{"ls", "-host", "bee", "-via", "false"}, strings.NewReader(""), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not with -via") {
		t.Errorf("-host with -via: %v", err)
	}
	t.Setenv(hook.Env, "http+unix:///nonexistent/hook.sock")
	for _, verb := range [][]string{{"ls", "-host", "bee"}, {"pause", "nightly", "-host", "bee"}, {"move", "nightly", "-host", "bee", "-yes"}} {
		if err := roundCmd(verb, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "not available from a pane") {
			t.Errorf("%v from a pane: %v", verb, err)
		}
	}
}
