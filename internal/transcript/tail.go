package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
)

// readChunk is how much one read takes from the file.
const readChunk = 1 << 20

// maxLine bounds one record. A longer line is skipped and reported, never held in memory whole.
const maxLine = 32 << 20

// line is one complete record line and the byte offset just past its newline.
type line struct {
	data []byte // nil when the line was longer than maxLine
	end  int64
}

// tail follows a JSONL file that another program appends to. It reads the file where it lies
// and never copies it. It hands out complete lines only: a last line with no newline yet (the
// writer is mid-append) waits until its newline arrives.
type tail struct {
	path    string
	info    fs.FileInfo // the file followed, to notice a replacement
	read    int64       // bytes read from the file so far
	done    int64       // offset just past the last complete line
	pending []byte      // bytes after the last newline
	skip    bool        // the current line passed maxLine: drop bytes until its newline
	gen     string      // hash of the first line: names this file's contents in cursors
}

// errReplaced reports that the file shrank or was replaced: offsets from before no longer apply.
var errReplaced = errors.New("transcript file was replaced or truncated")

// next reads what was appended since the last call and returns the complete lines.
// A missing file returns fs.ErrNotExist. errReplaced comes with no lines; the caller resets
// and calls next on a fresh tail.
func (t *tail) next() ([]line, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if t.info != nil && (!os.SameFile(t.info, info) || info.Size() < t.read) {
		return nil, errReplaced
	}
	t.info = info
	var out []line
	buf := make([]byte, readChunk)
	for t.read < info.Size() {
		want := min(int64(len(buf)), info.Size()-t.read)
		n, err := f.ReadAt(buf[:want], t.read)
		if n == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, err
		}
		chunk := buf[:n]
		base := t.read
		t.read += int64(n)
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				t.hold(chunk)
				break
			}
			end := base + int64(i) + 1
			t.done = end
			if t.skip {
				out = append(out, line{end: end})
				t.skip = false
			} else {
				data := append(t.pending, chunk[:i]...)
				t.pending = nil
				if t.gen == "" {
					sum := sha256.Sum256(data)
					t.gen = hex.EncodeToString(sum[:6])
				}
				out = append(out, line{data: data, end: end})
			}
			base = end
			chunk = chunk[i+1:]
		}
	}
	return out, nil
}

// hold keeps the bytes of an unfinished line, or starts skipping it when it grows past maxLine.
func (t *tail) hold(b []byte) {
	if t.skip {
		return
	}
	if len(t.pending)+len(b) > maxLine {
		t.pending, t.skip = nil, true
		return
	}
	t.pending = append(t.pending, b...)
}
