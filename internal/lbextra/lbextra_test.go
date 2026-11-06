package lbextra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	os.WriteFile(p, []byte("[logbook.backup]\nkeep = 7\n[logbook.export]\ngit = \"/work/export\"\n[logbook.vectors]\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\nkey_command = [\"printf\", \"k\"]\n"), 0o600)
	c, err := LoadConfig(p, "/work/state")
	if err != nil || c.Backup.Keep != 7 || c.Backup.Path != "/work/state/"+BackupDir || c.Export.Git != "/work/export" ||
		!c.VectorsOn() || c.Vectors.Index != "/work/state/"+VectorIndex || c.Vectors.BaseURL != "http://127.0.0.1:1/v1" ||
		c.Vectors.Model != "m" || !slices.Equal(c.Vectors.KeyCommand, []string{"printf", "k"}) {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := LoadConfig(filepath.Join(dir, "none.toml"), "/work/state"); err != nil || c.Backup.Keep != DefaultKeep || c.Export.Git != "" || c.VectorsOn() {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	for bad, want := range map[string]string{
		"[logbook.export]\ngit = \"relative\"\n": "git",
		"[logbook.backup]\nkeep = -1\n":          "keep",
		// The old key is refused, naming its replacement, so an old config is noticed.
		"[logbook.vectors]\nurl = \"http://127.0.0.1:1/v1/embeddings\"\nmodel = \"m\"\n":                        "base_url",
		"[logbook.vectors]\nbase_url = \"http://127.0.0.1:1/v1\"\n":                                             "model",
		"[logbook.vectors]\nmodel = \"m\"\n":                                                                    "base_url",
		"[logbook.vectors]\nbase_url = \"ftp://127.0.0.1/v1\"\nmodel = \"m\"\n":                                 "http or https",
		"[logbook.vectors]\nbase_url = \"http:///v1\"\nmodel = \"m\"\n":                                         "http or https",
		"[logbook.vectors]\nbase_url = \"not a url\"\nmodel = \"m\"\n":                                          "http or https",
		"[logbook.vectors]\nbase_url = \"http://u:p@127.0.0.1/v1\"\nmodel = \"m\"\n":                            "use key_command",
		"[logbook.vectors]\nbase_url = \"http://127.0.0.1/v1\"\nmodel = \"m\"\nkey_command = []\n":              "no program",
		"[logbook.vectors]\nbase_url = \"http://127.0.0.1/v1\"\nmodel = \"m\"\nkey_command = [\"\"]\n":          "no program",
		"[logbook.vectors]\nbase_url = \"http://127.0.0.1/v1\"\nmodel = \"m\"\nkey_command = [\"  \", \"x\"]\n": "no program",
		"[logbook.vectors]\nbase_url = \"http://127.0.0.1/v1\"\nmodel = \"m\"\nkey_command = \"printf k\"\n":    "key_command",
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p, "/work/state"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: %v, want an error naming %q", bad, err, want)
		}
	}
	// A section with neither base_url nor model is off, as [summaries] is.
	os.WriteFile(p, []byte("[logbook.vectors]\nindex = \"/work/v.gob\"\n"), 0o600)
	if c, err := LoadConfig(p, "/work/state"); err != nil || c.VectorsOn() || c.Vectors.Index != "/work/v.gob" {
		t.Fatalf("index only: %+v %v", c, err)
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

// embedServer is a fake OpenAI-compatible embeddings server: it answers POST /v1/embeddings
// only, refuses a key other than want ("" wants no Authorization header), and records each
// Authorization header it saw.
type embedServer struct {
	*httptest.Server
	mu    sync.Mutex
	want  string
	auths []string
	paths []string
}

func newEmbedServer(t *testing.T, want string) *embedServer {
	es := &embedServer{want: want}
	es.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.auths = append(es.auths, r.Header.Get("Authorization"))
		es.paths = append(es.paths, r.URL.Path)
		want := es.want
		es.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); (want == "" && got != "") || (want != "" && got != "Bearer "+want) {
			http.Error(w, "key", http.StatusUnauthorized)
			return
		}
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
	t.Cleanup(es.Close)
	return es
}

func (es *embedServer) seen() (auths, paths []string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	return slices.Clone(es.auths), slices.Clone(es.paths)
}

// logs collects log lines for a test.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// embedSoon calls Embed until the key has loaded (ErrNoKey) or the deadline passes.
func embedSoon(t *testing.T, e *HTTPEngine, texts []string) ([][]float32, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		vs, err := e.Embed(context.Background(), texts)
		if !errors.Is(err, ErrNoKey) || time.Now().After(deadline) {
			return vs, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHTTPEngine(t *testing.T) {
	es := newEmbedServer(t, "")
	// The base URL with a trailing slash still reaches <base_url>/embeddings.
	for _, base := range []string{es.URL + "/v1", es.URL + "/v1/"} {
		e := NewHTTPEngine(base, "m", nil, EngineOptions{})
		vs, err := e.Embed(context.Background(), []string{"a", "abc"})
		if err != nil || len(vs) != 2 || vs[0][0] != 1 || vs[1][0] != 3 {
			t.Fatalf("%s: %v %v", base, vs, err)
		}
		e.Close()
	}
	if auths, paths := es.seen(); !slices.Equal(paths, []string{"/v1/embeddings", "/v1/embeddings"}) || auths[0] != "" {
		t.Fatalf("paths %q auths %q", paths, auths)
	}
	e := NewHTTPEngine(es.URL+"/v1", "x", nil, EngineOptions{})
	defer e.Close()
	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("a 400 answered")
	}
	// A server that cannot be reached is named without its URL.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	e2 := NewHTTPEngine(gone.URL+"/v1?token=secret-query", "m", nil, EngineOptions{})
	defer e2.Close()
	if _, err := e2.Embed(context.Background(), []string{"a"}); err == nil || strings.Contains(err.Error(), "secret-query") {
		t.Fatalf("unreachable: %v", err)
	}
}

// TestHTTPEngineKeyCommand: key_command's output goes in the Authorization header, and the
// key is in no log or error.
func TestHTTPEngineKeyCommand(t *testing.T) {
	es := newEmbedServer(t, "k-secret-123")
	var l logs
	e := NewHTTPEngine(es.URL+"/v1", "m", []string{"printf", "\\nk-secret-123\\n"}, EngineOptions{Logf: l.logf})
	defer e.Close()
	vs, err := embedSoon(t, e, []string{"abc"})
	if err != nil || len(vs) != 1 || vs[0][0] != 3 {
		t.Fatalf("%v %v", vs, err)
	}
	if auths, _ := es.seen(); len(auths) != 1 || auths[0] != "Bearer k-secret-123" {
		t.Fatalf("auths %q", auths)
	}
	for _, line := range l.all() {
		if strings.Contains(line, "k-secret-123") {
			t.Fatalf("key in log: %q", line)
		}
	}
}

// TestHTTPEngineKeyCommandFails: a failing key_command sends nothing, fails each call cleanly,
// logs one line for the streak, and keeps its stderr (which may hold a secret) out of the log
// and the error.
func TestHTTPEngineKeyCommandFails(t *testing.T) {
	es := newEmbedServer(t, "k")
	var l logs
	e := NewHTTPEngine(es.URL+"/v1", "m", []string{"sh", "-c", "echo stderr-secret-456 >&2; exit 3"}, EngineOptions{Logf: l.logf})
	defer e.Close()
	waitUntil(t, func() bool { return len(l.all()) > 0 })
	for range 3 {
		if _, err := e.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrNoKey) || strings.Contains(err.Error(), "stderr-secret-456") {
			t.Fatalf("embed: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if lines := l.all(); len(lines) != 1 || strings.Contains(lines[0], "stderr-secret-456") || !strings.Contains(lines[0], "key_command") {
		t.Fatalf("logs %q", lines)
	}
	if auths, _ := es.seen(); len(auths) != 0 {
		t.Fatalf("a request went out: %q", auths)
	}
}

// TestHTTPEngineKeyRefused: a 401 runs the key command again once its backoff allows, and the
// new key is used.
func TestHTTPEngineKeyRefused(t *testing.T) {
	es := newEmbedServer(t, "new-key")
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	keys := []string{"old-key", "new-key"}
	runs := 0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := NewHTTPEngine(es.URL+"/v1", "m", nil, EngineOptions{Now: clock, KeyRun: func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		k := keys[min(runs, len(keys)-1)]
		runs++
		return k, nil
	}})
	defer e.Close()
	if _, err := embedSoon(t, e, []string{"a"}); err == nil {
		t.Fatal("the old key was accepted")
	}
	// Within the 5-minute wait after a good run, a refusal runs nothing.
	e.Embed(context.Background(), []string{"a"})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if runs != 1 {
		mu.Unlock()
		t.Fatalf("runs %d within the wait", runs)
	}
	now = now.Add(6 * time.Minute)
	mu.Unlock()
	e.Embed(context.Background(), []string{"a"}) // refused: starts a run
	waitUntil(t, func() bool {
		_, err := e.Embed(context.Background(), []string{"a"})
		return err == nil
	})
	if auths, _ := es.seen(); auths[len(auths)-1] != "Bearer new-key" {
		t.Fatalf("auths %q", auths)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "docs", "examples", "hab.toml"), "/work/state")
	if err != nil || c.Backup.Keep != 30 || c.Export.Git != "" || c.VectorsOn() {
		t.Fatalf("%+v %v", c, err)
	}
}
