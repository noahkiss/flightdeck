package harness

import (
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/internal/detect"
)

// TestImageMessageWrites: Claude Code gets one paste with each image path on its own line after
// the text; Codex gets each image path as its own paste, then the text, then Enter; with no
// images both get MessageWrites; other harnesses are unsupported.
func TestImageMessageWrites(t *testing.T) {
	a, b := "/w/p/.flightdeck/attachments/0123456789ab-a.png", "/w/p/.flightdeck/attachments/ba9876543210-b.jpg"
	cases := []struct {
		name, harness, text string
		images              []string
		want                []string
	}{
		{"claude", detect.KindClaude, "look", []string{a, b}, []string{pasteStart + "look\n\n" + a + "\n" + b + pasteEnd, "\r"}},
		{"claude images alone", detect.KindClaude, "", []string{a}, []string{pasteStart + a + pasteEnd, "\r"}},
		{"codex", detect.KindCodex, "look\n\n/w/p/log.txt", []string{a, b}, []string{pasteStart + a + pasteEnd, pasteStart + b + pasteEnd, pasteStart + "look\n\n/w/p/log.txt" + pasteEnd, "\r"}},
		{"codex images alone", detect.KindCodex, " ", []string{a}, []string{pasteStart + a + pasteEnd, "\r"}},
		{"no images", detect.KindCodex, "hi", nil, []string{pasteStart + "hi" + pasteEnd, "\r"}},
	}
	for _, c := range cases {
		got, err := ImageMessageWrites(c.harness, c.text, c.images)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: %q", c.name, got)
		}
		for i := range got {
			if string(got[i]) != c.want[i] {
				t.Errorf("%s: write %d = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
	if _, err := ImageMessageWrites("opencode", "x", []string{a}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("opencode: %v", err)
	}
	if got, err := ImageMessageWrites(detect.KindCodex, "x", []string{"/a\x1b[201~b.png"}); err != nil || string(got[0]) != pasteStart+"/a[201~b.png"+pasteEnd {
		t.Errorf("a path cannot end the paste: %q %v", got, err)
	}
}
