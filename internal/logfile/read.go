package logfile

import (
	"bufio"
	"container/heap"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Source is one process's log file. Its old files (Path.1, Path.2, ...) are read first, oldest
// first.
type Source struct {
	// Kind is the process kind --proc matches: hab, holder, hook.
	Kind string
	// Name is the label printed in front of each line: the kind, and for a holder its instance.
	Name string
	Path string
	// Loc is the time zone of the file's time stamps. gff-hab writes UTC; a holder of an older
	// build writes local time. Nil: UTC.
	Loc *time.Location
}

// Entry is one log line, with the lines after it that carry no time stamp (a stack trace).
type Entry struct {
	T    time.Time // zero when no line before it in its file had a time
	Name string
	Kind string
	Text string // the message with prefix and time cut off; continuation lines after a newline
}

// Filter selects entries. Zero values select all.
type Filter struct {
	Since, Until time.Time
	Kinds        []string
	Grep         string
}

func (f Filter) kind(k string) bool { return len(f.Kinds) == 0 || slices.Contains(f.Kinds, k) }

func (f Filter) match(e Entry) bool {
	if !f.kind(e.Kind) {
		return false
	}
	if !f.Since.IsZero() && !e.T.IsZero() && e.T.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.T.IsZero() && e.T.After(f.Until) {
		return false
	}
	return f.Grep == "" || strings.Contains(e.Text, f.Grep)
}

// timeLayout is the time stamp of the standard log package's date and time flags.
const timeLayout = "2006/01/02 15:04:05"

// ParseLine splits a log line written by the standard log package with a prefix such as
// "gff-hab: " and the date and time flags (microseconds optional) into its time and message.
// ok is false for a line with no time stamp at its start.
func ParseLine(line string, loc *time.Location) (t time.Time, msg string, ok bool) {
	if loc == nil {
		loc = time.UTC
	}
	rest := line
	if i := strings.Index(line, ": "); i > 0 && i <= 40 && !strings.ContainsAny(line[:i], " \t") {
		rest = line[i+2:]
	}
	if len(rest) < len(timeLayout) {
		return time.Time{}, line, false
	}
	n := len(timeLayout)
	if len(rest) > n && rest[n] == '.' {
		n++
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
	}
	layout := timeLayout
	if n > len(timeLayout) {
		layout += "." + strings.Repeat("0", n-len(timeLayout)-1)
	}
	t, err := time.ParseInLocation(layout, rest[:n], loc)
	if err != nil {
		return time.Time{}, line, false
	}
	return t, strings.TrimPrefix(rest[n:], " "), true
}

// Files lists a source's files in reading order: its old files from the oldest, then Path.
func (s Source) Files() []string {
	olds, _ := filepath.Glob(escapeGlob(s.Path) + ".*")
	type old struct {
		n    int
		path string
	}
	var olds2 []old
	for _, o := range olds {
		n, err := strconv.Atoi(strings.TrimPrefix(o, s.Path+"."))
		if err == nil && n > 0 {
			olds2 = append(olds2, old{n, o})
		}
	}
	slices.SortFunc(olds2, func(a, b old) int { return b.n - a.n })
	out := make([]string, 0, len(olds2)+1)
	for _, o := range olds2 {
		out = append(out, o.path)
	}
	return append(out, s.Path)
}

func escapeGlob(p string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(p)
}

// reader yields one source's entries in file order.
type reader struct {
	src   Source
	files []string
	f     *os.File
	br    *bufio.Reader
	next  *Entry // the entry being gathered
	last  time.Time
	err   error
}

func newReader(s Source) *reader { return &reader{src: s, files: s.Files()} }

func (r *reader) line() (string, bool) {
	for {
		if r.br == nil {
			if len(r.files) == 0 {
				return "", false
			}
			f, err := os.Open(r.files[0])
			r.files = r.files[1:]
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					r.err = err
				}
				continue
			}
			r.f, r.br = f, bufio.NewReaderSize(f, 64<<10)
		}
		s, err := r.br.ReadString('\n')
		if s != "" {
			return strings.TrimRight(s, "\r\n"), true
		}
		if err != nil {
			if err != io.EOF {
				r.err = err
			}
			r.f.Close()
			r.f, r.br = nil, nil
		}
	}
}

// entry returns the next entry, or nil at the end.
func (r *reader) entry() *Entry {
	for {
		s, ok := r.line()
		if !ok {
			e := r.next
			r.next = nil
			return e
		}
		t, msg, timed := ParseLine(s, r.src.Loc)
		if !timed {
			if r.next == nil {
				r.next = &Entry{T: r.last, Name: r.src.Name, Kind: r.src.Kind, Text: s}
			} else {
				r.next.Text += "\n" + s
			}
			continue
		}
		r.last = t
		e := r.next
		r.next = &Entry{T: t, Name: r.src.Name, Kind: r.src.Kind, Text: msg}
		if e != nil {
			return e
		}
	}
}

// heapItem is one source's next entry in Merge; ord is the source's place, for equal times.
type heapItem struct {
	e   *Entry
	r   *reader
	ord int
}

type entryHeap []heapItem

func (h entryHeap) Len() int { return len(h) }
func (h entryHeap) Less(i, j int) bool {
	if !h[i].e.T.Equal(h[j].e.T) {
		return h[i].e.T.Before(h[j].e.T)
	}
	return h[i].ord < h[j].ord
}
func (h entryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any) {
	*h = append(*h, x.(heapItem))
}
func (h *entryHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Merge reads every source and calls out with each entry the filter selects, ordered by time.
// Entries of one source keep their file order. It never loads a file whole. It returns the
// first read error after it read what it could.
func Merge(sources []Source, f Filter, out func(Entry)) error {
	var h entryHeap
	var readers []*reader
	for i, s := range sources {
		if !f.kind(s.Kind) {
			continue
		}
		r := newReader(s)
		readers = append(readers, r)
		if e := r.entry(); e != nil {
			h = append(h, heapItem{e, r, i})
		}
	}
	heap.Init(&h)
	for h.Len() > 0 {
		top := h[0]
		if f.match(*top.e) {
			out(*top.e)
		}
		if e := top.r.entry(); e != nil {
			h[0].e = e
			heap.Fix(&h, 0)
		} else {
			heap.Pop(&h)
		}
	}
	for _, r := range readers {
		if r.err != nil {
			return r.err
		}
	}
	return nil
}

// followed is what Follow knows of one file.
type followed struct {
	info os.FileInfo
	off  int64
	last time.Time
}

// Follower prints the lines added to sources' current files after it was made.
type Follower struct {
	sources func() []Source
	f       Filter
	seen    map[string]*followed
}

// NewFollower notes where each source's current file ends now. sources is called again at
// each poll, so a new holder's file is picked up.
func NewFollower(sources func() []Source, f Filter) *Follower {
	fw := &Follower{sources: sources, f: f, seen: map[string]*followed{}}
	for _, s := range sources() {
		if st, err := os.Stat(s.Path); err == nil && f.kind(s.Kind) {
			fw.seen[s.Path] = &followed{info: st, off: st.Size()}
		}
	}
	return fw
}

// Run prints new lines every poll until ctx ends. A file that was renamed away, replaced or
// truncated is read again from its start; a file that appeared is read from its start. Each
// line is one entry; a line with no time stamp carries the time of the line before it.
func (fw *Follower) Run(ctx context.Context, poll time.Duration, out func(Entry)) {
	for {
		fw.poll(out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

func (fw *Follower) poll(out func(Entry)) {
	for _, s := range fw.sources() {
		if !fw.f.kind(s.Kind) {
			continue
		}
		st, err := os.Stat(s.Path)
		if err != nil {
			delete(fw.seen, s.Path)
			continue
		}
		fl := fw.seen[s.Path]
		switch {
		case fl == nil:
			fl = &followed{info: st}
			fw.seen[s.Path] = fl
		case !os.SameFile(fl.info, st) || st.Size() < fl.off:
			fl.off = 0
		}
		fl.info = st
		if st.Size() > fl.off {
			fl.off = readFrom(s, fl, st.Size(), fw.f, out)
		}
	}
}

// readFrom prints the whole lines between fl.off and size and returns the offset after the
// last one.
func readFrom(s Source, fl *followed, size int64, f Filter, out func(Entry)) int64 {
	file, err := os.Open(s.Path)
	if err != nil {
		return fl.off
	}
	defer file.Close()
	b := make([]byte, size-fl.off)
	n, _ := file.ReadAt(b, fl.off)
	b = b[:n]
	end := strings.LastIndexByte(string(b), '\n')
	if end < 0 {
		return fl.off
	}
	for _, line := range strings.Split(string(b[:end]), "\n") {
		t, msg, ok := ParseLine(line, s.Loc)
		if ok {
			fl.last = t
		} else {
			t, msg = fl.last, line
		}
		e := Entry{T: t, Name: s.Name, Kind: s.Kind, Text: msg}
		if f.match(e) {
			out(e)
		}
	}
	return fl.off + int64(end) + 1
}
