package logbook

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// Stage 4: idempotency keys and Search.

func TestKeyAppliesOnce(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	b := Batch{Steps: []Step{Create(TypeTask, map[string]any{"title": "once"})}, Key: "k1", KeyHash: "h1"}
	first, err := a.Write(ctx, b)
	if err != nil || first.Repeated {
		t.Fatalf("first write: %+v %v", first, err)
	}
	// The same key again, with a freshly minted id in the step: nothing applies.
	b.Steps = []Step{Create(TypeTask, map[string]any{"title": "once"})}
	again, err := a.Write(ctx, b)
	if err != nil || !again.Repeated || !slices.Equal(again.Records, first.Records) || !slices.Equal(again.Ops, first.Ops) {
		t.Fatalf("repeat: %+v %v, first %+v", again, err, first)
	}
	if st, _ := a.Status(ctx); st.Records != 1 || st.Ops != 1 {
		t.Fatalf("status after the repeat: %+v", st)
	}
	// Another request under the same key is refused.
	if _, err := a.Write(ctx, Batch{Steps: b.Steps, Key: "k1", KeyHash: "h2"}); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("reused key: %v", err)
	}
	// A refused write keeps no key: the retry runs.
	bad := Batch{Steps: []Step{Create(TypeTask, map[string]any{})}, Key: "k2", KeyHash: "h"}
	if _, err := a.Write(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad write: %v", err)
	}
	bad.Steps = []Step{Create(TypeTask, map[string]any{"title": "fixed"})}
	if r, err := a.Write(ctx, bad); err != nil || r.Repeated {
		t.Fatalf("retry after a refusal: %+v %v", r, err)
	}
	// A second Store on the file (the CLI) sees the key.
	other := openReplica(t, "A", a.path)
	b.Steps = []Step{Create(TypeTask, map[string]any{"title": "once"})}
	if r, err := other.Write(ctx, b); err != nil || !r.Repeated {
		t.Fatalf("repeat through another writer: %+v %v", r, err)
	}
	// Keys go after KeyKeep.
	a.clock.Advance(KeyKeep*time.Millisecond + time.Hour)
	if _, err := a.Write(ctx, Batch{Steps: []Step{Create(TypeTask, map[string]any{"title": "later"})}, Key: "k3", KeyHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if r, err := a.Write(ctx, b); err != nil || r.Repeated {
		t.Fatalf("an expired key still held: %+v %v", r, err)
	}
	if _, err := a.Write(ctx, Batch{Steps: b.Steps, Key: string(make([]byte, MaxKey+1))}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long key: %v", err)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "A")
	agent := &Actor{Kind: "agent", Harness: "claude", Session: "s-1", Host: "box-a"}
	blocker := a.write(t, agent, Create(TypeTask, map[string]any{"title": "blocker task", "project": "proj-aaaaaaaa"})).Records[0]
	a.clock.Advance(time.Minute)
	blocked := a.write(t, nil, Create(TypeTask, map[string]any{"title": "blocked task", "project": "proj-aaaaaaaa"})).Records[0]
	a.clock.Advance(30 * time.Second)
	a.write(t, agent, Step{Kind: KindAdd, Record: blocker, Field: "links", Value: Link{Kind: LinkBlocks, To: blocked}})
	a.clock.Advance(time.Minute)
	finding := a.write(t, &Actor{Kind: "person", Device: "dev-1"}, Step{Kind: KindCreate, Record: NewID(TypeFinding), Type: TypeFinding,
		Fields: map[string]any{"title": "a general finding about zebras"}, Source: &Source{Session: "s-2", Quote: "note that"}}).Records[0]
	a.write(t, nil, Step{Kind: KindAdd, Record: finding, Field: "tags", Value: "zoo"})

	ids := func(q Query) []string {
		t.Helper()
		items, _, err := a.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	check := func(name string, q Query, want ...string) {
		t.Helper()
		if got := ids(q); !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	// blocker was changed after blocked (its link), so it is newer.
	check("all, newest change first", Query{}, finding, blocker, blocked)
	check("type", Query{Type: TypeTask}, blocker, blocked)
	check("project", Query{Project: "proj-aaaaaaaa"}, blocker, blocked)
	check("general", Query{General: true}, finding)
	check("tag", Query{Tag: "zoo"}, finding)
	check("author session", Query{Author: "s-1"}, blocker)
	check("author device", Query{Author: "dev-1"}, finding)
	check("author harness", Query{Author: "claude"}, blocker)
	check("session from source", Query{Session: "s-2"}, finding)
	check("host", Query{Host: "box-a"}, blocker)
	check("host by origin", Query{Host: "SHA256:A"}, finding, blocker, blocked)
	check("words", Query{Words: "zebras"}, finding)
	tasks := ids(Query{Words: "task", Status: "open"})
	slices.Sort(tasks)
	want := []string{blocker, blocked}
	slices.Sort(want)
	if !slices.Equal(tasks, want) {
		t.Errorf("words with a filter: got %v, want %v", tasks, want)
	}
	check("words that are syntax", Query{Words: `"NEAR(`})
	check("since", Query{SinceMS: base.Add(90 * time.Second).UnixMilli()}, finding, blocker)
	check("ready: the blocked task waits", Query{Ready: true}, blocker)
	a.write(t, nil, Step{Kind: KindSet, Record: blocker, Field: "status", Value: "done"})
	check("ready once the blocker is done", Query{Ready: true}, blocked)
	if items, more, _ := a.Search(ctx, Query{Limit: 2}); len(items) != 2 || !more {
		t.Errorf("limit: %d items, more %v", len(items), more)
	}
	a.write(t, nil, Step{Kind: KindSet, Record: finding, Field: "deleted", Value: true})
	check("deleted left out", Query{Type: TypeFinding})
	check("deleted asked for", Query{Type: TypeFinding, Deleted: true}, finding)
}
