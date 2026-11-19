// Package logfile writes, caps and reads the text log files of docs/specs/diagnostics.md 4.3:
// a buffered writer that rotates its own file at a size cap, copy-truncate for a file another
// process writes, and a reader that merges several files by time (4.8).
package logfile

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultQueue is how many lines wait for the file before Write drops them (4.3).
const DefaultQueue = 1000

// checkEvery and checkLines bound how long the writer goes without comparing its open file with
// the path: a removed or replaced file is reopened at the next check (4.3, the inode check).
const (
	checkEvery = 60 * time.Second
	checkLines = 1000
)

// Config sets up a Writer.
type Config struct {
	// Max is the size at which the file is renamed to .1 and a new one started. Zero: no cap.
	Max int64
	// Keep is how many renamed files stay: .1 to .Keep.
	Keep int
	// Queue is how many lines may wait for the file. Default DefaultQueue.
	Queue int
	// Tee, if set, gets a copy of each line at once, from Write. It is how a process run by hand
	// keeps its log on the terminal too.
	Tee io.Writer
	// Early, if set and Tee is not, gets each line written before Open, at once. Lines that a
	// process writes before it may open its file (it waits on a lock, or fails first) are then
	// not lost.
	Early io.Writer
	// Dropped formats the line that reports n lines dropped (queue full, or a write that
	// failed). It is written before the next line that reaches the file. Default: a plain line.
	Dropped func(n int64) []byte
	// OnFlush is called on the writer's goroutine after it wrote lines to the file and the
	// queue is empty.
	OnFlush func()
	// Now is the clock for the inode check. Default time.Now.
	Now func() time.Time
}

// Writer is an io.Writer for a log.Logger. Write never blocks on the disk: it queues the line
// and returns. One goroutine writes the queue to the file, rotates it at Max and reopens it
// when the path no longer names the open file. Until Open, lines wait in the queue.
type Writer struct {
	cfg Config

	mu      sync.Mutex // guards closed and every send on ch
	closed  bool
	opened  atomic.Bool
	ch      chan []byte
	flush   chan chan struct{}
	open    chan string
	quit    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
}

// New starts a writer. It writes nothing until Open names its file.
func New(cfg Config) *Writer {
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Dropped == nil {
		cfg.Dropped = func(n int64) []byte { return []byte("logfile: " + itoa(n) + " lines dropped\n") }
	}
	w := &Writer{cfg: cfg, ch: make(chan []byte, cfg.Queue), flush: make(chan chan struct{}), open: make(chan string, 1),
		quit: make(chan struct{}), done: make(chan struct{})}
	go w.loop()
	return w
}

// Open starts writing to path, after the lines already queued. Its folder is made with mode
// 0700 when missing; the file is opened for append with mode 0600. Only the first call counts.
func (w *Writer) Open(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := openFile(path)
	if err != nil {
		return err
	}
	f.Close()
	if w.opened.CompareAndSwap(false, true) {
		w.open <- path
	}
	return nil
}

// Write queues a copy of p, one log line. It drops the line when the queue is full, and counts
// it. It never fails.
func (w *Writer) Write(p []byte) (int, error) {
	if w.cfg.Tee != nil {
		w.cfg.Tee.Write(p)
	} else if w.cfg.Early != nil && !w.opened.Load() {
		w.cfg.Early.Write(p)
	}
	b := append([]byte(nil), p...)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	select {
	case w.ch <- b:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// Dropped is how many lines were dropped and not reported in the file yet.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Flush waits until every line queued before it is in the file. Before Open it returns at once.
func (w *Writer) Flush() {
	if !w.opened.Load() {
		return
	}
	ack := make(chan struct{})
	select {
	case w.flush <- ack:
	case <-w.done:
		return
	}
	select {
	case <-ack:
	case <-w.done:
	}
}

// FlushTimeout is Flush that gives up after d. It reports whether the queue was written.
func (w *Writer) FlushTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() { w.Flush(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Close writes what is queued, when the file is open, and closes it. Later writes go to Tee
// only.
func (w *Writer) Close() error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
		close(w.quit)
	}
	w.mu.Unlock()
	<-w.done
	return nil
}

func (w *Writer) loop() {
	defer close(w.done)
	var path string
	select {
	case path = <-w.open:
	case <-w.quit:
		select {
		case path = <-w.open: // opened and closed at once: still write the queue
		default:
			return
		}
	}
	f := &file{path: path, max: w.cfg.Max, keep: w.cfg.Keep, now: w.cfg.Now}
	defer f.close()
	for {
		select {
		case b, ok := <-w.ch:
			if !ok {
				return
			}
			w.put(f, b)
			if !w.drain(f) {
				return
			}
		case ack := <-w.flush:
			open := w.drain(f)
			close(ack)
			if !open {
				return
			}
		}
		if w.cfg.OnFlush != nil {
			w.cfg.OnFlush()
		}
	}
}

// drain writes every queued line. It reports false once the queue is closed.
func (w *Writer) drain(f *file) bool {
	for {
		select {
		case b, ok := <-w.ch:
			if !ok {
				return false
			}
			w.put(f, b)
		default:
			return true
		}
	}
}

// put writes one line, after the count of lines dropped before it.
func (w *Writer) put(f *file, b []byte) {
	if n := w.dropped.Swap(0); n > 0 {
		if f.write(w.cfg.Dropped(n)) != nil {
			w.dropped.Add(n)
		}
	}
	if f.write(b) != nil {
		w.dropped.Add(1) // a full disk drops lines the same way (4.3)
	}
}

// file is the open log file on the writer's goroutine.
type file struct {
	path      string
	max       int64
	keep      int
	now       func() time.Time
	f         *os.File
	size      int64
	lines     int
	checkedAt time.Time
}

func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}

func (f *file) close() {
	if f.f != nil {
		f.f.Close()
		f.f = nil
	}
}

// reopen opens the path again, making its folder when it was removed.
func (f *file) reopen() error {
	f.close()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	nf, err := openFile(f.path)
	if err != nil {
		return err
	}
	st, err := nf.Stat()
	if err != nil {
		nf.Close()
		return err
	}
	f.f, f.size, f.lines, f.checkedAt = nf, st.Size(), 0, f.now()
	return nil
}

// check reopens the file when the path is gone or names another file.
func (f *file) check() {
	f.lines, f.checkedAt = 0, f.now()
	open, err1 := f.f.Stat()
	at, err2 := os.Stat(f.path)
	if err1 != nil || err2 != nil || !os.SameFile(open, at) {
		f.reopen()
		return
	}
	f.size = open.Size() // another process may have truncated or written it
}

func (f *file) write(b []byte) error {
	if f.f == nil {
		if err := f.reopen(); err != nil {
			return err
		}
	} else if f.lines >= checkLines || f.now().Sub(f.checkedAt) >= checkEvery {
		f.check()
		if f.f == nil {
			return os.ErrClosed
		}
	}
	if f.max > 0 && f.size > 0 && f.size+int64(len(b)) > f.max {
		f.close()
		Shift(f.path, f.keep)
		if err := f.reopen(); err != nil {
			return err
		}
	}
	n, err := f.f.Write(b)
	f.size += int64(n)
	f.lines++
	return err
}

// Shift renames path to path.1, path.1 to path.2 and so on, keeping keep old files; the oldest
// beyond keep is removed. With keep 0, path is removed.
func Shift(path string, keep int) {
	os.Remove(Old(path, keep+1))
	if keep <= 0 {
		os.Remove(path)
		return
	}
	for k := keep; k >= 1; k-- {
		from := path
		if k > 1 {
			from = Old(path, k-1)
		}
		os.Rename(from, Old(path, k))
	}
}

// Old is the name of path's kth old file: path.k.
func Old(path string, k int) string { return path + "." + itoa(int64(k)) }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
