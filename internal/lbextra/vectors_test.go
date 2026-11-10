package lbextra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// testEngine wraps FakeEngine: it can be down, slow, or refuse any text that holds a word.
type testEngine struct {
	mu     sync.Mutex
	down   bool
	block  chan struct{} // when set, Embed of more than one text waits for it to close
	all    bool          // block every Embed, a query's too
	refuse string
	calls  [][]string
}

func (e *testEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	down, block, refuse, all := e.down, e.block, e.refuse, e.all
	e.calls = append(e.calls, slices.Clone(texts))
	e.mu.Unlock()
	if block != nil && (all || len(texts) > 1) {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if down {
		return nil, errors.New("embeddings server: could not be reached")
	}
	for _, t := range texts {
		if refuse != "" && strings.Contains(t, refuse) {
			return nil, &StatusError{Code: 400}
		}
	}
	return FakeEngine{}.Embed(ctx, texts)
}

func (e *testEngine) set(f func(e *testEngine)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(e)
}

// A forget drops the record's old vector at once, from memory and the file, even when the
// engine is down: the forgotten words no longer find it (logbook.md 5.7, 13.2).
func TestVectorsForgetEngineDown(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	res := write(t, st, create("task-dddd4444", logbook.TypeTask, map[string]any{"title": "rotate", "body": "zebracorn hunter2"}))
	path := filepath.Join(t.TempDir(), "v.gob")
	eng := &testEngine{}
	v := &Vectors{Engine: eng, Path: path}
	if n, err := v.Sync(ctx, st); err != nil || n != 1 {
		t.Fatalf("sync %d %v", n, err)
	}
	if ids, _ := v.Search(ctx, "zebracorn hunter2", 3); !slices.Equal(ids, []string{"task-dddd4444"}) {
		t.Fatalf("before forget %v", ids)
	}
	write(t, st, logbook.Step{Kind: logbook.KindForget, Record: "task-dddd4444", Target: res.Ops[0]})
	eng.set(func(e *testEngine) { e.down = true })
	if _, err := v.Sync(ctx, st); err == nil {
		t.Fatal("sync with the engine down: no error")
	}
	// Search embeds the query through a FakeEngine of its own: the index alone decides.
	v.Engine = FakeEngine{}
	if ids, err := v.Search(ctx, "zebracorn hunter2", 3); err != nil || len(ids) != 0 {
		t.Fatalf("forgotten words still find %v (%v)", ids, err)
	}
	// The file too: a restart loads no vector of the forgotten text.
	v2 := &Vectors{Engine: FakeEngine{}, Path: path}
	v2.mu.Lock()
	v2.load()
	_, held := v2.recs["task-dddd4444"]
	v2.mu.Unlock()
	if held {
		t.Fatal("the index file still holds the forgotten record's vector")
	}
	// With the engine back, the record is embedded again from what is left.
	v.Engine = eng
	eng.set(func(e *testEngine) { e.down = false })
	if n, err := v.Sync(ctx, st); err != nil || n != 1 {
		t.Fatalf("sync after the engine is back %d %v", n, err)
	}
	if ids, _ := v.Search(ctx, "zebracorn hunter2", 3); len(ids) != 0 {
		t.Fatalf("re-embedded record found by forgotten words: %v", ids)
	}
}

// A slow embed in Sync never holds up a Search, and Search honours its context.
func TestVectorsSearchNotBlockedBySync(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	write(t, st, create("task-eeee5555", logbook.TypeTask, map[string]any{"title": "rotate the signing certificate"}))
	eng := &testEngine{}
	v := &Vectors{Engine: eng}
	if _, err := v.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		write(t, st, create(logbook.NewID(logbook.TypeTask), logbook.TypeTask, map[string]any{"title": fmt.Sprint("other ", i)}))
	}
	block := make(chan struct{})
	eng.set(func(e *testEngine) { e.block = block })
	done := make(chan error, 1)
	go func() { _, err := v.Sync(ctx, st); done <- err }()
	waitUntil(t, func() bool { eng.mu.Lock(); defer eng.mu.Unlock(); return len(eng.calls) >= 2 })
	// The query's one text is not held: the index lock is what is tested.
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	ids, err := v.Search(sctx, "signing certificate", 3)
	cancel()
	if err != nil || len(ids) == 0 || ids[0] != "task-eeee5555" || time.Since(start) > time.Second {
		t.Fatalf("search during a slow sync: %v %v after %v", ids, err, time.Since(start))
	}
	// A search whose own embed is slow ends at its context.
	slow := &Vectors{Engine: &testEngine{block: block, all: true}}
	sctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = slow.Search(sctx, "signing", 3)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow search: %v", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Text is capped on a rune boundary, and a record the engine refuses alone is marked and never
// stops the records after it.
func TestVectorsOversizedRecord(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("é", MaxText)
	r := &logbook.Record{Title: "t", Body: long, Notes: []logbook.Note{{Text: "late note"}}}
	if txt := Text(r); len(txt) > MaxText || !utf8.ValidString(txt) || strings.Contains(txt, "late note") {
		t.Fatalf("text %d bytes, valid %v", len(txt), utf8.ValidString(txt))
	}
	st := newStore(t)
	write(t, st, create("task-aaaa0001", logbook.TypeTask, map[string]any{"title": "poison pill"}))
	write(t, st, create("task-aaaa0002", logbook.TypeTask, map[string]any{"title": "rotate the signing certificate"}))
	eng := &testEngine{refuse: "poison"}
	v := &Vectors{Engine: eng, Path: filepath.Join(t.TempDir(), "v.gob")}
	if n, err := v.Sync(ctx, st); err != nil || n != 1 {
		t.Fatalf("sync %d %v", n, err)
	}
	if ids, _ := v.Search(ctx, "signing certificate", 3); !slices.Equal(ids, []string{"task-aaaa0002"}) {
		t.Fatalf("search %v", ids)
	}
	// The refused record is not asked for again until its text changes.
	eng.set(func(e *testEngine) { e.calls = nil })
	write(t, st, create("task-aaaa0003", logbook.TypeTask, map[string]any{"title": "third"}))
	if n, err := v.Sync(ctx, st); err != nil || n != 1 || len(eng.calls) != 1 || len(eng.calls[0]) != 1 {
		t.Fatalf("second sync %d %v, calls %q", n, err, eng.calls)
	}
	write(t, st, logbook.Step{Kind: logbook.KindSet, Record: "task-aaaa0001", Field: "title", Value: "cured"})
	if n, err := v.Sync(ctx, st); err != nil || n != 1 {
		t.Fatalf("sync after the text changed %d %v", n, err)
	}
	if _, err := os.Stat(v.Path); err != nil {
		t.Fatal(err)
	}
}
