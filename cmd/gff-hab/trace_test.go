package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/trace"
)

// failedLine matches a failed-request line and its trace (diagnostics.md 4.6, 5.2).
var failedLine = regexp.MustCompile(`request failed: method (\S+), error ([^,]+), client (\S+), [^,]+, trace ([agh][0-9A-Z]{11})`)

// failedLines returns the failed-request lines of method in a hab's hab.log.
func failedLines(t *testing.T, state, method string) [][]string {
	t.Helper()
	var out [][]string
	for _, m := range failedLine.FindAllStringSubmatch(habLog(t, state), -1) {
		if m[1] == method {
			out = append(out, m)
		}
	}
	return out
}

// TestTracePeer is diagnostics test 9 end to end and the stage 4 acceptance (5.2, 5.4): a
// failing call from gff's client to a session on a peer hab yields one failed-request line on
// each hab, both with gff's trace; the answer carries it too; gff-hab logs --trace finds the
// line. A malformed trace is replaced by the hab's own.
func TestTracePeer(t *testing.T) {
	t.Parallel()
	p := newMeshPair(t)
	if w := welcome(t, p.a.sock); !strings.Contains(strings.Join(w.Capabilities, " "), control.TraceCapability) {
		t.Fatalf("capabilities %v lack %s", w.Capabilities, control.TraceCapability)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	// gff's own client stack: habconn with gff's trace origin (cmd/gff).
	conn, err := habconn.Connect(ctx, habconn.Config{Transport: habconn.UnixTransport{Path: p.a.sock},
		Client: api.ClientInfo{Name: "gff", Version: "test", Kind: api.ClientKindTerminal}, TraceOrigin: trace.Gff, NoSubscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var r api.PaneText
	err = conn.Call(ctx, "pane.read", api.PaneReadParams{Session: "bee/nosuch"}, &r)
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Fatalf("pane.read of a missing session on the peer: %v, want not_found", err)
	}

	var la, lb [][]string
	waitUntil(t, "a failed-request line on each hab", func() bool {
		la, lb = failedLines(t, p.a.state, "pane.read"), failedLines(t, p.b.state, "pane.read")
		return len(la) > 0 && len(lb) > 0
	})
	if len(la) != 1 || len(lb) != 1 {
		t.Fatalf("want one pane.read line on each hab, got a %q, b %q", la, lb)
	}
	id := la[0][4]
	if id[0] != trace.Gff || lb[0][4] != id {
		t.Fatalf("traces: a %s, b %s; want one g trace on both", id, lb[0][4])
	}
	if la[0][3] != "terminal" || lb[0][3] != "hab" {
		t.Errorf("client kinds: a %s, b %s; want terminal on a, hab (the peer link) on b", la[0][3], lb[0][3])
	}
	if out, code := runCode(t, nil, "logs", "-state", p.a.state, "-trace", strings.ToLower(id)); code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, "trace "+id) {
		t.Errorf("logs -trace on a: exit %d, %q; want the one line", code, out)
	}
	if out, code := runCode(t, nil, "logs", "-state", p.b.state, "-trace", trace.New(trace.Gff)); code != 0 || out != "" {
		t.Errorf("logs -trace of another trace on b: exit %d, %q; want nothing", code, out)
	}

	// pane.type forwards on its own path (hab-logic-api.md 3.12); it carries the trace too.
	var typed api.PaneTyped
	err = conn.Call(ctx, "pane.type", api.PaneTypeParams{Session: "bee/nosuch", Text: "x"}, &typed)
	if !errors.As(err, &ce) {
		t.Fatalf("pane.type of a missing session on the peer: %v, want an error", err)
	}
	var ta, tb [][]string
	waitUntil(t, "a pane.type line on each hab", func() bool {
		ta, tb = failedLines(t, p.a.state, "pane.type"), failedLines(t, p.b.state, "pane.type")
		return len(ta) > 0 && len(tb) > 0
	})
	if len(ta) != 1 || len(tb) != 1 || ta[0][4][0] != trace.Gff || tb[0][4] != ta[0][4] || ta[0][4] == id {
		t.Fatalf("pane.type lines: a %q, b %q; want one each with one new g trace", ta, tb)
	}

	// A malformed trace is replaced, not refused: the peer's line carries the hab's own trace.
	// Client.Call sends the context's trace as it is.
	err = p.a.control().Call(trace.With(ctx, "not a trace!"), "pane.read", api.PaneReadParams{Session: "bee/nosuch"}, &r)
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Fatalf("pane.read with a malformed trace: %v, want not_found", err)
	}
	waitUntil(t, "a second line on each hab", func() bool {
		la, lb = failedLines(t, p.a.state, "pane.read"), failedLines(t, p.b.state, "pane.read")
		return len(la) > 1 && len(lb) > 1
	})
	got := la[1][4]
	if got[0] != trace.Hab || lb[1][4] != got {
		t.Fatalf("traces: a %s, b %s; want a's new h trace on both", got, lb[1][4])
	}
	if !strings.Contains(habLog(t, p.a.state), "trace "+got+", trace_replaced") {
		t.Errorf("a's line does not note trace_replaced for %s", got)
	}
	if strings.Contains(habLog(t, p.a.state)+habLog(t, p.b.state), "not a trace!") {
		t.Error("a log holds the malformed trace")
	}
}
