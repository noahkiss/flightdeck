package capcom

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the context goldens in testdata")

// goldenItems holds one item of each kind the hab resolves, with the fields
// cmd/gff-hab/capcom_context.go writes for that kind. The app's CapcomEntryTests reads the
// goldens to check its parser against the real format; keep the field names in step with the
// resolvers.
var goldenItems = []Item{
	{Kind: KindNotification, Ref: "n7", Host: "testhost", Title: "Build failed", Body: []Field{
		{Name: "title", Value: "Build failed"},
		{Name: "body", Value: "step 3 of 4\nexit 2"},
		{Name: "level", Value: "active"},
		{Name: "raised", Value: "2026-01-01T09:58:00Z"},
		{Name: "session", Value: "builder"},
	}, Hint: "Read it with notice_read id=n7 host=testhost."},
	{Kind: KindCommand, Ref: "%2:4", Host: "testhost", Title: "make test", Body: []Field{
		{Name: "command", Value: "make test"},
		{Name: "status", Value: "exit 2"},
		{Name: "folder", Value: "/work/app"},
		{Name: "pane", Value: "%2"},
	}, Hint: `Its output is in that pane, not in the read tools: gff-hab api call command.output '{"pane":"%2","command":4}' prints it.`},
	{Kind: KindFile, Ref: "a1", Host: "testhost", Title: "build log.txt", Body: []Field{
		{Name: "path", Value: "/work/capcom/attachments/build log.txt"},
		{Name: "size", Value: "512 bytes"},
		{Name: "type", Value: "text/plain"},
	}, Hint: "It is under your folder: read it with your own tools, or file_read path=/work/capcom/attachments/build log.txt host=testhost."},
	{Kind: KindSession, Ref: "s9", Host: "peer-b", Title: "api-refactor", Body: []Field{
		{Name: "name", Value: "api-refactor"},
		{Name: "state", Value: "running"},
		{Name: "agent", Value: "claude, working"},
		{Name: "title", Value: "Split the handler"},
	}, Hint: "Read its transcript with transcript_read session=s9 host=peer-b; agents shows its state now."},
}

// TestContextGolden fails when Message's output drifts from the goldens: a message with every
// kind and the user's text, and the same blocks alone. -update rewrites them.
func TestContextGolden(t *testing.T) {
	for _, c := range []struct{ file, text string }{
		{"context-message.txt", "is this safe?"},
		{"context-alone.txt", ""},
	} {
		got := Message(goldenItems, c.text)
		path := filepath.Join("testdata", c.file)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to write it)", err)
		}
		if got != string(want) {
			t.Errorf("%s drifted from Message's output; run with -update and check the app's CapcomEntryTests\ngot:\n%s\nwant:\n%s", c.file, got, want)
		}
	}
}
