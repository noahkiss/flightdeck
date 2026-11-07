package gffclient

import (
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// The thread as text (capcom-service.md 7.7): one line per item, the newest user message
// with its mark, the tools line, a card with its id and choices, a divider, typing.
func TestThreadLines(t *testing.T) {
	user := api.CapcomAuthor{Kind: "user"}
	capcom := api.CapcomAuthor{Kind: "capcom"}
	items := []api.CapcomItem{
		{ID: "c1", Kind: "message", Author: user, Text: "first", Read: "2026-11-06T12:00:00Z"},
		{ID: "c2", Kind: "tools", Author: capcom, Tools: []api.CapcomToolCall{{Name: "WebSearch"}, {Name: "mcp__flightdeck__transcript_read"}}},
		{ID: "c3", Kind: "message", Author: capcom, Text: "an\nanswer"},
		{ID: "c4", Kind: "card", Author: api.CapcomAuthor{Kind: "flightdeck"}, Text: "Capcom wants to open example.com/x",
			Card: &api.CapcomCard{Type: "approval", Ref: "r1", Choices: []string{"allow", "allow_site", "deny"}}},
		{ID: "c5", Kind: "divider", Author: api.CapcomAuthor{Kind: "flightdeck"}, Text: "New conversation"},
		{ID: "c6", Kind: "message", Author: user, Text: "flyer", Attachments: []string{"a1"}},
		{ID: "c7", Kind: "message", Author: capcom, Text: "build failed", Event: true},
	}
	got := strings.Join(ThreadLines(items, true), "\n")
	want := strings.Join([]string{
		"you: first",
		"  · tools: WebSearch, transcript_read",
		"capcom: an answer",
		"[approval r1] Capcom wants to open example.com/x · allow/allow_site/deny",
		"──── New conversation ────",
		"you: flyer [1 image] · delivered",
		"capcom (wrote first): build failed",
		"capcom is typing…",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	items[3].Answer = &api.CapcomAnswer{Choice: "allow_site"}
	if l := ThreadLines(items, false)[3]; !strings.HasSuffix(l, " · allow_site") {
		t.Fatalf("answered card %q", l)
	}
}
