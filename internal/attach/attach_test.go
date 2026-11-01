package attach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	s        *Service
	clk      *clock
	project  string
	state    string
	readonly string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := t.TempDir()
	f := &fixture{clk: &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}, project: filepath.Join(d, "project"), state: filepath.Join(d, "state")}
	if err := os.Mkdir(f.project, 0o700); err != nil {
		t.Fatal(err)
	}
	f.readonly = filepath.Join(d, "readonly")
	if err := os.Mkdir(f.readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	f.s = f.open()
	t.Cleanup(func() { f.s.Close() })
	return f
}

func (f *fixture) open() *Service {
	return New(Config{
		StatePath: filepath.Join(f.state, "attachments.json"),
		Session:   func(id string) bool { return id == "s1" || id == "s2" || id == "nofolder" || id == "readonly" },
		Folder: func(_ context.Context, s string) string {
			switch s {
			case "nofolder":
				return ""
			case "readonly":
				return f.readonly
			}
			return f.project
		},
		Fallback: filepath.Join(f.state, "attachments"),
		Now:      f.clk.now,
	})
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func kindOf(t *testing.T, err error) api.ErrorKind {
	t.Helper()
	var e *control.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a control error", err)
	}
	return e.Data.Kind
}

// upload sends b in runs of run bytes and returns the stored path.
func (f *fixture) upload(t *testing.T, session, name string, b []byte, run int) (string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: session, Name: name, Type: "image/png", Size: uint64(len(b)), SHA256: sum(b)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Chunk != Chunk {
		t.Fatalf("chunk %d, want %d", st.Chunk, Chunk)
	}
	for off := 0; off < len(b); off += run {
		end := min(off+run, len(b))
		w, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: st.ID, Offset: int64(off), Data: b[off:end]})
		if err != nil {
			t.Fatal(err)
		}
		if w.Received != int64(end) || w.Done != (end == len(b)) {
			t.Fatalf("after %d: %+v", end, w)
		}
	}
	return st.ID, st.Path
}

// TestUploadInRuns: runs in order build the file at the path put named, inside the project's
// attachment folder, which ignores itself; get reads it back in runs; the id is in the path.
func TestUploadInRuns(t *testing.T) {
	f := newFixture(t)
	b := bytes.Repeat([]byte("0123456789"), 300_000) // 3 MB: three full runs and a short one
	id, path := f.upload(t, "s1", "screen shot.png", b, Chunk)
	if want := filepath.Join(f.project, Folder, id+"-screen-shot.png"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	if IDOfPath(path) != id {
		t.Errorf("IDOfPath(%s) = %q, want %s", path, IDOfPath(path), id)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("stored file differs (%v)", err)
	}
	ign, err := os.ReadFile(filepath.Join(f.project, ".flightdeck", ".gitignore"))
	if err != nil || !strings.Contains(string(ign), "\n*\n") {
		t.Errorf("ignore file: %q, %v", ign, err)
	}
	if parts, _ := filepath.Glob(filepath.Join(f.project, Folder, ".*.part")); len(parts) != 0 {
		t.Errorf("partial files left: %v", parts)
	}
	var back []byte
	for off := int64(0); ; {
		d, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id, Offset: off, Limit: 5 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Data) > Chunk || d.Size != int64(len(b)) || d.Offset != off || d.Name != "screen shot.png" || d.Type != "image/png" {
			t.Fatalf("run at %d: %d bytes, size %d, offset %d, %q %q", off, len(d.Data), d.Size, d.Offset, d.Name, d.Type)
		}
		back = append(back, d.Data...)
		off += int64(len(d.Data))
		if d.EOF {
			break
		}
	}
	if !bytes.Equal(back, b) {
		t.Error("get returned other bytes")
	}
	d, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id})
	if err != nil || len(d.Data) != DefaultRun {
		t.Errorf("default run: %d bytes, %v", len(d.Data), err)
	}
	// A retried last run answers done again.
	w, err := f.s.Write(context.Background(), api.AttachmentWriteParams{ID: id, Offset: int64(len(b)) - 10, Data: b[len(b)-10:]})
	if err != nil || !w.Done || w.Received != int64(len(b)) {
		t.Errorf("retried last run: %+v, %v", w, err)
	}
}

// TestWriteRules: an out-of-order run, a run past the size, an empty or oversize run, a bad hash
// and an unknown id each get their kind; a bad hash drops the upload and its partial file.
func TestWriteRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b := []byte("hello, agent")
	st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "a.txt", Type: "text/plain", Size: uint64(len(b)), SHA256: sum([]byte("other"))})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		p    api.AttachmentWriteParams
		want api.ErrorKind
	}{
		{"offset ahead", api.AttachmentWriteParams{ID: st.ID, Offset: 3, Data: b[3:]}, api.ErrorKindConflict},
		{"empty run", api.AttachmentWriteParams{ID: st.ID, Data: nil}, api.ErrorKindInvalid},
		{"oversize run", api.AttachmentWriteParams{ID: st.ID, Data: make([]byte, Chunk+1)}, api.ErrorKindInvalid},
		{"past the size", api.AttachmentWriteParams{ID: st.ID, Data: append(append([]byte{}, b...), 'x')}, api.ErrorKindInvalid},
		{"unknown id", api.AttachmentWriteParams{ID: "000000000000", Data: b}, api.ErrorKindNotFound},
	}
	for _, c := range cases {
		if _, err := f.s.Write(ctx, c.p); err == nil || kindOf(t, err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: st.ID, Data: b}); err == nil || kindOf(t, err) != api.ErrorKindInvalid {
		t.Fatalf("bad hash: %v", err)
	}
	if _, err := os.Stat(st.Path); err == nil {
		t.Error("a file with a bad hash was stored")
	}
	if parts, _ := filepath.Glob(filepath.Join(f.project, Folder, ".*.part")); len(parts) != 0 {
		t.Errorf("partial files left: %v", parts)
	}
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: st.ID, Data: b}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("write after a bad hash: %v", err)
	}
}

// TestPutRules: an unknown session, a missing name, a file over the limit and a malformed hash
// are refused; an empty file is done at once; a session with no folder stores under the
// fallback.
func TestPutRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	good := sum([]byte("x"))
	cases := []struct {
		name string
		p    api.AttachmentPutParams
		want api.ErrorKind
	}{
		{"unknown session", api.AttachmentPutParams{Session: "nope", Name: "a", Size: 1, SHA256: good}, api.ErrorKindNotFound},
		{"no name", api.AttachmentPutParams{Session: "s1", Name: " ", Size: 1, SHA256: good}, api.ErrorKindInvalid},
		{"over the limit", api.AttachmentPutParams{Session: "s1", Name: "a", Size: MaxSize + 1, SHA256: good}, api.ErrorKindInvalid},
		{"bad hash", api.AttachmentPutParams{Session: "s1", Name: "a", Size: 1, SHA256: "abc"}, api.ErrorKindInvalid},
	}
	for _, c := range cases {
		if _, err := f.s.Put(ctx, c.p); err == nil || kindOf(t, err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
	_, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "a", Size: MaxSize + 1, SHA256: good})
	if err == nil || !strings.Contains(err.Error(), "25 MB") {
		t.Errorf("the limit is not named: %v", err)
	}
	st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "empty.txt", Size: 0, SHA256: sum(nil)})
	if err != nil || !st.Done {
		t.Fatalf("empty file: %+v, %v", st, err)
	}
	if fi, err := os.Stat(st.Path); err != nil || fi.Size() != 0 {
		t.Errorf("empty file not stored: %v", err)
	}
	id, path := f.upload(t, "nofolder", "log.txt", []byte("log"), 2)
	if want := filepath.Join(f.state, "attachments", "nofolder", Folder, id+"-log.txt"); path != want {
		t.Errorf("fallback path %s, want %s", path, want)
	}
	if os.Geteuid() != 0 { // root writes in a read-only folder
		id, path = f.upload(t, "readonly", "log.txt", []byte("log"), 2)
		if want := filepath.Join(f.state, "attachments", "readonly", Folder, id+"-log.txt"); path != want {
			t.Errorf("a folder the hab cannot write: path %s, want %s", path, want)
		}
	}
}

// TestOpenLimitAndIdle: at most MaxOpen uploads at once; one with no write for UploadIdle is
// dropped with its partial file, and frees its place.
func TestOpenLimitAndIdle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < MaxOpen; i++ {
		st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "a", Size: 2, SHA256: sum([]byte("ab"))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, st.ID)
	}
	if _, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "a", Size: 2, SHA256: sum([]byte("ab"))}); err == nil || kindOf(t, err) != api.ErrorKindUnavailable {
		t.Fatalf("put past the limit: %v", err)
	}
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: ids[0], Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	f.clk.add(UploadIdle - time.Second)
	f.s.Sweep()
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: ids[0], Offset: 1, Data: []byte("b")}); err != nil {
		t.Fatalf("an upload written to within the idle time was dropped: %v", err)
	}
	f.clk.add(UploadIdle)
	f.s.Sweep()
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: ids[1], Data: []byte("a")}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("an idle upload was kept: %v", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(f.project, Folder, ".*.part")); len(parts) != 0 {
		t.Errorf("partial files left: %v", parts)
	}
	if _, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "a", Size: 2, SHA256: sum([]byte("ab"))}); err != nil {
		t.Errorf("put after the idle drop: %v", err)
	}
}

// TestRestart: the index survives a restart; the partial file of an upload open at the stop is
// removed at the next start; a file older than KeepFor is deleted, a moved one only forgotten.
func TestRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	keep, keepPath := f.upload(t, "s1", "keep.png", []byte("keep"), 4)
	old, oldPath := f.upload(t, "s1", "old.png", []byte("old"), 4)
	moved, movedPath := f.upload(t, "s1", "moved.png", []byte("moved"), 4)
	if _, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "half", Size: 4, SHA256: sum([]byte("half"))}); err != nil {
		t.Fatal(err)
	}
	parts, _ := filepath.Glob(filepath.Join(f.project, Folder, ".*.part"))
	if len(parts) != 1 {
		t.Fatalf("partial files: %v", parts)
	}
	// Stop without Close, as a crash would: the index names the partial file.
	f.s.stopped.Do(func() { close(f.s.stop); <-f.s.done })
	if err := os.Rename(movedPath, filepath.Join(f.project, "moved.png")); err != nil {
		t.Fatal(err)
	}
	f.clk.add(KeepFor - time.Hour)
	f.s = f.open()
	if _, err := os.Stat(parts[0]); err == nil {
		t.Error("the partial file survived a restart")
	}
	if !f.s.Known(keep) || !f.s.Known(old) || f.s.Known(moved) {
		t.Errorf("after restart: keep %v, old %v, moved %v known", f.s.Known(keep), f.s.Known(old), f.s.Known(moved))
	}
	f.clk.add(2 * time.Hour)
	f.s.Sweep()
	for _, p := range []string{keepPath, oldPath} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s outlived KeepFor", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.project, "moved.png")); err != nil {
		t.Errorf("a moved file was deleted: %v", err)
	}
	f.s.Close()
}

// TestPaths: session.send takes only done uploads of the same session, in order.
func TestPaths(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, ap := f.upload(t, "s1", "a.png", []byte("a"), 1)
	b, bp := f.upload(t, "s1", "b.png", []byte("b"), 1)
	other, _ := f.upload(t, "s2", "c.png", []byte("c"), 1)
	got, err := f.s.Paths("s1", []string{b, a})
	if err != nil || len(got) != 2 || got[0] != bp || got[1] != ap {
		t.Fatalf("paths %v, %v", got, err)
	}
	if _, err := f.s.Paths("s1", []string{other}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("another session's id: %v", err)
	}
	st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "x", Size: 2, SHA256: sum([]byte("xy"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Paths("s1", []string{st.ID}); err == nil || kindOf(t, err) != api.ErrorKindConflict {
		t.Errorf("an open upload: %v", err)
	}
	os.Remove(ap)
	if _, err := f.s.Paths("s1", []string{a}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("a removed file: %v", err)
	}
}

// TestMessageText: paths follow the text after a blank line, and SplitMessage takes them back
// off for the chat view; a path that is not an attachment stays in the text.
func TestMessageText(t *testing.T) {
	p1 := "/w/p/.flightdeck/attachments/0123456789ab-shot.png"
	p2 := "/w/p/.flightdeck/attachments/ba9876543210-log.txt"
	cases := []struct {
		text  string
		paths []string
		want  string
	}{
		{"look at this", []string{p1}, "look at this\n\n" + p1},
		{"two\n", []string{p1, p2}, "two\n\n" + p1 + "\n" + p2},
		{"", []string{p1}, p1},
		{"no files", nil, "no files"},
	}
	for _, c := range cases {
		got := MessageText(c.text, c.paths)
		if got != c.want {
			t.Errorf("MessageText(%q) = %q, want %q", c.text, got, c.want)
		}
		rest, ids := SplitMessage(got, nil)
		if want := strings.TrimRight(c.text, "\n"); rest != want || len(ids) != len(c.paths) {
			t.Errorf("SplitMessage(%q) = %q %v", got, rest, ids)
		}
	}
	rest, ids := SplitMessage("see\n\n"+p1+"\n"+p2+"\n", func(id string) bool { return id == "ba9876543210" })
	if rest != "see\n\n"+p1 || len(ids) != 1 || ids[0] != "ba9876543210" {
		t.Errorf("known filter: %q %v", rest, ids)
	}
	if rest, ids := SplitMessage("open /etc/hosts\n"+p1+" please", nil); ids != nil || rest != "open /etc/hosts\n"+p1+" please" {
		t.Errorf("not trailing paths: %q %v", rest, ids)
	}
	if rest, ids := SplitMessage("relative\n.flightdeck/attachments/0123456789ab-a.png", nil); ids != nil || !strings.HasPrefix(rest, "relative") {
		t.Errorf("a relative path was taken: %q %v", rest, ids)
	}
}

// TestSafeNameLong: long names with a long last "extension", multibyte runes at the cut and
// names of every length up to 400 bytes never panic, stay within MaxName bytes and stay valid
// UTF-8.
func TestSafeNameLong(t *testing.T) {
	names := []string{
		"Meeting notes v2.final draft for the quarterly planning review with all the teams and stakeholders here",
		"a." + strings.Repeat("b", 200),
		strings.Repeat("é", 120) + ".png",
		strings.Repeat("日本", 60) + ".jpeg",
		strings.Repeat("x", 99) + "é.txt",
		"a." + strings.Repeat("é", 200),
		strings.Repeat("x", 90) + "." + strings.Repeat("ü", 20),
	}
	for n := 0; n <= 400; n++ {
		names = append(names, strings.Repeat("a", n), "x."+strings.Repeat("é", n), strings.Repeat("ß", n)+".pdf")
	}
	for _, in := range names {
		checkSafeName(t, in)
	}
	if got := SafeName(strings.Repeat("a", 150) + ".png"); !strings.HasSuffix(got, ".png") || len(got) != MaxName {
		t.Errorf("a short extension is kept: %q", got)
	}
}

func checkSafeName(t *testing.T, in string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SafeName(%q) panicked: %v", in, r)
		}
	}()
	got := SafeName(in)
	if len(got) > MaxName || got == "" || !utf8.ValidString(got) || strings.ContainsAny(got, "/ \n") {
		t.Errorf("SafeName(%q) = %q (%d bytes)", in, got, len(got))
	}
}

func FuzzSafeName(f *testing.F) {
	for _, s := range []string{"a.png", "Meeting notes v2.final draft", "a." + strings.Repeat("é", 80), "../x", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) { checkSafeName(t, in) })
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"Screen Shot 2026-03-01 at 9.41.png": "Screen-Shot-2026-03-01-at-9.41.png",
		"../../etc/passwd":                   "passwd",
		".env":                               "env",
		"a;rm -rf $(x).txt":                  "a-rm-rf-x-.txt",
		"":                                   "file",
		"Ünïcode café.jpg":                   "Ünïcode-café.jpg",
		strings.Repeat("a", 300) + ".png":    strings.Repeat("a", MaxName-4) + ".png",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestThumbnail: a large PNG with transparency becomes a JPEG at most ThumbSide on its longest
// side, flattened over white; a non-image is refused by get with invalid.
func TestThumbnail(t *testing.T) {
	f := newFixture(t)
	img := image.NewNRGBA(image.Rect(0, 0, 1000, 500))
	for y := 0; y < 500; y++ {
		for x := 0; x < 1000; x++ {
			if x < 500 {
				img.Set(x, y, color.NRGBA{R: 255, A: 255})
			} // the right half stays transparent
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	id, _ := f.upload(t, "s1", "wide.png", buf.Bytes(), Chunk)
	d, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id, Thumbnail: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "image/jpeg" || !d.EOF || d.Size != int64(len(d.Data)) {
		t.Errorf("thumbnail header: %q eof %v size %d", d.Type, d.EOF, d.Size)
	}
	th, err := jpeg.Decode(bytes.NewReader(d.Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := th.Bounds(); b.Dx() != ThumbSide || b.Dy() != ThumbSide/2 {
		t.Errorf("thumbnail %v, want %dx%d", b, ThumbSide, ThumbSide/2)
	}
	r, g, b, _ := th.At(ThumbSide*3/4, ThumbSide/4).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent area is %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
	if _, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id, Thumbnail: true, Offset: 1}); err == nil || kindOf(t, err) != api.ErrorKindInvalid {
		t.Errorf("thumbnail with offset: %v", err)
	}
	txt, _ := f.upload(t, "s1", "a.txt", []byte("not an image"), Chunk)
	if _, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: txt, Thumbnail: true}); err == nil || kindOf(t, err) != api.ErrorKindInvalid {
		t.Errorf("thumbnail of text: %v", err)
	}
	if _, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: "000000000000"}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

// TestConcurrentWrites: two writes to one upload at once never both land; under -race.
func TestConcurrentWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b := bytes.Repeat([]byte("z"), 64)
	st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: "s1", Name: "z", Size: uint64(len(b)), SHA256: sum(b)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: st.ID, Offset: 0, Data: b[:32]}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d writes at offset 0 landed, want 1", ok)
	}
	w, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: st.ID, Offset: 32, Data: b[32:]})
	if err != nil || !w.Done {
		t.Fatalf("last run: %+v, %v", w, err)
	}
}
