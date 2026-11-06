package logbook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var person = &Actor{Kind: "person", Device: "cli"}

// seedExport writes a project, a task in it with tags, a link, notes and a priority, a finding,
// a deleted guide and a proposal.
func seedExport(t *testing.T, r *replica) {
	t.Helper()
	r.write(t, person, create("proj-aaaa1111", TypeProject, map[string]any{"title": "demo"}),
		add("proj-aaaa1111", "identities", "example.com/owner/demo"))
	r.write(t, person, create("task-bbbb2222", TypeTask, map[string]any{"title": "ship it", "body": "the body",
		"project": "proj-aaaa1111", "priority": 2}),
		add("task-bbbb2222", "tags", "release"), add("task-bbbb2222", "links", Link{Kind: LinkRelates, To: "find-cccc3333"}))
	r.clock.Advance(1000)
	r.write(t, person, note("task-bbbb2222", "first note"), note("task-bbbb2222", "second note"))
	r.write(t, person, create("find-cccc3333", TypeFinding, map[string]any{"title": "a finding", "body": "fixed text"}))
	r.write(t, person, create("guide-dddd4444", TypeGuide, map[string]any{"title": "old guide"}))
	r.write(t, person, set("guide-dddd4444", "deleted", true))
	r.write(t, person, Step{Kind: KindCreate, Record: "proposal-eeee5555", Type: TypeProposal,
		Fields: map[string]any{"title": "a proposal", "target": "record"}})
}

func TestExportImportKeepsIDs(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "a")
	seedExport(t, a)
	var buf bytes.Buffer
	n, err := a.Export(ctx, &buf)
	if err != nil || n != 5 {
		t.Fatalf("export: %d, %v", n, err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first ExportRecord
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.ID != "proj-aaaa1111" {
		t.Fatalf("first line is not the project: %s (%v)", lines[0], err)
	}
	task := a.get(t, "task-bbbb2222")

	b := newReplica(t, "b")
	b.clock.Advance(86_400_000) // the importing hab writes a day later
	res, err := b.Import(ctx, bytes.NewReader(buf.Bytes()), person)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res != (ImportResult{Written: 4, Skipped: 1}) {
		t.Fatalf("import result %+v", res)
	}
	got := b.get(t, "task-bbbb2222")
	if got.Title != "ship it" || got.Body != "the body" || got.Project != "proj-aaaa1111" || got.Status != "open" {
		t.Fatalf("imported task %+v", got)
	}
	if str(got.Fields["created"].Value) != stampTime(task.Created) || string(got.Fields["imported"].Value) != "true" {
		t.Fatalf("created %s imported %s, want %s and true", got.Fields["created"].Value, got.Fields["imported"].Value, stampTime(task.Created))
	}
	if string(got.Fields["priority"].Value) != "2" || len(got.Tags) != 1 || len(got.Links) != 1 || len(got.Notes) != 2 ||
		got.Notes[0].Text != "first note" {
		t.Fatalf("imported task fields %+v tags %v links %v notes %v", got.Fields, got.Tags, got.Links, got.Notes)
	}
	if got.Created.Origin != "SHA256:b" {
		t.Fatalf("the importing hab did not write it: %v", got.Created)
	}
	if g := b.get(t, "guide-dddd4444"); !g.Deleted {
		t.Fatal("deleted guide imported live")
	}
	if p := b.get(t, "proj-aaaa1111"); len(p.Identities) != 1 {
		t.Fatalf("project identities %v", p.Identities)
	}
	if _, err := b.Get(ctx, "proposal-eeee5555"); err != ErrNotFound {
		t.Fatalf("proposal imported: %v", err)
	}

	// An export of the import keeps the original dates and the mark.
	var again bytes.Buffer
	if _, err := b.Export(ctx, &again); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), `"created":"`+stampTime(task.Created)+`"`) || !strings.Contains(again.String(), `"imported":true`) {
		t.Fatalf("re-export lost the date or the mark:\n%s", again.String())
	}
	// A second run writes nothing.
	res, err = b.Import(ctx, bytes.NewReader(buf.Bytes()), person)
	if err != nil || res != (ImportResult{Exists: 4, Skipped: 1}) {
		t.Fatalf("second import %+v, %v", res, err)
	}
	// An imported record's created and imported never change.
	if _, err := b.Write(ctx, Batch{Actor: person, Steps: []Step{set("task-bbbb2222", "created", "2020-01-01T00:00:00Z")}}); !isInvalid(err) {
		t.Fatalf("set of created: %v", err)
	}
}

func TestImportRefusals(t *testing.T) {
	ctx := context.Background()
	for name, line := range map[string]string{
		"bad id":        `{"id":"Task_1","type":"task","title":"x","created":"2026-01-01T00:00:00Z"}`,
		"bad date":      `{"id":"task-abc","type":"task","title":"x","created":"yesterday"}`,
		"unknown key":   `{"id":"task-abc","type":"task","title":"x","colour":"red"}`,
		"no title":      `{"id":"task-abc","type":"task","title":""}`,
		"wrong field":   `{"id":"find-abc","type":"finding","title":"x","fields":{"priority":1}}`,
		"not json":      `{"id":`,
		"unknown type":  `{"id":"memo-abc","type":"memo","title":"x"}`,
		"bad status":    `{"id":"task-abc","type":"task","title":"x","status":"later"}`,
		"line too long": `{"id":"task-abc","type":"task","title":"x","body":"` + strings.Repeat("a", MaxImportLine) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newReplica(t, "r")
			in := `{"id":"task-ok1","type":"task","title":"kept"}` + "\n" + line + "\n"
			res, err := r.Import(ctx, strings.NewReader(in), person)
			if err == nil || !strings.Contains(err.Error(), "line 2") {
				t.Fatalf("err %v", err)
			}
			if res.Written != 1 {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

func TestBackupRestoresWorkingReplica(t *testing.T) {
	ctx := context.Background()
	a := newReplica(t, "a")
	seedExport(t, a)
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.db")
	if err := a.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := a.Backup(ctx, path); err == nil {
		t.Fatal("a second backup over the first")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode: %v %v", fi.Mode(), err)
	}
	// A write after the backup is lost by the restore, and comes back from a peer.
	late := a.write(t, person, create("task-ffff6666", TypeTask, map[string]any{"title": "after the backup"}))
	want := dump(t, a.Store)

	restored := filepath.Join(t.TempDir(), "logbook.db")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restored, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r := openReplica(t, "a", restored)
	if got := r.get(t, "task-bbbb2222"); got.Title != "ship it" || len(got.Notes) != 2 {
		t.Fatalf("restored task %+v", got)
	}
	if _, err := r.Get(ctx, "task-ffff6666"); err != ErrNotFound {
		t.Fatalf("restored replica holds a later write: %v", err)
	}
	// It searches and takes a write.
	if ids, err := r.Find(ctx, "finding", 10); err != nil || len(ids) != 1 {
		t.Fatalf("find: %v %v", ids, err)
	}
	// Its own later op comes back from a peer that holds it, then it writes past it.
	peer := newReplica(t, "peer")
	push(t, a, peer)
	push(t, peer, r)
	if got := r.get(t, "task-ffff6666"); got.Title != "after the backup" {
		t.Fatalf("own op not back: %+v", got)
	}
	if dump(t, r.Store) != want {
		t.Fatalf("restored replica differs after sync:\n%s\nwant\n%s", dump(t, r.Store), want)
	}
	res := r.write(t, person, create("task-gggg7777", TypeTask, map[string]any{"title": "after the restore"}))
	if res.Ops[0].Seq <= late.Ops[0].Seq {
		t.Fatalf("restored replica reused a seq: %v after %v", res.Ops[0], late.Ops[0])
	}
}
