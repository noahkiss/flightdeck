package trace

import (
	"context"
	"strings"
	"testing"
)

func TestNewIsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range []byte{App, Gff, Hab} {
		for range 200 {
			id := New(o)
			if !Valid(id) || id[0] != o {
				t.Fatalf("New(%c) = %q: not a valid trace of that origin", o, id)
			}
			if seen[id] {
				t.Fatalf("New made %q twice", id)
			}
			seen[id] = true
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"g0123456789A", "hZZZZZZZZZZZ", "a00000000000"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",
		"g0123456789",   // 11
		"g0123456789AB", // 13
		"x0123456789A",  // unknown origin
		"G0123456789A",  // upper-case origin
		"g0123456789a",  // lower case
		"g012345678IL",  // letters Crockford leaves out
		"g01234567 89",  // space
		"g01234567\n89",
		"gé123456789", // not ASCII
		strings.Repeat("g", 4096),
	}
	for _, s := range bad {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestCheck(t *testing.T) {
	if id, r := Check("g0123456789A"); id != "g0123456789A" || r {
		t.Errorf("Check(valid) = %q, %v", id, r)
	}
	if id, r := Check(""); !Valid(id) || id[0] != Hab || r {
		t.Errorf("Check(\"\") = %q, %v; want a new h trace, not replaced", id, r)
	}
	if id, r := Check("not a trace"); !Valid(id) || id[0] != Hab || !r {
		t.Errorf("Check(malformed) = %q, %v; want a new h trace, replaced", id, r)
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	if From(ctx) != "" {
		t.Fatal("a bare context carries a trace")
	}
	if With(ctx, "") != ctx {
		t.Fatal("With an empty id changed the context")
	}
	if got := From(With(ctx, "g0123456789A")); got != "g0123456789A" {
		t.Fatalf("From(With) = %q", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("G0123456789a"); got != "g0123456789A" || !Valid(got) {
		t.Errorf("Normalize = %q", got)
	}
	if Normalize("") != "" {
		t.Error("Normalize(\"\") is not empty")
	}
}

// TestNewAllocations: minting a trace allocates only its string (diagnostics.md 11).
func TestNewAllocations(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() { _ = New(Hab) }); n > 1 {
		t.Fatalf("New allocates %v times, want at most 1", n)
	}
}
