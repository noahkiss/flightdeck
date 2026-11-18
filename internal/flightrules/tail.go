package flightrules

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
)

// maxLine bounds one record line. A longer line is skipped with a warning.
var maxLine = 32 << 20

// tail reads a JSONL file in place, from the offset the last read ended.
type tail struct {
	path string
	what string      // "transcript", "rollout": the file's name in warnings
	info fs.FileInfo // the file read so far, to see a replacement
	off  int64       // bytes read: the end of the last complete line
}

// read feeds each complete line added since the last call to fn. A file that does not exist yet
// reads as nothing. A file that was replaced or cut resets l, calls reset when it is not nil,
// and reads again from the start.
func (t *tail) read(ctx context.Context, l *Ledger, reset func(), fn func(line []byte)) error {
	f, err := os.Open(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if t.info != nil && (!os.SameFile(t.info, info) || info.Size() < t.off) {
		l.Reset()
		if reset != nil {
			reset()
		}
		t.off = 0
		l.Warn("replaced", "the %s was replaced or cut; the ledger was read again from the start", t.what)
	}
	t.info = info
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return err
	}
	n, err := scanLines(ctx, f, t.off, func(line []byte, end int64) {
		if line == nil {
			l.Warn("oversize:"+strconv.FormatInt(end, 10), "a %s line ending at byte %d is longer than %d bytes and was skipped", t.what, end, maxLine)
			return
		}
		fn(line)
	})
	t.off = n
	return err
}

// scanLines reads complete lines from r, which starts at offset from, and calls fn with each
// line and the offset just past its newline; fn gets nil for a line over maxLine. It returns
// the offset after the last complete line: a partial last line is left for the next read.
func scanLines(ctx context.Context, r io.Reader, from int64, fn func(line []byte, end int64)) (int64, error) {
	br := bufio.NewReaderSize(r, 256<<10)
	off := from
	var buf []byte
	var n int64 // bytes of the current line so far
	over := false
	for {
		if err := ctx.Err(); err != nil {
			return off, err
		}
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !over {
			if len(buf)+len(chunk) > maxLine {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == io.EOF {
			return off, nil // a partial last line waits for the next read
		}
		if err != nil {
			return off, err
		}
		off += n
		if over {
			fn(nil, off)
		} else {
			fn(buf, off)
		}
		buf, n, over = buf[:0], 0, false
	}
}
