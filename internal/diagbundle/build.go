package diagbundle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/diaglog"
	"github.com/noahkiss/flightdeck/internal/friction"
	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/logfile"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/tallies"
	"github.com/noahkiss/flightdeck/internal/trace"
)

// Format is the bundle format version the manifest names.
const Format = 1

// ManifestFile is the manifest's name in a bundle.
const ManifestFile = "manifest.json"

// Kind says how a file the command made enters the bundle.
type Kind int

const (
	// KindText is text: pass 2 on each line.
	KindText Kind = iota
	// KindJSON is a JSON value: pass 2 on each string.
	KindJSON
	// KindKept is kept as it is: versions.json, which holds version words only.
	KindKept
)

// Extra is a file the command made: status.txt, service.txt, detect.json, versions.json.
type Extra struct {
	Path string
	Data []byte
	Kind Kind
}

// LeftOut is an item of 8.2 the bundle does not hold, and why.
type LeftOut struct {
	Item string `json:"item"`
	Why  string `json:"why"`
}

// Options says what Write gathers.
type Options struct {
	// State is gff-hab's state folder.
	State string
	// Since starts the window: log lines, crash files, health records, tally days and friction
	// notes before it are left out. Until is the time the manifest gives as the window's end.
	Since, Until time.Time
	// NoFriction leaves the friction notes out.
	NoFriction bool
	// HabConfig and Peers are the config files. Empty or missing: left out.
	HabConfig, Peers string
	// Extras are the files the command made.
	Extras []Extra
	// LeftOut are the items the command could not make.
	LeftOut []LeftOut
}

// FileEntry is one file of the bundle.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Manifest is manifest.json (8.2, 8.3): the format, the time, the window, each file and its
// size, and the replacements per rule, never the text replaced.
type Manifest struct {
	Format       int            `json:"format"`
	Created      time.Time      `json:"created"`
	Since        time.Time      `json:"since"`
	Until        time.Time      `json:"until"`
	Files        []FileEntry    `json:"files"`
	Replacements map[string]int `json:"replacements"`
	// Unparsed counts the lines that do not parse, such as a line a crash cut short: they are
	// left out (12).
	Unparsed int `json:"unparsed_lines"`
	// Long counts the lines over MaxLine: they are left out, never read whole (11).
	Long    int       `json:"long_lines"`
	LeftOut []LeftOut `json:"left_out,omitempty"`
}

// MaxLine is the longest line a bundle reads from a file. A longer one, such as a file a power
// loss filled with NUL bytes, is read to its end in pieces and left out.
const MaxLine = 64 << 10

// readLine reads one line of at most MaxLine bytes from br, a reader with a MaxLine buffer.
// long is true for a longer line: it is skipped to its end and its text is not returned.
func readLine(br *bufio.Reader) (line string, long bool, err error) {
	b, err := br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return string(b), false, err
	}
	for errors.Is(err, bufio.ErrBufferFull) {
		_, err = br.ReadSlice('\n')
	}
	return "", true, err
}

// logFileRE matches the log files a bundle reads (8.2): hab.log, each holder's file, hook.log
// and capcom.log, with their old files. hab.stderr is not one: it holds a text copy of lines
// hab.log holds by key, with no classes (diagnostics review L4), and a crash's traceback, which
// the crash file holds too.
var logFileRE = regexp.MustCompile(`^(hab\.log|hook\.log|capcom\.log|gff-hab-holder\.[A-Za-z0-9_-]+\.log)(\.[0-9]+)?$`)

// Write gathers the bundle into dir, an empty folder, and writes its manifest. It streams each
// file: no log file is read whole (11).
func Write(dir string, r *Redactor, o Options) (Manifest, error) {
	b := &builder{dir: dir, r: r, o: o}
	steps := []func() error{b.extras, b.logs, b.crash, b.health, b.tallies, b.config}
	if !o.NoFriction {
		steps = append(steps, b.friction)
	}
	for _, f := range steps {
		if err := f(); err != nil {
			return Manifest{}, err
		}
	}
	m := Manifest{Format: Format, Created: o.Until.UTC(), Since: o.Since.UTC(), Until: o.Until.UTC(), Files: b.files,
		Replacements: r.Counts(), Unparsed: b.unparsed, Long: b.long, LeftOut: append(o.LeftOut, b.leftOut...)}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	data, err := marshal(m, true)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeFile(filepath.Join(dir, ManifestFile), append(data, '\n')); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

type builder struct {
	dir      string
	r        *Redactor
	o        Options
	files    []FileEntry
	unparsed int
	long     int
	leftOut  []LeftOut
}

func (b *builder) state(parts ...string) string {
	return filepath.Join(append([]string{b.o.State}, parts...)...)
}

// in reports whether t is in the window. The window has no end: a line written while the bundle
// is made is kept.
func (b *builder) in(t time.Time) bool { return !t.Before(b.o.Since) }

// create opens a bundle file for writing; done records its size, or removes it when it is
// empty.
func (b *builder) create(rel string) (*os.File, func() error, error) {
	p := filepath.Join(b.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, err
	}
	done := func() error {
		st, err := f.Stat()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if st.Size() == 0 {
			return os.Remove(p)
		}
		b.files = append(b.files, FileEntry{Path: rel, Size: st.Size()})
		return nil
	}
	return f, done, nil
}

func (b *builder) put(rel string, data []byte) error {
	f, done, err := b.create(rel)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return done()
}

func (b *builder) extras() error {
	for _, e := range b.o.Extras {
		var data []byte
		switch e.Kind {
		case KindKept:
			data = e.Data
		case KindJSON:
			var v any
			dec := json.NewDecoder(bytes.NewReader(e.Data))
			dec.UseNumber() // a pid or a size stays as written
			if err := dec.Decode(&v); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			out, err := marshal(b.r.JSON(v), true)
			if err != nil {
				return err
			}
			data = append(out, '\n')
		default:
			var sb strings.Builder
			sc := b.r.Scrubber()
			for line := range strings.Lines(string(e.Data)) {
				if s, ok := sc.Line(strings.TrimRight(line, "\n")); ok {
					sb.WriteString(s + "\n")
				}
			}
			data = []byte(sb.String())
		}
		if err := b.put(e.Path, data); err != nil {
			return err
		}
	}
	return nil
}

// logs copies each log file's lines in the window: a structured line by both passes, a text
// line by pass 2. A line with no time stamp, such as a stack trace, goes with the line before.
func (b *builder) logs() error {
	entries, err := os.ReadDir(b.state("logs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !logFileRE.MatchString(e.Name()) {
			continue
		}
		loc := time.UTC
		if strings.HasPrefix(e.Name(), "gff-hab-holder.") {
			loc = time.Local // the holder writes local time until its next build (4.8)
		}
		if err := b.logFile(e.Name(), loc); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) logFile(name string, loc *time.Location) error {
	src, err := os.Open(b.state("logs", name))
	if err != nil {
		return nil // gone since the listing: rotated
	}
	defer src.Close()
	if st, err := src.Stat(); err != nil || st.ModTime().Before(b.o.Since) {
		return err
	}
	f, done, err := b.create("logs/" + b.r.Scrub(name)) // a holder's file is named for its instance
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	sc := b.r.Scrubber()
	keep := true // a line before the first time stamp: its time is unknown, so kept
	br := bufio.NewReaderSize(src, MaxLine)
	for {
		line, long, rerr := readLine(br)
		if long {
			b.long++
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if strings.HasPrefix(strings.TrimSpace(line), "{") {
				if l, ok := diaglog.Parse(line); ok {
					keep = b.in(l.T)
					if keep {
						out, _ := b.r.Line(line)
						w.WriteString(out + "\n")
					}
				} else {
					b.unparsed++
				}
			} else {
				if t, _, ok := logfile.ParseLine(line, loc); ok {
					keep = b.in(t)
				}
				if s, ok := sc.Line(line); ok && keep {
					w.WriteString(s + "\n")
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return done()
}

// crash copies the crash files and stack dumps written in the window, by pass 2 (8.2).
func (b *builder) crash() error {
	entries, err := os.ReadDir(b.state("crash"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !b.in(info.ModTime()) {
			continue
		}
		if err := b.textFile(b.state("crash", e.Name()), "crash/"+b.r.Scrub(e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) textFile(path, rel string) error {
	src, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer src.Close()
	f, done, err := b.create(rel)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	sc := b.r.Scrubber()
	br := bufio.NewReaderSize(src, MaxLine)
	for {
		line, long, rerr := readLine(br)
		if long {
			b.long++
		}
		if line != "" {
			if s, ok := sc.Line(strings.TrimRight(line, "\r\n")); ok {
				w.WriteString(s + "\n")
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return done()
}

// health copies the health records of the window (6.3). Their fields are fixed; the cause is
// text, and every string gets pass 2.
func (b *builder) health() error {
	src, err := os.Open(b.state("health.jsonl"))
	if err != nil {
		return nil
	}
	defer src.Close()
	f, done, err := b.create("health.jsonl")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	br := bufio.NewReaderSize(src, MaxLine)
	for {
		line, long, rerr := readLine(br)
		if long {
			b.long++
		}
		if line = strings.TrimSpace(line); line != "" {
			var rec health.Record
			if json.Unmarshal([]byte(line), &rec) != nil {
				b.unparsed++
			} else if b.in(rec.T) {
				s := b.r.Scrub
				rec.Proc, rec.Instance, rec.Kind, rec.Cause = s(rec.Proc), s(rec.Instance), s(rec.Kind), s(rec.Cause)
				rec.Run, rec.Build, rec.Version, rec.CrashFile = s(rec.Run), s(rec.Build), s(rec.Version), s(rec.CrashFile)
				out, _ := marshal(rec, false)
				w.Write(append(out, '\n'))
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return done()
}

var dayFileRE = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})\.json$`)

// tallies copies the day files of the window and seen.json. The writer holds them to names from
// closed sets and numbers (7.3); the bundle does not trust the file, so every key and string
// still gets pass 2 (diagnostics review L6). A file that does not parse is left out and counted.
func (b *builder) tallies() error {
	dir := b.state(tallies.DirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	first := b.o.Since.Local().Format("2006-01-02")
	for _, e := range entries {
		m := dayFileRE.FindStringSubmatch(e.Name())
		if e.Name() != tallies.SeenFile && (m == nil || m[1] < first) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			b.unparsed++
			continue
		}
		out, err := marshal(b.r.JSON(v), false)
		if err != nil {
			return err
		}
		if err := b.put("tallies/"+e.Name(), append(out, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// friction copies the notes of the window: the text by pass 2, each tail line as a log line.
func (b *builder) friction() error {
	notes, bad, err := friction.List(b.state(friction.Dir), b.o.Since)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b.unparsed += bad
	s := b.r.Scrub
	for i, n := range notes {
		// The file name takes the trace only in its own form: a note's JSON could hold
		// "../x" (diagnostics review L9).
		id := n.Trace
		if !trace.Valid(id) {
			id = fmt.Sprintf("note%d", i+1)
		}
		n.Trace, n.Client, n.Session, n.Pane = s(n.Trace), s(n.Client), s(n.Session), s(n.Pane)
		n.Text, n.Build, n.Version = s(n.Text), s(n.Build), s(n.Version)
		for i := range n.Log {
			l := &n.Log[i]
			l.Proc = s(l.Proc)
			if out, ok := b.r.Line(l.Text); ok {
				l.Text = out
			} else if strings.HasPrefix(strings.TrimSpace(l.Text), "{") {
				l.Text = ""
				b.unparsed++
			} else {
				l.Text = s(l.Text)
			}
		}
		data, err := marshal(n, true)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("friction/%s-%s.json", n.T.UTC().Format("20060102T150405.000Z"), id)
		if err := b.put(name, append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// config copies the config files with their keys and no values, except a [settings] entry whose
// setting holds an enum, a number or a Boolean (8.3).
func (b *builder) config() error {
	for _, c := range []struct{ path, rel string }{{b.o.HabConfig, "config/hab.toml"}, {b.o.Peers, "config/peers.toml"}} {
		if c.path == "" {
			continue
		}
		data, err := os.ReadFile(c.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var tree map[string]any
		if _, err := toml.Decode(string(data), &tree); err != nil {
			b.leftOut = append(b.leftOut, LeftOut{Item: c.rel, Why: "the file does not parse as TOML"})
			continue
		}
		var out strings.Builder
		if err := toml.NewEncoder(&out).Encode(b.configTree(tree, "")); err != nil {
			return err
		}
		if err := b.put(c.rel, []byte(out.String())); err != nil {
			return err
		}
	}
	return nil
}

// configTree is a config table with each value replaced, unless the allowlist keeps it. A key
// stays only when the schema owns it (schemaKey); every other key, such as a round's, an alias's
// or a repository's, becomes its name token (diagnostics review M2).
func (b *builder) configTree(t map[string]any, table string) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		key := k
		if !schemaKey(k) {
			key = b.r.Name(k)
		}
		switch x := v.(type) {
		case map[string]any:
			out[key] = b.configTree(x, k)
		case []map[string]any:
			list := make([]map[string]any, len(x))
			for i := range x {
				list[i] = b.configTree(x[i], k)
			}
			out[key] = list
		default:
			if d, ok := settings.Lookup(k); ok && table == "settings" && d.Plain() {
				if _, err := d.CheckGo(v); err == nil {
					out[key] = v
					continue
				}
			}
			out[key] = "<value>"
		}
	}
	return out
}

func writeFile(p string, data []byte) error {
	return os.WriteFile(p, data, 0o600)
}

// Archive writes the bundle folder dir as a .tar.gz at dst, its files under the folder top. It
// writes a temporary file beside dst and renames it, so dst is never half written.
func Archive(dst, dir, top string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".flightdeck-diagnostics-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(filepath.Join(top, rel))
		if e.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	for _, c := range []io.Closer{tw, gz} {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
