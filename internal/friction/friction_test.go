package friction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// TestStoreCaps: the folder keeps the newest Keep notes; a note older than TailAge loses its
// log tail and keeps its text; a note over MaxBytes drops its oldest tail lines (9.2, 11).
func TestStoreCaps(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), Dir)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := now.Add(-TailAge - time.Hour)
	line := Line{T: old, Proc: "hab", Text: "a line"}
	first := Note{T: old, Trace: trace.New(trace.Hab), Client: "app", Text: "old note", Log: []Line{line}}
	if err := Write(dir, first, now); err != nil {
		t.Fatal(err)
	}
	n, err := Find(dir, first.Trace)
	if err != nil || n.Text != "old note" || len(n.Log) != 0 || !n.Dropped {
		t.Fatalf("an old note after a write: %+v %v", n, err)
	}
	for i := range Keep {
		if err := Write(dir, Note{T: now.Add(time.Duration(i) * time.Second), Trace: trace.New(trace.Gff), Text: "n", Log: []Line{line}}, now); err != nil {
			t.Fatal(err)
		}
	}
	notes, bad, err := List(dir, time.Time{})
	if err != nil || bad != 0 || len(notes) != Keep {
		t.Fatalf("%d notes, %d bad, %v; want %d", len(notes), bad, err, Keep)
	}
	if _, err := Find(dir, first.Trace); err != ErrNotFound {
		t.Fatalf("the oldest note outlived the cap: %v", err)
	}
	if len(notes[0].Log) != 1 {
		t.Fatalf("a new note lost its tail: %+v", notes[0])
	}

	big := Note{T: now.Add(time.Hour), Trace: trace.New(trace.App), Text: "big"}
	for range 2000 {
		big.Log = append(big.Log, Line{T: now, Proc: "hab", Text: strings.Repeat("x", 400)})
	}
	if err := Write(dir, big, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName(big))
	st, err := os.Stat(path)
	if err != nil || st.Size() > MaxBytes || st.Mode().Perm() != 0o600 {
		t.Fatalf("a big note's file: %v", err)
	}
	got, err := Find(dir, strings.ToLower(big.Trace))
	if err != nil || got.LogCut == 0 || got.LogCut+len(got.Log) != 2000 || got.Text != "big" {
		t.Fatalf("a big note: cut %d, kept %d, %v", got.LogCut, len(got.Log), err)
	}
	if notes, _, _ := List(dir, now.Add(30*time.Minute)); len(notes) != 1 || notes[0].Trace != big.Trace {
		t.Fatalf("List since: %d notes", len(notes))
	}
}

func TestCheckTextAndTrace(t *testing.T) {
	t.Parallel()
	if s, err := CheckText("  a note\n"); err != nil || s != "a note" {
		t.Errorf("CheckText: %q %v", s, err)
	}
	for _, bad := range []string{"", " \t\n", strings.Repeat("é", MaxText+1), "\xff"} {
		if _, err := CheckText(bad); err == nil {
			t.Errorf("CheckText(%.20q) took it", bad)
		}
	}
	if _, err := CheckText(strings.Repeat("é", MaxText)); err != nil {
		t.Errorf("CheckText at the limit: %v", err)
	}
	if err := Write(t.TempDir(), Note{Trace: "../../x"}, time.Now()); err == nil {
		t.Error("Write took a bad trace")
	}
}
