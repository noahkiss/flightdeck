package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

func kindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

func tool(id, name, status string, input any) api.ChatMessage {
	b, _ := json.Marshal(input)
	return api.ChatMessage{ID: id, Kind: api.ChatKindTool, At: "2026-01-01T10:00:0" + id[len(id)-1:] + "Z",
		Tool: &api.ChatTool{Name: name, Status: api.ToolStatus(status), Input: b}}
}

// TestTouches: each tool shape yields the right file, image and change marks; a failed or
// denied edit changes nothing; relative paths take the folder; the last call names the item.
func TestTouches(t *testing.T) {
	msgs := []api.ChatMessage{
		{ID: "u1", Kind: api.ChatKindUser, Text: "hi"},
		tool("m1", "Write", "done", map[string]string{"file_path": "/w/a.go", "content": "package a\n"}),
		tool("m2", "Edit", "done", map[string]string{"file_path": "/w/a.go", "old_string": "a", "new_string": "b"}),
		tool("m3", "Edit", "error", map[string]string{"file_path": "/w/failed.go", "old_string": "a", "new_string": "b"}),
		tool("m4", "Read", "done", map[string]string{"file_path": "shots/s.png"}),
		tool("m5", "Read", "done", map[string]string{"file_path": "/w/notes.txt"}),
		tool("m6", "MultiEdit", "done", map[string]any{"file_path": "/w/b.go", "edits": []map[string]string{{"old_string": "x", "new_string": "y"}, {"old_string": "p", "new_string": "q"}}}),
		tool("m7", "apply_patch", "done", map[string]any{"/w/c.py": map[string]string{"type": "update", "unified_diff": "@@ -1 +1 @@\n-a\n+b\n"}, "/w/d.py": map[string]string{"type": "delete"}}),
		{ID: "m8", Kind: api.ChatKindTool, Tool: &api.ChatTool{Name: "view_image", Status: "done", Target: "/tmp/v.jpg"}},
		tool("m9", "Edit", "denied", map[string]string{"file_path": "/w/denied.go", "old_string": "a", "new_string": "b"}),
	}
	got := map[string]Touch{}
	for _, t := range Touches(msgs, "/w") {
		got[t.Path] = t
	}
	if a := got["/w/a.go"]; !a.Changed || a.Image || a.Message != "m2" || len(a.Edits) != 2 || !a.Edits[0].Write {
		t.Errorf("a.go: %+v", a)
	}
	if _, ok := got["/w/failed.go"]; ok {
		t.Error("a failed edit counts")
	}
	if _, ok := got["/w/denied.go"]; ok {
		t.Error("a denied edit counts")
	}
	if s := got["/w/shots/s.png"]; !s.Image || s.Changed {
		t.Errorf("relative image read: %+v", s)
	}
	if _, ok := got["/w/notes.txt"]; ok {
		t.Error("a read of a text file counts")
	}
	if b := got["/w/b.go"]; len(b.Edits) != 2 || !b.Changed {
		t.Errorf("multiedit: %+v", b)
	}
	if c := got["/w/c.py"]; c.Edits[0].Diff == "" || !c.Changed {
		t.Errorf("apply_patch update: %+v", c)
	}
	if d := got["/w/d.py"]; !d.Edits[0].Deleted {
		t.Errorf("apply_patch delete: %+v", d)
	}
	if v := got["/tmp/v.jpg"]; !v.Image {
		t.Errorf("view_image: %+v", v)
	}
	if n := len(Touches(msgs, "")); n != len(got)-1 {
		t.Errorf("with no folder the relative path should drop: %d of %d", n, len(got))
	}
}

func TestKinds(t *testing.T) {
	for name, want := range map[string]api.ContentKind{
		"a.PNG": api.ContentKindImage, "r.md": api.ContentKindMarkdown, "d.pdf": api.ContentKindPdf,
		"p.html": api.ContentKindHtml, "x.go": api.ContentKindText, "f.zip": api.ContentKindFile,
		"c.patch": api.ContentKindDiff, "noext": api.ContentKindFile,
	} {
		if got := KindOf(name); got != want {
			t.Errorf("KindOf(%s) = %s, want %s", name, got, want)
		}
	}
	if TypeOf("a.md") != "text/markdown" || TypeOf("a.png") != "image/png" || TypeOf("a.pdf") != "application/pdf" {
		t.Errorf("types: %s %s %s", TypeOf("a.md"), TypeOf("a.png"), TypeOf("a.pdf"))
	}
	if ID("s1", "/a") == ID("s2", "/a") || ID("s1", "/a") != ID("s1", "/a") {
		t.Error("ids")
	}
}

// TestStore: a show of the same path replaces the reference and moves it last, the cap drops
// the oldest, the file survives a reopen, and prune drops gone sessions.
func TestStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "content.json")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Add("s1", Shown{Path: "/a", At: "1"})
	s.Add("s1", Shown{Path: "/b", At: "2"})
	s.Add("s1", Shown{Path: "/a", At: "3", Title: "again"})
	l := s.List("s1")
	if len(l) != 2 || l[0].Path != "/b" || l[1].Title != "again" {
		t.Fatalf("list %+v", l)
	}
	for i := 0; i < MaxShown+5; i++ {
		s.Add("s2", Shown{Path: "/f" + string(rune('a'+i%26)) + strings.Repeat("x", i)})
	}
	if n := len(s.List("s2")); n != MaxShown {
		t.Fatalf("cap: %d", n)
	}
	s2, err := OpenStore(p)
	if err != nil || len(s2.List("s1")) != 2 {
		t.Fatalf("reopen: %v %+v", err, s2.List("s1"))
	}
	s2.SetFolder("s1", "/w/one")
	s2.SetFolder("s2", "/w/two")
	if s4, _ := OpenStore(p); s4.Folder("s1") != "/w/one" {
		t.Fatalf("folder after reopen %q", s4.Folder("s1"))
	}
	s2.Prune(func(id string) bool { return id == "s2" })
	s3, _ := OpenStore(p)
	if len(s3.List("s1")) != 0 || len(s3.List("s2")) != MaxShown || s3.Folder("s1") != "" || s3.Folder("s2") != "/w/two" {
		t.Fatal("prune")
	}
}

// world is a service over one session with a hand-made conversation.
type world struct {
	dir   string
	msgs  []api.ChatMessage
	cwd   string
	pub   []api.ContentItem
	now   time.Time
	svc   *Service
	store *Store
}

func newWorld(t *testing.T) *world {
	w := &world{dir: t.TempDir(), now: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
	if r, err := filepath.EvalSymlinks(w.dir); err == nil {
		w.dir = r
	}
	w.store, _ = OpenStore("")
	w.svc = New(Config{
		Store:   w.store,
		Session: func(id string) (api.Session, bool) { return api.Session{ID: id}, id == "s1" || id == "s2" },
		PaneSession: func(tok string) (string, bool) {
			return "s1", tok == "tok1"
		},
		Conversation: func(ctx context.Context, s string) ([]api.ChatMessage, string, error) {
			if s != "s1" {
				return nil, "", nil
			}
			return w.msgs, w.cwd, nil
		},
		Publish:  func(it api.ContentItem) { w.pub = append(w.pub, it) },
		Now:      func() time.Time { return w.now },
		Logf:     t.Logf,
		CacheFor: time.Nanosecond,
	})
	return w
}

func (w *world) file(t *testing.T, rel, data string) string {
	t.Helper()
	p := filepath.Join(w.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *world) list(t *testing.T, session string) map[string]api.ContentItem {
	t.Helper()
	l, err := w.svc.List(context.Background(), api.ContentListParams{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]api.ContentItem{}
	for _, it := range l.Items {
		out[it.Path] = it
	}
	return out
}

// TestShow: a show stores the reference and sends content.updated; the token names the
// session; relative paths, directories, FIFOs, missing files, unknown kinds, a long title and
// an unknown session are refused.
func TestShow(t *testing.T) {
	w := newWorld(t)
	png := w.file(t, "shot.png", "\x89PNG fake")
	ctx := context.Background()
	res, err := w.svc.Show(ctx, api.ContentShowParams{PaneToken: "tok1", Path: png, Title: "The bug"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != "s1" || res.ID != ID("s1", png) || len(w.pub) != 1 {
		t.Fatalf("show: %+v %+v", res, w.pub)
	}
	if p := w.pub[0]; !p.Shown || p.Kind != api.ContentKindImage || p.Title != "The bug" || p.Size == nil || *p.Size != 9 || p.Type != "image/png" {
		t.Fatalf("event %+v", p)
	}
	fifo := filepath.Join(w.dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		p    api.ContentShowParams
		want api.ErrorKind
	}{
		"relative":   {api.ContentShowParams{Session: "s1", Path: "shot.png"}, api.ErrorKindInvalid},
		"dotdot":     {api.ContentShowParams{Session: "s1", Path: "../shot.png"}, api.ErrorKindInvalid},
		"directory":  {api.ContentShowParams{Session: "s1", Path: w.dir}, api.ErrorKindInvalid},
		"fifo":       {api.ContentShowParams{Session: "s1", Path: fifo}, api.ErrorKindInvalid},
		"missing":    {api.ContentShowParams{Session: "s1", Path: filepath.Join(w.dir, "nope.png")}, api.ErrorKindNotFound},
		"kind":       {api.ContentShowParams{Session: "s1", Path: png, Kind: "video"}, api.ErrorKindInvalid},
		"title":      {api.ContentShowParams{Session: "s1", Path: png, Title: strings.Repeat("t", MaxTitle+1)}, api.ErrorKindInvalid},
		"no session": {api.ContentShowParams{Session: "s9", Path: png}, api.ErrorKindNotFound},
		"bad token":  {api.ContentShowParams{PaneToken: "nope", Path: png}, api.ErrorKindNotFound},
		"both":       {api.ContentShowParams{Session: "s1", PaneToken: "tok1", Path: png}, api.ErrorKindInvalid},
	} {
		if _, err := w.svc.Show(ctx, c.p); kindOf(err) != c.want {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	if len(w.pub) != 1 {
		t.Errorf("a refused show sent an event: %d", len(w.pub))
	}
}

// TestReadBounds: reads come in chunks with size and modified time; only listed ids are read
// (another session's id, a made-up id, an unreferenced file); offsets past the end, a file
// over the cap and a file replaced by a FIFO are refused; the chunk cap holds.
func TestReadBounds(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("0123456789"), 300_000) // 3 MB
	big := w.file(t, "big.txt", string(data))
	res, err := w.svc.Show(ctx, api.ContentShowParams{Session: "s1", Path: big})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	var off int64
	for {
		r, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID, Offset: off, Limit: 4 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Data) > MaxChunk {
			t.Fatalf("chunk of %d bytes", len(r.Data))
		}
		if r.Size != int64(len(data)) || r.Modified == "" || r.Offset != off {
			t.Fatalf("chunk meta %+v", r)
		}
		got = append(got, r.Data...)
		off += int64(len(r.Data))
		if r.EOF {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatal("bytes differ")
	}
	r, _ := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID})
	if len(r.Data) != DefaultChunk {
		t.Errorf("default chunk %d", len(r.Data))
	}
	if _, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID, Offset: int64(len(data)) + 1}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("past the end: %v", err)
	}
	if _, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID, Offset: -1}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("negative offset: %v", err)
	}
	// Ids from elsewhere are not readable: another session's, one made from a path nobody
	// referenced, and nonsense.
	secret := w.file(t, "secret.txt", "no")
	for _, p := range []api.ContentReadParams{
		{Session: "s2", ID: res.ID},
		{Session: "s1", ID: ID("s1", secret)},
		{Session: "s1", ID: ID("s1", "/etc/passwd")},
		{Session: "s1", ID: "../../etc/passwd"},
	} {
		if _, err := w.svc.Read(ctx, p); kindOf(err) != api.ErrorKindNotFound {
			t.Errorf("read %+v: %v", p, err)
		}
	}
	// Over the cap: a sparse file.
	huge := filepath.Join(w.dir, "huge.bin")
	f, _ := os.Create(huge)
	f.Truncate(MaxFile + 1)
	f.Close()
	hres, err := w.svc.Show(ctx, api.ContentShowParams{Session: "s1", Path: huge})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: hres.ID}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("over the cap: %v", err)
	}
	// Replaced by a FIFO after the show: refused, and the call does not hang.
	os.Remove(big)
	if err := syscall.Mkfifo(big, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID})
		done <- err
	}()
	select {
	case err := <-done:
		if kindOf(err) != api.ErrorKindInvalid {
			t.Errorf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read of a FIFO hangs")
	}
	// Gone: not_found, and the list keeps the item with no size.
	os.Remove(big)
	if _, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: res.ID}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("gone: %v", err)
	}
	if it := w.list(t, "s1")[big]; it.ID == "" || it.Size != nil {
		t.Errorf("gone item %+v", it)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// TestDerivedAndDiffs: tool calls and git status of the session's folder make the list; a
// symlink in the repository that leads out is left out; diffs come from git for a tracked
// file, every line for an untracked one, and the edit calls for a file outside the
// repository.
func TestDerivedAndDiffs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	w := newWorld(t)
	repo := filepath.Join(w.dir, "repo")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	tracked := w.file(t, "repo/app.go", "package app\n\nfunc A() {}\n")
	w.file(t, "repo/gone.go", "package app\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	w.file(t, "repo/app.go", "package app\n\nfunc B() {}\n")
	os.Remove(filepath.Join(repo, "gone.go"))
	untracked := w.file(t, "repo/new.md", "# New\n\nline\n")
	shot := w.file(t, "repo/shot.png", "png")
	outside := w.file(t, "elsewhere/notes.txt", "hello world\n")
	w.file(t, "elsewhere/secret.txt", "secret\n")
	if err := os.Symlink(filepath.Join(w.dir, "elsewhere", "secret.txt"), filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	w.cwd = repo
	w.msgs = []api.ChatMessage{
		tool("m1", "Edit", "done", map[string]string{"file_path": tracked, "old_string": "A", "new_string": "B"}),
		tool("m2", "Write", "done", map[string]string{"file_path": "shot.png", "content": "png"}),
		tool("m3", "Edit", "done", map[string]string{"file_path": outside, "old_string": "hello", "new_string": "hello world"}),
	}
	items := w.list(t, "s1")
	if it := items[tracked]; it.Change != api.ContentChangeModified || it.Message != "m1" || it.At == "" {
		t.Errorf("tracked %+v", it)
	}
	if it := items[shot]; it.Kind != api.ContentKindImage || it.Change != api.ContentChangeUntracked || it.Message != "m2" {
		t.Errorf("image %+v", it)
	}
	if it := items[untracked]; it.Change != api.ContentChangeUntracked || it.At != "" || it.Kind != api.ContentKindMarkdown {
		t.Errorf("untracked %+v", it)
	}
	if it := items[filepath.Join(repo, "gone.go")]; it.Change != api.ContentChangeDeleted || it.Size != nil {
		t.Errorf("deleted %+v", it)
	}
	if _, ok := items[filepath.Join(repo, "link.txt")]; ok {
		t.Error("a symlink that leads out of the repository is listed")
	}
	if it := items[outside]; it.Change != api.ContentChangeModified {
		t.Errorf("outside %+v", it)
	}
	ctx := context.Background()
	d, err := w.svc.Diff(ctx, api.ContentDiffParams{Session: "s1", ID: ID("s1", tracked)})
	if err != nil || d.Source != api.ContentDiffSourceGit || !strings.Contains(d.Diff, "-func A() {}") || !strings.Contains(d.Diff, "+func B() {}") {
		t.Errorf("git diff %+v %v", d, err)
	}
	d, err = w.svc.Diff(ctx, api.ContentDiffParams{Session: "s1", ID: ID("s1", untracked)})
	if err != nil || d.Source != api.ContentDiffSourceUntracked || !strings.Contains(d.Diff, "@@ -0,0 +1,3 @@\n+# New\n+\n+line\n") {
		t.Errorf("untracked diff %+v %v", d, err)
	}
	d, err = w.svc.Diff(ctx, api.ContentDiffParams{Session: "s1", ID: ID("s1", outside)})
	if err != nil || d.Source != api.ContentDiffSourceTranscript || !strings.Contains(d.Diff, "-hello\n+hello world\n") {
		t.Errorf("transcript diff %+v %v", d, err)
	}
	if _, err := w.svc.Diff(ctx, api.ContentDiffParams{Session: "s1", ID: ID("s1", filepath.Join(repo, "link.txt"))}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("diff of a left-out path: %v", err)
	}
	// The items read back: a derived image and a git-only file.
	r, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: ID("s1", shot)})
	if err != nil || string(r.Data) != "png" || !r.EOF {
		t.Errorf("read derived %+v %v", r, err)
	}
	if _, err := w.svc.Read(ctx, api.ContentReadParams{Session: "s1", ID: ID("s1", filepath.Join(w.dir, "elsewhere", "secret.txt"))}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("read of the symlink target: %v", err)
	}
	// After a hab restart the transcript is out of reach until the next hook: git status
	// still comes from the folder the store recorded.
	w.msgs, w.cwd = nil, ""
	if it := w.list(t, "s1")[untracked]; it.Change != api.ContentChangeUntracked {
		t.Errorf("with no transcript the recorded folder should still list git status: %+v", it)
	}
}

// TestInside: the escape check follows symlinks, and checks a missing path through its parent.
func TestInside(t *testing.T) {
	d := t.TempDir()
	d, _ = filepath.EvalSymlinks(d)
	os.MkdirAll(filepath.Join(d, "top", "sub"), 0o755)
	os.Symlink("/", filepath.Join(d, "top", "root"))
	top := filepath.Join(d, "top")
	for p, want := range map[string]bool{
		filepath.Join(top, "sub"):             true,
		filepath.Join(top, "sub", "gone.txt"): true,
		filepath.Join(top, "root", "etc"):     false,
		filepath.Join(top, "..", "top2", "x"): false,
		filepath.Join(top, "root", "missing"): false,
		top:                                   true,
	} {
		if got := inside(top, p); got != want {
			t.Errorf("inside(%s) = %v", p, got)
		}
	}
}
