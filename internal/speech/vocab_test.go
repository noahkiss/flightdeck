package speech

import (
	"slices"
	"testing"
)

func TestTerms(t *testing.T) {
	got := Terms([]string{"internal/holderreg/registry.go", "internal/link/link_test.go", "Makefile", "docs/specs/voice.md"})
	want := []string{"internal", "holderreg", "registry.go", "registry", "link", "link_test.go", "link_test", "Makefile", "docs", "specs", "voice.md", "voice"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestFixup(t *testing.T) {
	terms := Terms([]string{"internal/holderreg/registry.go", "internal/link/link_test.go", "app/fetchUser.ts", "Makefile",
		"README.md", "server/Server.swift", "a.go", "cmd/gff-hab/main.go",
		"NOTICE", "LICENSE", ".github/workflows/ci.yml", "setup.py", "login/view.ts", "backup.sh"})
	for _, tc := range []struct{ in, want string }{
		{"Run the tests in link test dot go.", "Run the tests in link_test.go."},
		{"call fetch user from the view", "call fetchUser from the view"},
		{"Look in holder reg, then registry.", "Look in holderreg, then registry."},
		{"open the readme", "open the readme"},                                         // all capitals: never a term
		{"You will notice the license is MIT.", "You will notice the license is MIT."}, // NOTICE, LICENSE
		{"Open a PR on GitHub.", "Open a PR on GitHub."},                               // .github
		{"Please set up the server and log in.", "Please set up the server and log in."},
		{"Back up the files", "Back up the files"},
		{"Fetch user first", "Fetch user first"}, // a capital that would be lost
		{"git hub actions", "github actions"},    // the dot of .github is dropped
		{"edit make file and build", "edit Makefile and build"},
		{"start the server now", "start the server now"},             // plain term: no recase of one word
		{"long ago", "long ago"},                                     // a.go is too short to match
		{"fetch, user", "fetch, user"},                               // punctuation between the words
		{"build gff hab first", "build gff-hab first"},               // two words to a hyphenated folder
		{"(fetch user)", "(fetchUser)"},                              // outer punctuation stays
		{"nothing to change here", "nothing to change here"},         // no term
		{"fetchUser is already right", "fetchUser is already right"}, // exact already
		{"fetchuser in one word", "fetchUser in one word"},           // one word, marked term
	} {
		got, changed := Fixup(tc.in, terms)
		if got != tc.want || changed != (tc.in != tc.want) {
			t.Errorf("Fixup(%q) = %q, %v; want %q", tc.in, got, changed, tc.want)
		}
	}
}

// TestFixupAmbiguous: two terms with one key leave the words as spoken.
func TestFixupAmbiguous(t *testing.T) {
	got, changed := Fixup("see fetch user", []string{"fetchUser", "fetch_user"})
	if changed || got != "see fetch user" {
		t.Fatalf("got %q, %v", got, changed)
	}
	if got, changed := Fixup("anything", nil); changed || got != "anything" {
		t.Fatalf("no terms: %q", got)
	}
}
