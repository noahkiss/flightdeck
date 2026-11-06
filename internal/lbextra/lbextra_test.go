package lbextra

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

var person = &logbook.Actor{Kind: "person", Device: "cli"}

func newStore(t *testing.T) *logbook.Store {
	t.Helper()
	st, err := logbook.Open(context.Background(), filepath.Join(t.TempDir(), "logbook.db"), logbook.Options{Origin: "SHA256:test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func write(t *testing.T, st *logbook.Store, steps ...logbook.Step) logbook.WriteResult {
	t.Helper()
	res, err := st.Write(context.Background(), logbook.Batch{Actor: person, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func create(id, typ string, f map[string]any) logbook.Step {
	return logbook.Step{Kind: logbook.KindCreate, Record: id, Type: typ, Fields: f}
}

func seed(t *testing.T, st *logbook.Store) {
	write(t, st, create("proj-aaaa1111", logbook.TypeProject, map[string]any{"title": "demo"}))
	write(t, st, create("task-bbbb2222", logbook.TypeTask, map[string]any{"title": "ship it", "project": "proj-aaaa1111"}),
		logbook.Step{Kind: logbook.KindAdd, Record: "task-bbbb2222", Field: "tags", Value: "release"})
	write(t, st, create("find-cccc3333", logbook.TypeFinding, map[string]any{"title": "general finding", "body": "plain"}))
	write(t, st, create("task-weird1", logbook.TypeTask, map[string]any{"title": "odd project", "project": "../../out"}))
}

func mdFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestWriteTree(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seed(t, st)
	dir := filepath.Join(t.TempDir(), "tree")
	res, err := WriteTree(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"general/find-cccc3333.md", "general/task-weird1.md", "proj-aaaa1111/proj-aaaa1111.md", "proj-aaaa1111/task-bbbb2222.md"}
	if got := mdFiles(t, dir); strings.Join(got, ",") != strings.Join(want, ",") || res.Written != 4 {
		t.Fatalf("files %v (%+v), want %v", got, res, want)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "proj-aaaa1111/task-bbbb2222.md"))
	if !strings.Contains(string(b), `id: "task-bbbb2222"`) || !strings.Contains(string(b), "# ship it") || !strings.Contains(string(b), `tags: ["release"]`) {
		t.Fatalf("markdown:\n%s", b)
	}
	// Same tree: nothing written. A deleted record's file goes.
	write(t, st, logbook.Step{Kind: logbook.KindSet, Record: "find-cccc3333", Field: "deleted", Value: true})
	if res, err = WriteTree(ctx, st, dir); err != nil || res.Written != 0 || res.Removed != 1 {
		t.Fatalf("second write %+v %v", res, err)
	}
	// A folder with other files is refused.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "mine.txt"), []byte("x"), 0o600)
	if _, err := WriteTree(ctx, st, other); !errors.Is(err, ErrNotTree) {
		t.Fatalf("other folder: %v", err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func TestGitExportForget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	st := newStore(t)
	seed(t, st)
	secret := "hunter2-secret-value"
	res := write(t, st, create("task-dddd4444", logbook.TypeTask, map[string]any{"title": "has a secret", "body": "the key is " + secret}))
	dir := filepath.Join(t.TempDir(), "export")
	g := GitExport{Dir: dir}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	r, err := g.Export(ctx, st, now)
	if err != nil || !r.Committed || r.Records != 5 {
		t.Fatalf("export %+v %v", r, err)
	}
	if n := len(mdFiles(t, dir)); n != 5 {
		t.Fatalf("%d files, want one per record", n)
	}
	// Nothing changed: no commit.
	if r, err := g.Export(ctx, st, now); err != nil || r.Committed {
		t.Fatalf("empty export committed: %+v %v", r, err)
	}
	// Forget the body: the next export's files hold no trace of it.
	write(t, st, logbook.Step{Kind: logbook.KindForget, Record: "task-dddd4444", Target: res.Ops[0]})
	if r, err := g.Export(ctx, st, now.Add(time.Hour)); err != nil || !r.Committed {
		t.Fatalf("export after forget %+v %v", r, err)
	}
	if err := exec.Command("git", "-C", dir, "grep", "-q", secret, "HEAD", "--").Run(); err == nil {
		t.Fatal("the forgotten text is in the export's last commit")
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if b, _ := os.ReadFile(p); !d.IsDir() && strings.Contains(string(b), secret) {
			t.Fatalf("%s holds the forgotten text", p)
		}
		return nil
	})
	if n := strings.Count(git(t, dir, "log", "--format=%an"), "Flightdeck Logbook"); n != 2 {
		t.Fatalf("%d commits by the export, want 2", n)
	}
	if lc := g.LastCommit(ctx); !lc.Equal(now.Add(time.Hour)) {
		t.Fatalf("last commit %v", lc)
	}
}

func TestBackupSlots(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seed(t, st)
	b := Backup{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 3}
	loc := time.FixedZone("test", -5*3600)
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, loc) }
	if s := LastSlot(at(6, 2)); !s.Equal(at(5, 3)) {
		t.Fatalf("slot before 03:00: %v", s)
	}
	runs := 0
	step := func(now time.Time) {
		if due, err := b.Due(now); err != nil {
			t.Fatal(err)
		} else if due {
			if _, err := b.Run(ctx, st, now); err != nil {
				t.Fatal(err)
			}
			runs++
		}
	}
	step(at(1, 12)) // first start: due at once
	step(at(1, 13))
	step(at(2, 4)) // after the night's slot
	step(at(2, 5))
	// Asleep from the 2nd to the 9th: one backup on waking, not seven.
	step(at(9, 10))
	step(at(9, 11))
	if runs != 3 {
		t.Fatalf("%d backups, want 3", runs)
	}
	for d := 10; d <= 12; d++ {
		step(at(d, 4))
	}
	stems, _ := b.stems()
	if len(stems) != 3 {
		t.Fatalf("kept %d, want 3", len(stems))
	}
	for _, s := range stems {
		for _, ext := range []string{".db", ".jsonl"} {
			if fi, err := os.Stat(filepath.Join(b.Dir, backupPrefix+s+ext)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s%s: %v", s, ext, err)
			}
		}
	}
	// The newest copy opens as a working replica.
	r, err := logbook.Open(ctx, filepath.Join(b.Dir, backupPrefix+stems[0]+".db"), logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rec, err := r.Get(ctx, "task-bbbb2222"); err != nil || rec.Title != "ship it" {
		t.Fatalf("restored: %v %v", rec, err)
	}
	if _, err := r.Write(ctx, logbook.Batch{Actor: person, Steps: []logbook.Step{create("", logbook.TypeTask, map[string]any{"title": "new"})}}); err != nil {
		t.Fatalf("write to the restored copy: %v", err)
	}
}

func TestRunnerAlwaysOnOnly(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seed(t, st)
	state := t.TempDir()
	on := false
	r := &Runner{Store: st, State: state, AlwaysOn: func() bool { return on }, Now: func() time.Time { return time.Now() }}
	r.Step(ctx)
	if _, err := os.Stat(filepath.Join(state, BackupDir)); !os.IsNotExist(err) {
		t.Fatal("a hab without the mark backed up")
	}
	on = true
	r.Step(ctx)
	if ents, _ := os.ReadDir(filepath.Join(state, BackupDir)); len(ents) != 2 {
		t.Fatalf("backup files %d, want 2", len(ents))
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hab.toml")
	os.WriteFile(p, []byte("[logbook.backup]\nkeep = 7\n[logbook.export]\ngit = \"/work/export\"\n[logbook.vectors]\nurl = \"http://127.0.0.1:1/v1/embeddings\"\nmodel = \"m\"\n"), 0o600)
	c, err := LoadConfig(p, "/work/state")
	if err != nil || c.Backup.Keep != 7 || c.Backup.Path != "/work/state/"+BackupDir || c.Export.Git != "/work/export" ||
		!c.VectorsOn() || c.Vectors.Index != "/work/state/"+VectorIndex {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := LoadConfig(filepath.Join(dir, "none.toml"), "/work/state"); err != nil || c.Backup.Keep != DefaultKeep || c.Export.Git != "" || c.VectorsOn() {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	for _, bad := range []string{"[logbook.export]\ngit = \"relative\"\n", "[logbook.vectors]\nurl = \"http://x\"\n", "[logbook.backup]\nkeep = -1\n"} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p, "/work/state"); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestVectors(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seed(t, st)
	write(t, st, create("task-eeee5555", logbook.TypeTask, map[string]any{"title": "rotate the signing certificate"}))
	path := filepath.Join(t.TempDir(), "v.gob")
	v := &Vectors{Engine: FakeEngine{}, Path: path}
	if n, err := v.Sync(ctx, st); err != nil || n != 5 {
		t.Fatalf("sync %d %v", n, err)
	}
	ids, err := v.Search(ctx, "certificate signing", 3)
	if err != nil || len(ids) == 0 || ids[0] != "task-eeee5555" {
		t.Fatalf("search %v %v", ids, err)
	}
	// Only changed records are embedded again; a deleted one leaves the index.
	write(t, st, logbook.Step{Kind: logbook.KindSet, Record: "task-eeee5555", Field: "deleted", Value: true})
	if n, err := v.Sync(ctx, st); err != nil || n != 0 {
		t.Fatalf("sync after delete %d %v", n, err)
	}
	if ids, _ := v.Search(ctx, "certificate signing", 3); len(ids) > 0 && ids[0] == "task-eeee5555" {
		t.Fatalf("deleted record found: %v", ids)
	}
	// The index reloads from its file.
	v2 := &Vectors{Engine: FakeEngine{}, Path: path}
	if n, err := v2.Sync(ctx, st); err != nil || n != 0 {
		t.Fatalf("reloaded index embedded %d again (%v)", n, err)
	}
}

func TestHTTPEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "m" {
			http.Error(w, "model", http.StatusBadRequest)
			return
		}
		type d struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []d `json:"data"`
		}
		for i := len(req.Input) - 1; i >= 0; i-- { // out of order on purpose
			out.Data = append(out.Data, d{i, []float32{float32(len(req.Input[i])), 1}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	vs, err := HTTPEngine{URL: srv.URL, Model: "m"}.Embed(context.Background(), []string{"a", "abc"})
	if err != nil || len(vs) != 2 || vs[0][0] != 1 || vs[1][0] != 3 {
		t.Fatalf("%v %v", vs, err)
	}
	if _, err := (HTTPEngine{URL: srv.URL, Model: "x"}).Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("a 400 answered")
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "docs", "examples", "hab.toml"), "/work/state")
	if err != nil || c.Backup.Keep != 30 || c.Export.Git != "" || c.VectorsOn() {
		t.Fatalf("%+v %v", c, err)
	}
}
