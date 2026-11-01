package transcript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
)

// readChunk is how much one read takes from the file.
const readChunk = 1 << 20

// headLen is how much of the file's start the tail keeps, to notice the file was rewritten.
const headLen = 4 << 10

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
	head    []byte      // the file's first bytes, up to headLen
}

// errReplaced reports that the file shrank or was replaced: offsets from before no longer apply.
var errReplaced = errors.New("transcript file was replaced or truncated")

// next reads what was appended since the last call and returns the complete lines.
// A missing file returns fs.ErrNotExist. errReplaced comes with no lines; the caller resets
// and calls next on a fresh tail. When ctx ends between two chunks, next returns the lines read
// so far and ctx's error; the next call goes on from there. A file that has not grown costs one
// open and one stat, and allocates no read buffer.
func (t *tail) next(ctx context.Context) ([]line, error) {
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
	if t.read >= info.Size() {
		t.info = info
		return nil, nil
	}
	if t.info != nil && !t.unchanged(f) {
		return nil, errReplaced // truncated and written again past where we read
	}
	t.info = info
	var out []line
	buf := make([]byte, min(readChunk, info.Size()-t.read))
	for t.read < info.Size() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
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
		if base < headLen && len(t.head) == int(base) {
			t.head = append(t.head, chunk[:min(int64(n), headLen-base)]...)
		}
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

// unchanged checks, when the file has grown, that the bytes read before are still there: the
// file starts with the same head, and the byte before the next line is still a newline. A file
// truncated and written again past the old size fails one of them, short of a rewrite that
// repeats the first 4 KiB and puts a newline at the same offset.
func (t *tail) unchanged(f *os.File) bool {
	if len(t.head) > 0 {
		b := make([]byte, len(t.head))
		if n, _ := f.ReadAt(b, 0); n != len(b) || !bytes.Equal(b, t.head) {
			return false
		}
	}
	if t.done > 0 {
		var b [1]byte
		if n, _ := f.ReadAt(b[:], t.done-1); n != 1 || b[0] != '\n' {
			return false
		}
	}
	return true
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
