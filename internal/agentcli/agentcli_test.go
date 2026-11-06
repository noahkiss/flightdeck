package agentcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestUsageAndOutsidePane: no verb, an unknown verb and a run outside a pane are usage errors
// that call nothing; Text names the follow-up only when one was given.
func TestUsageAndOutsidePane(t *testing.T) {
	none := func(string) string { return "" }
	for _, args := range [][]string{nil, {"nonesuch"}, {"compact", "go on"}} {
		var out, errb bytes.Buffer
		if code := Run("gff", args, strings.NewReader(""), &out, &errb, none); code != exitUsage || out.Len() != 0 {
			t.Errorf("%q: exit %d, out %q", args, code, out.String())
		}
	}
	var errb bytes.Buffer
	Run("gff", []string{"compact"}, strings.NewReader(""), &bytes.Buffer{}, &errb, none)
	if !strings.Contains(errb.String(), "GFF_HAB is not set") {
		t.Errorf("outside a pane: %q", errb.String())
	}
	if s := Text(api.Compaction{ID: "cmp-1"}); !strings.Contains(s, "cmp-1") || strings.Contains(s, "follow-up") {
		t.Errorf("text %q", s)
	}
	if s := Text(api.Compaction{ID: "cmp-1", FollowUp: true}); !strings.Contains(s, "follow-up") {
		t.Errorf("text %q", s)
	}
}
