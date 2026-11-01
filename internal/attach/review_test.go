package attach

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestRetryReplacesItsUpload: a put of the same file for the same session drops the earlier
// open upload and its partial file, so retries never fill the open limit; another session's
// upload of the same file stays.
func TestRetryReplacesItsUpload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b := []byte("photo bytes")
	put := func(session string) api.AttachmentStored {
		st, err := f.s.Put(ctx, api.AttachmentPutParams{Session: session, Name: "p.jpg", Size: uint64(len(b)), SHA256: sum(b)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	first := put("s1")
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: first.ID, Data: b[:3]}); err != nil {
		t.Fatal(err)
	}
	other := put("s2")
	for i := 0; i < MaxOpen*2; i++ {
		put("s1")
	}
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: first.ID, Offset: 3, Data: b[3:]}); err == nil || kindOf(t, err) != api.ErrorKindNotFound {
		t.Errorf("the replaced upload still takes writes: %v", err)
	}
	if _, err := f.s.Write(ctx, api.AttachmentWriteParams{ID: other.ID, Data: b}); err != nil {
		t.Errorf("another session's upload was dropped: %v", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(f.project, Folder, ".*.part")); len(parts) != 1 {
		t.Errorf("partial files: %v, want the one open upload", parts)
	}
}

// TestLinkedFolderFallsBack: a .flightdeck or attachments folder that is a link out of the
// session folder is not followed; the file goes to the fallback folder.
func TestLinkedFolderFallsBack(t *testing.T) {
	for _, link := range []string{".flightdeck", Folder} {
		f := newFixture(t)
		elsewhere := t.TempDir()
		target := filepath.Join(f.project, link)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, target); err != nil {
			t.Fatal(err)
		}
		id, path := f.upload(t, "s1", "a.png", []byte("x"), 1)
		if want := filepath.Join(f.state, "attachments", "s1", Folder, id+"-a.png"); path != want {
			t.Errorf("link %s: path %s, want %s", link, path, want)
		}
		var files []string
		filepath.Walk(elsewhere, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() && strings.Contains(p, id) {
				files = append(files, p)
			}
			return nil
		})
		if len(files) != 0 {
			t.Errorf("link %s: the file went through the link: %v", link, files)
		}
		if link == ".flightdeck" {
			if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
				t.Errorf("link %s: folders were made through the link: %v", link, entries)
			}
		}
	}
}

// TestExpiryKeepsRecordsOnAFailingDisk: a record whose file cannot be checked (a folder the
// hab cannot read now) is kept; only a file that is gone is forgotten.
func TestExpiryKeepsRecordsOnAFailingDisk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	f := newFixture(t)
	id, _ := f.upload(t, "s1", "a.png", []byte("x"), 1)
	dir := filepath.Join(f.project, Folder)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	f.s.Sweep()
	if !f.s.Known(id) {
		t.Error("a record was forgotten while its folder could not be read")
	}
}

// TestThumbnailCached: a second thumbnail of the same id comes from the cache.
func TestThumbnailCached(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	id, path := f.upload(t, "s1", "a.png", buf.Bytes(), Chunk)
	a, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id, Thumbnail: true})
	if err != nil {
		t.Fatal(err)
	}
	// Overwritten with bytes that are no image: a fresh decode would fail.
	if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := f.s.Get(context.Background(), api.AttachmentGetParams{ID: id, Thumbnail: true})
	if err != nil || !bytes.Equal(a.Data, b.Data) {
		t.Errorf("second thumbnail: %v", err)
	}
}

// TestMatchSends: a transcript message whose harness turned the paths into images matches its
// send by text without markers, the nearest in time within MatchWindow, and gives the ids and
// the text without markers; other sessions, other text and old sends do not match. Sends
// survive a restart.
func TestMatchSends(t *testing.T) {
	f := newFixture(t)
	f.s.NoteSent("s1", "look at this", []string{"0123456789ab", "ba9876543210"})
	f.clk.add(time.Minute)
	f.s.NoteSent("s1", "", []string{"aaaaaaaaaaaa"})
	at := f.clk.now()
	cases := []struct {
		name, session, text string
		at                  time.Time
		ids                 []string
		clean               string
	}{
		{"claude", "s1", "look at this\n\n[Image #1]\n\n[image]\n\n[image]", at, []string{"0123456789ab", "ba9876543210"}, "look at this"},
		{"codex", "s1", "[image 800x600 PNG][image 10x10 JPEG]look at this", at, []string{"0123456789ab", "ba9876543210"}, "look at this"},
		{"paths left", "s1", "look at this\n\n/w/p/.flightdeck/attachments/0123456789ab-a.png", at, []string{"0123456789ab", "ba9876543210"}, "look at this"},
		{"images alone", "s1", "[Image #1]\n\n[image]", at, []string{"aaaaaaaaaaaa"}, ""},
		{"other session", "s2", "look at this [image]", at, nil, ""},
		{"other text", "s1", "something else [image]", at, nil, ""},
		{"too late", "s1", "look at this [image]", at.Add(MatchWindow + 2*time.Minute), nil, ""},
	}
	for _, c := range cases {
		clean, ids, ok := f.s.Match(c.session, c.text, c.at)
		if ok != (c.ids != nil) || (ok && (clean != c.clean || strings.Join(ids, ",") != strings.Join(c.ids, ","))) {
			t.Errorf("%s: %q %v %v", c.name, clean, ids, ok)
		}
	}
	f.s.Close()
	f.s = f.open()
	if _, ids, ok := f.s.Match("s1", "look at this [Image #1]", at); !ok || len(ids) != 2 {
		t.Errorf("sends lost at restart: %v %v", ids, ok)
	}
	if !HasMarker("x [Image #2]") || !HasMarker("[local_image: /a.png]") || HasMarker("[images are fine]") {
		t.Error("HasMarker")
	}
}
