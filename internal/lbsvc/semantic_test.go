package lbsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// TestSemanticFind: a semantic find ranks by this hab's index, else by a peer's, else answers
// the stated message; the ranking passes this replica's filters and private rule; a call from a
// peer is never forwarded.
func TestSemanticFind(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	w, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{
		create("task", "alpha"), create("finding", "beta"), create("task", "gamma"),
		{Op: api.LogbookChangeOpCreate, Type: "project", Title: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	a, b, g, proj := w.IDs[0], w.IDs[1], w.IDs[2], w.IDs[3]
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{
		{Op: api.LogbookChangeOpSet, ID: g, Field: "project", Value: api.RawJSON(`"` + proj + `"`)}}}); err != nil {
		t.Fatal(err)
	}

	// No index and no peer.
	_, err = r.svc.Find(ctx, api.LogbookFindParams{Words: "anything", Semantic: true})
	if kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), NoVectors) {
		t.Fatalf("no vector hab: %v", err)
	}
	if _, err := r.svc.Find(ctx, api.LogbookFindParams{Semantic: true}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("no words: %v", err)
	}

	// A peer that has none.
	r.svc.cfg.Forward = func(context.Context, api.LogbookFindParams) (api.LogbookRecords, bool, error) {
		return api.LogbookRecords{}, false, nil
	}
	if _, err := r.svc.Find(ctx, api.LogbookFindParams{Words: "x", Semantic: true}); !strings.Contains(err.Error(), NoVectors) {
		t.Fatalf("no peer with vectors: %v", err)
	}

	// A peer's ranking, filtered here: the private project's record is hidden from a pane outside it.
	forwarded := 0
	r.svc.cfg.Forward = func(_ context.Context, p api.LogbookFindParams) (api.LogbookRecords, bool, error) {
		forwarded++
		return api.LogbookRecords{Records: []api.LogbookRecord{{ID: g}, {ID: "task-notherex"}, {ID: b}, {ID: a}}}, true, nil
	}
	res, err := r.svc.find(ctx, api.LogbookFindParams{Words: "x", Semantic: true}, []string{proj})
	if err != nil || ids(res) != b+","+a {
		t.Fatalf("forwarded: %v %v", ids(res), err)
	}
	// A call from a peer is not forwarded again.
	if _, err := r.svc.Find(control.WithPeerCall(ctx), api.LogbookFindParams{Words: "x", Semantic: true}); !strings.Contains(err.Error(), NoVectors) || forwarded != 1 {
		t.Fatalf("peer call forwarded: %v (%d)", err, forwarded)
	}

	// A local index wins, and filters apply to its order.
	r.svc.cfg.Vectors = func(_ context.Context, words string, k int) ([]string, error) {
		return []string{g, b, a}, nil
	}
	res, err = r.svc.Find(ctx, api.LogbookFindParams{Words: "x", Semantic: true, Type: "task"})
	if err != nil || ids(res) != g+","+a || forwarded != 1 {
		t.Fatalf("local: %v %v", ids(res), err)
	}
	res, err = r.svc.Find(control.WithPeerCall(ctx), api.LogbookFindParams{Words: "x", Semantic: true, Limit: 1})
	if err != nil || ids(res) != g || !res.More {
		t.Fatalf("local from a peer, limit 1: %v %v %v", ids(res), res.More, err)
	}
}

func ids(r api.LogbookRecords) string {
	var out []string
	for _, x := range r.Records {
		out = append(out, x.ID)
	}
	return strings.Join(out, ",")
}
