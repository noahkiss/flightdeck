package push

import (
	"strings"
	"testing"
)

// TestCapcomMarksAndCard: a weather reply's push ends with the Apple Weather mark after the cut
// text (capcom-service.md 8.3); a calendar card pushes with the Add category (8.5).
func TestCapcomMarksAndCard(t *testing.T) {
	long := strings.Repeat("word ", 200)
	n := CapcomMessage("c1", "s1", long, false, "Apple Weather")
	if !strings.HasSuffix(n.Body, "\nApple Weather") || n.Category != CategoryCapcomReply {
		t.Fatalf("message %+v", n)
	}
	c := CapcomCard("c2", "s1", "Add to calendar: Dinner", false)
	if c.Category != CategoryCapcomCard || c.Item != "c2" || c.Kind != KindCapcom || c.Body != "Add to calendar: Dinner" {
		t.Fatalf("card %+v", c)
	}
}
