package content

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/gitstatus"
)

// Config is what New needs. Session and Store are required.
type Config struct {
	Store *Store
	// Session looks up a session record.
	Session func(id string) (api.Session, bool)
	// PaneSession finds the session of the pane a GFF_HAB token names. nil: pane_token is refused.
	PaneSession func(token string) (session string, ok bool)
	// Conversation returns the session's chat messages and the folder its transcript records.
	// A session with no transcript yet returns nil messages and no error.
	Conversation func(ctx context.Context, session string) (msgs []api.ChatMessage, cwd string, err error)
	// ProjectDir is the most recently used folder of the session's project, or "".
	ProjectDir func(session string) string
	Git        gitstatus.Reader
	// Publish sends a content.updated event.
	Publish func(api.ContentItem)
	Now     func() time.Time
	Logf    func(string, ...any)
	// GitTimeout bounds one git status or git diff. Default 2 s.
	GitTimeout time.Duration
	// MaxItems bounds one list. Default MaxItems.
	MaxItems int
	// CacheFor is how long a session's list answers content.read and content.diff before it is
	// built again. Default 5 s.
	CacheFor time.Duration
}

// Service serves content.show, content.list, content.read and content.diff.
type Service struct {
	cfg Config

	mu    sync.Mutex
	cache map[string]cached // by session
}

type cached struct {
	at   time.Time
	list built
}

// built is a session's content as content.list built it, with what read and diff need.
type built struct {
	items   []api.ContentItem
	more    bool
	folder  string
	top     string                      // the repository's top folder, or ""
	realTop string                      // top with its symlinks resolved
	shown   map[string]bool             // paths with a stored reference
	changes map[string]gitstatus.Change // by absolute path
	touches map[string]Touch            // by absolute path
	byID    map[string]int              // index into items
}

// New returns a service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.GitTimeout <= 0 {
		cfg.GitTimeout = 2 * time.Second
	}
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = MaxItems
	}
	if cfg.CacheFor <= 0 {
		cfg.CacheFor = 5 * time.Second
	}
	cfg.Store.SetLogf(cfg.Logf)
	return &Service{cfg: cfg, cache: map[string]cached{}}
}

// Close saves any reference change not yet written.
func (s *Service) Close() error { return s.cfg.Store.Close() }

func errf(kind api.ErrorKind, format string, args ...any) error {
	return control.Errorf(kind, format, args...)
}

// Show stores a reference to a regular file for the session and sends content.updated.
func (s *Service) Show(ctx context.Context, p api.ContentShowParams) (api.ContentShown, error) {
	session := p.Session
	switch {
	case session != "" && p.PaneToken != "":
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "content.show takes session or pane_token, not both")
	case session == "" && p.PaneToken == "":
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "content.show needs a session or a pane_token")
	case session == "":
		var ok bool
		if s.cfg.PaneSession != nil {
			session, ok = s.cfg.PaneSession(p.PaneToken)
		}
		if !ok {
			return api.ContentShown{}, errf(api.ErrorKindNotFound, "the pane token names no pane with a session")
		}
	}
	if _, ok := s.cfg.Session(session); !ok {
		return api.ContentShown{}, errf(api.ErrorKindNotFound, "no session %s", session)
	}
	if p.Path == "" || !filepath.IsAbs(p.Path) || strings.ContainsRune(p.Path, 0) {
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "path %q is not an absolute path", p.Path)
	}
	if p.Kind != "" && !isKind(p.Kind) {
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "kind %q is not one of %v", p.Kind, api.Enums["ContentKind"])
	}
	if utf8.RuneCountInString(p.Title) > MaxTitle {
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "title is longer than %d characters", MaxTitle)
	}
	// Resolved, so a show through a symlinked folder (macOS /tmp, /var) and a git status entry
	// for the same file make one item.
	path, err := filepath.EvalSymlinks(filepath.Clean(p.Path))
	if err != nil {
		return api.ContentShown{}, errf(api.ErrorKindNotFound, "%s: no such file", p.Path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return api.ContentShown{}, errf(api.ErrorKindNotFound, "%s: no such file", path)
	}
	if !fi.Mode().IsRegular() {
		return api.ContentShown{}, errf(api.ErrorKindInvalid, "%s is not a regular file", path)
	}
	kind := p.Kind
	if kind == "" {
		kind = KindOf(path)
	}
	ref := Shown{Path: path, Kind: kind, Title: p.Title, At: s.cfg.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.cfg.Store.Add(session, ref); err != nil {
		return api.ContentShown{}, errf(api.ErrorKindUnavailable, "storing the reference: %v", err)
	}
	s.mu.Lock()
	delete(s.cache, session)
	s.mu.Unlock()
	it := item(session, path)
	applyShown(&it, ref)
	stat(&it)
	if s.cfg.Publish != nil {
		s.cfg.Publish(it)
	}
	return api.ContentShown{ID: it.ID, Session: session}, nil
}

// NoteFolder records the folder a session started in, so its content and attachments have a
// folder before its transcript records one.
func (s *Service) NoteFolder(session, folder string) {
	if err := s.cfg.Store.SetFolder(session, folder); err != nil {
		s.cfg.Logf("content: %v", err)
	}
}

// Folder returns the folder a session works in: the one recorded last (from its transcript or
// its start), else its transcript's, else its project's most recently used one, else "".
func (s *Service) Folder(ctx context.Context, session string) string {
	if f := s.cfg.Store.Folder(session); f != "" {
		return f
	}
	if s.cfg.Conversation != nil {
		if _, cwd, err := s.cfg.Conversation(ctx, session); err == nil && cwd != "" {
			s.NoteFolder(session, cwd)
			return cwd
		}
	}
	if s.cfg.ProjectDir != nil {
		return s.cfg.ProjectDir(session)
	}
	return ""
}

// Forget drops a session's stored references and folder: its record is gone.
// It runs inside the structure store's event callback, so the file write happens off this path.
func (s *Service) Forget(session string) {
	s.cfg.Store.Forget(session)
	s.mu.Lock()
	delete(s.cache, session)
	s.mu.Unlock()
}

func isKind(k api.ContentKind) bool {
	for _, w := range api.Enums["ContentKind"] {
		if string(k) == w {
			return true
		}
	}
	return false
}

func item(session, path string) api.ContentItem {
	return api.ContentItem{ID: ID(session, path), Session: session, Path: path, Name: filepath.Base(path),
		Kind: KindOf(path), Type: TypeOf(path)}
}

func applyShown(it *api.ContentItem, r Shown) {
	it.Shown, it.Title, it.At = true, r.Title, r.At
	if r.Kind != "" {
		it.Kind = r.Kind
	}
}

// stat fills Size and Modified for a regular file.
func stat(it *api.ContentItem) {
	if fi, err := os.Stat(it.Path); err == nil && fi.Mode().IsRegular() {
		n := fi.Size()
		it.Size = &n
		it.Modified = modTime(fi)
	}
}

// modTime is the modification time as content.read and content.list report it.
func modTime(fi os.FileInfo) string { return fi.ModTime().UTC().Format(time.RFC3339Nano) }

// List builds the session's content.
func (s *Service) List(ctx context.Context, p api.ContentListParams) (api.ContentList, error) {
	if p.Session == "" {
		return api.ContentList{}, errf(api.ErrorKindInvalid, "content.list needs a session")
	}
	b, err := s.build(ctx, p.Session)
	if err != nil {
		return api.ContentList{}, err
	}
	s.mu.Lock()
	s.cache[p.Session] = cached{at: s.cfg.Now(), list: b}
	s.mu.Unlock()
	items := b.items
	if items == nil {
		items = []api.ContentItem{}
	}
	return api.ContentList{Items: items, Folder: b.folder, More: b.more}, nil
}

// lookup finds a listed item of the session, from a list built at most CacheFor ago.
func (s *Service) lookup(ctx context.Context, session, id string) (api.ContentItem, built, error) {
	if session == "" || id == "" {
		return api.ContentItem{}, built{}, errf(api.ErrorKindInvalid, "needs a session and an id")
	}
	s.mu.Lock()
	c, ok := s.cache[session]
	s.mu.Unlock()
	b := c.list
	if !ok || s.cfg.Now().Sub(c.at) > s.cfg.CacheFor {
		var err error
		if b, err = s.build(ctx, session); err != nil {
			return api.ContentItem{}, built{}, err
		}
		s.mu.Lock()
		s.cache[session] = cached{at: s.cfg.Now(), list: b}
		s.mu.Unlock()
	}
	i, ok := b.byID[id]
	if !ok {
		return api.ContentItem{}, built{}, errf(api.ErrorKindNotFound, "no content %s in session %s", id, session)
	}
	return b.items[i], b, nil
}

// build lists a session's content: shown references, then files its tool calls touched, then
// git status of its folder.
func (s *Service) build(ctx context.Context, session string) (built, error) {
	if _, ok := s.cfg.Session(session); !ok {
		return built{}, errf(api.ErrorKindNotFound, "no session %s", session)
	}
	var msgs []api.ChatMessage
	var cwd string
	if s.cfg.Conversation != nil {
		var err error
		msgs, cwd, err = s.cfg.Conversation(ctx, session)
		if err != nil {
			s.cfg.Logf("content: session %s: transcript: %v", session, err)
		}
	}
	// The folder: the transcript's, else the one it recorded last (the agent state is gone after
	// a hab restart until the next hook), else the project's.
	folder := cwd
	if folder != "" {
		if err := s.cfg.Store.SetFolder(session, folder); err != nil {
			s.cfg.Logf("content: %v", err)
		}
	} else {
		folder = s.cfg.Store.Folder(session)
	}
	if folder == "" && s.cfg.ProjectDir != nil {
		folder = s.cfg.ProjectDir(session)
	}
	b := built{folder: folder, changes: map[string]gitstatus.Change{}, touches: map[string]Touch{}, byID: map[string]int{}, shown: map[string]bool{}}
	byPath := map[string]*api.ContentItem{}
	var order []string
	get := func(path string) *api.ContentItem {
		if it, ok := byPath[path]; ok {
			return it
		}
		it := item(session, path)
		byPath[path] = &it
		order = append(order, path)
		return &it
	}
	for _, r := range s.cfg.Store.List(session) {
		applyShown(get(r.Path), r)
		b.shown[r.Path] = true
	}
	for _, t := range Touches(msgs, folder) {
		b.touches[t.Path] = t
		it := get(t.Path)
		if it.At == "" || (!it.Shown && t.At != "") {
			it.At = t.At
		}
		it.Message = t.Message
		if t.Changed {
			it.Change = api.ContentChangeModified
		}
	}
	if folder != "" {
		gctx, cancel := context.WithTimeout(ctx, s.cfg.GitTimeout)
		top, changes, cut, err := s.cfg.Git.Changes(gctx, folder)
		cancel()
		if err != nil {
			s.cfg.Logf("content: session %s: git status: %v", session, err)
		}
		b.more = cut
		b.top = top
		realTop := top
		if top != "" {
			if r, err := filepath.EvalSymlinks(top); err == nil {
				realTop = r
			}
		}
		b.realTop = realTop
		for _, c := range changes {
			path := filepath.Join(top, filepath.FromSlash(c.Path))
			if !inside(realTop, path) {
				s.cfg.Logf("content: session %s: %s leads out of the repository; left out", session, c.Path)
				continue
			}
			b.changes[path] = c
			it := get(path)
			it.Change = api.ContentChange(c.Kind)
		}
	}
	items := make([]api.ContentItem, 0, len(order))
	for _, path := range order {
		it := byPath[path]
		stat(it)
		items = append(items, *it)
	}
	// Oldest first; items only git status lists (no time) go last, by path.
	sort.SliceStable(items, func(i, j int) bool {
		a, c := items[i].At, items[j].At
		switch {
		case a == "" && c == "":
			return items[i].Path < items[j].Path
		case a == "":
			return false
		case c == "":
			return true
		}
		return timeOf(a).Before(timeOf(c))
	})
	if limit := s.cfg.MaxItems; len(items) > limit {
		// Git-only items (no time) sit last and go first, so a large untracked tree never
		// pushes out what the agent showed or touched; then the oldest timed items go.
		timed := 0
		for timed < len(items) && items[timed].At != "" {
			timed++
		}
		keepGit := max(0, limit-timed)
		keepGit = min(keepGit, len(items)-timed)
		items = append(items[:timed:timed], items[timed:timed+keepGit]...)
		if len(items) > limit {
			items = items[len(items)-limit:]
		}
		b.more = true
	}
	b.items = items
	for i, it := range items {
		b.byID[it.ID] = i
	}
	return b, nil
}

func timeOf(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// inside says path, after following its symlinks, stays in top (already resolved). A path that
// does not exist (a deleted file) is checked through its nearest existing parent.
func inside(top, path string) bool {
	if top == "" {
		return false
	}
	p := path
	var rest []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			p = filepath.Join(append([]string{r}, rest...)...)
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
	rel, err := filepath.Rel(top, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Read reads a run of a listed file's bytes.
func (s *Service) Read(ctx context.Context, p api.ContentReadParams) (api.ContentData, error) {
	if p.Offset < 0 {
		return api.ContentData{}, errf(api.ErrorKindInvalid, "offset must not be negative")
	}
	if p.Limit < 0 {
		return api.ContentData{}, errf(api.ErrorKindInvalid, "limit must not be negative")
	}
	it, b, err := s.lookup(ctx, p.Session, p.ID)
	if err != nil {
		return api.ContentData{}, err
	}
	if err := b.check(it.Path); err != nil {
		return api.ContentData{}, err
	}
	limit := p.Limit
	if limit == 0 {
		limit = DefaultChunk
	}
	limit = min(limit, MaxChunk)
	// O_NONBLOCK: opening a FIFO someone put at the path must not hang the call.
	f, err := os.OpenFile(it.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return api.ContentData{}, errf(api.ErrorKindNotFound, "%s is no longer on the host", it.Path)
	}
	defer f.Close()
	// Checked again on the open file: the path may have changed since check.
	fi, err := f.Stat()
	if err != nil {
		return api.ContentData{}, errf(api.ErrorKindUnavailable, "%s: %v", it.Path, err)
	}
	if !fi.Mode().IsRegular() {
		return api.ContentData{}, errf(api.ErrorKindInvalid, "%s is not a regular file", it.Path)
	}
	size := fi.Size()
	if size > MaxFile {
		return api.ContentData{}, errf(api.ErrorKindInvalid, "%s is %d bytes; content.read serves files up to %d bytes", it.Path, size, MaxFile)
	}
	if p.Offset > size {
		return api.ContentData{}, errf(api.ErrorKindInvalid, "offset %d is past the end of the file (%d bytes)", p.Offset, size)
	}
	n := min(int64(limit), size-p.Offset)
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, p.Offset)
	if err != nil && err != io.EOF {
		return api.ContentData{}, errf(api.ErrorKindUnavailable, "%s: %v", it.Path, err)
	}
	buf = buf[:got]
	return api.ContentData{
		ID: it.ID, Type: it.Type, Size: size, Modified: modTime(fi),
		Offset: p.Offset, Data: buf, EOF: p.Offset+int64(got) >= size,
	}, nil
}

// Diff returns a listed file's diff: git against HEAD, every line of an untracked or added
// file, or the session's edit calls when git knows no change.
func (s *Service) Diff(ctx context.Context, p api.ContentDiffParams) (api.ContentDiff, error) {
	it, b, err := s.lookup(ctx, p.Session, p.ID)
	if err != nil {
		return api.ContentDiff{}, err
	}
	out := api.ContentDiff{ID: it.ID, Path: it.Path, Source: api.ContentDiffSourceNone}
	rel := ""
	if b.top != "" {
		if r, err := filepath.Rel(b.top, it.Path); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = filepath.ToSlash(r)
		}
	}
	if err := b.check(it.Path); err != nil {
		return api.ContentDiff{}, err
	}
	c, changed := b.changes[it.Path]
	if changed && (c.Kind == "untracked" || c.Kind == "added") {
		d, cut, err := addedDiff(it.Path, rel)
		if err == nil {
			out.Source, out.Diff, out.Truncated = api.ContentDiffSourceUntracked, d, cut
			return out, nil
		}
	}
	if changed && rel != "" {
		gctx, cancel := context.WithTimeout(ctx, s.cfg.GitTimeout)
		d, cut, err := s.cfg.Git.Diff(gctx, b.top, rel, MaxDiff)
		cancel()
		if err != nil {
			s.cfg.Logf("content: git diff %s: %v", rel, err)
		} else if d != "" {
			out.Source, out.Diff, out.Truncated = api.ContentDiffSourceGit, d, cut
			return out, nil
		}
	}
	if t, ok := b.touches[it.Path]; ok && len(t.Edits) > 0 {
		out.Source = api.ContentDiffSourceTranscript
		out.Diff, out.Truncated = editsDiff(it.Path, t.Edits)
	}
	return out, nil
}

// check refuses a path that is not a regular file (a device reacts to an open) and, for an item
// only git status listed, one that now leads out of the repository through a symlink.
func (b built) check(path string) error {
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return errf(api.ErrorKindInvalid, "%s is not a regular file", path)
	}
	_, gitOnly := b.changes[path]
	_, touched := b.touches[path]
	if gitOnly && !touched && !b.shown[path] && !inside(b.realTop, path) {
		return errf(api.ErrorKindNotFound, "%s leads out of the repository", path)
	}
	return nil
}

// diffName keeps a file name on one diff header line.
func diffName(s string) string {
	return strings.NewReplacer("\n", "?", "\r", "?").Replace(s)
}

// addedDiff is a diff that adds every line of the file.
func addedDiff(path, rel string) (string, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxDiff))
	if err != nil {
		return "", false, err
	}
	name := diffName(rel)
	if rel == "" {
		name = diffName(path)
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return fmt.Sprintf("--- /dev/null\n+++ b/%s\nBinary file\n", name), false, nil
	}
	lines := splitLines(string(data))
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", name, len(lines))
	for _, l := range lines {
		sb.WriteString("+" + l + "\n")
	}
	return capDiff(sb.String(), fi.Size() > MaxDiff)
}

// editsDiff writes the session's edit calls as one diff, one hunk per edit. Line numbers are
// unknown, so each hunk header names the edit instead.
func editsDiff(path string, edits []Edit) (string, bool) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a%s\n+++ b%s\n", diffName(path), diffName(path))
	for i, e := range edits {
		switch {
		case e.Deleted:
			fmt.Fprintf(&sb, "@@ edit %d: deleted @@\n", i+1)
		case e.Diff != "":
			d := e.Diff
			if !strings.HasSuffix(d, "\n") {
				d += "\n"
			}
			sb.WriteString(d)
		default:
			old, nw := splitLines(e.Old), splitLines(e.New)
			if e.Write {
				old = nil
			}
			fmt.Fprintf(&sb, "@@ -1,%d +1,%d @@ edit %d\n", len(old), len(nw), i+1)
			for _, l := range old {
				sb.WriteString("-" + l + "\n")
			}
			for _, l := range nw {
				sb.WriteString("+" + l + "\n")
			}
		}
	}
	d, cut, _ := capDiff(sb.String(), false)
	return d, cut
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// capDiff cuts a diff at MaxDiff, on a line end.
func capDiff(d string, cut bool) (string, bool, error) {
	if len(d) <= MaxDiff {
		return d, cut, nil
	}
	d = d[:MaxDiff]
	if i := strings.LastIndexByte(d, '\n'); i > 0 {
		d = d[:i+1]
	}
	return d, true, nil
}
