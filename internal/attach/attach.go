// Package attach stores the files the owner sends to a session (app.md 4.4): an upload in
// ordered runs, checked by its SHA-256, written into the session's folder where every harness
// can read it, and handed to the agent as a path.
package attach

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

const (
	// MaxSize is the largest file the hab takes (app.md 4.4).
	MaxSize = 25_000_000
	// Chunk is the most bytes one attachment.write takes, and the most one attachment.get run
	// returns: content.read's bound.
	Chunk = 1 << 20
	// DefaultRun is the size of an attachment.get run when the caller names no limit.
	DefaultRun = 256 << 10
	// KeepFor is how long a stored file stays before the hab deletes it.
	KeepFor = 30 * 24 * time.Hour
	// UploadIdle drops an upload that got no write for this long.
	UploadIdle = 2 * time.Minute
	// MaxOpen bounds the uploads open at once.
	MaxOpen = 8
	// MaxName bounds a stored file name, in bytes, before the id prefix.
	MaxName = 100
	// Folder is where attachments go, under the session's folder.
	Folder = ".flightdeck/attachments"
	// IDLen is the length of an attachment id: hex.
	IDLen = 12
)

// pathRE finds an attachment path in message text: the folder, then the id and a dash.
var pathRE = regexp.MustCompile(`(?:^|/)\.flightdeck/attachments/([0-9a-f]{12})-[^/\s]+$`)

// IDOfPath returns the id in a stored attachment's path, or "".
func IDOfPath(path string) string {
	m := pathRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return m[1]
}

// Record is one stored file.
type Record struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	At      time.Time `json:"at"`
}

// Config is what New needs. Session and Folder are required.
type Config struct {
	// StatePath is the index file. Empty: nothing is saved.
	StatePath string
	// Session reports whether a session record exists.
	Session func(id string) bool
	// Folder returns the folder a session works in, or "" when the hab knows none.
	Folder func(ctx context.Context, session string) string
	// Fallback holds a folder per session for sessions with no folder.
	Fallback string
	Now      func() time.Time
	Logf     func(string, ...any)
	MaxSize  int64 // default MaxSize
}

// Service serves attachment.put, attachment.write and attachment.get.
type Service struct {
	cfg Config

	mu      sync.Mutex
	recs    map[string]Record
	open    map[string]*upload
	stop    chan struct{}
	stopped sync.Once
	done    chan struct{}
}

type upload struct {
	rec      Record
	part     string
	f        *os.File
	h        hash.Hash
	received int64
	last     time.Time
	busy     bool // a write runs
}

// index is the saved state: the stored files and the partial files of open uploads.
type index struct {
	Files []Record `json:"files"`
	Parts []string `json:"parts,omitempty"`
}

func errf(kind api.ErrorKind, format string, args ...any) error {
	return control.Errorf(kind, format, args...)
}

// New opens the index, removes the partial files an earlier run left, and starts the sweeper.
// A broken index is logged and replaced: it holds no data the files themselves lack.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = MaxSize
	}
	s := &Service{cfg: cfg, recs: map[string]Record{}, open: map[string]*upload{}, stop: make(chan struct{}), done: make(chan struct{})}
	if cfg.StatePath != "" {
		b, err := os.ReadFile(cfg.StatePath)
		var ix index
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			cfg.Logf("attachments: %v", err)
		case json.Unmarshal(b, &ix) != nil:
			cfg.Logf("attachments: %s is not valid; starting a new index", cfg.StatePath)
		default:
			for _, r := range ix.Files {
				if validID(r.ID) {
					s.recs[r.ID] = r
				}
			}
			for _, p := range ix.Parts {
				if strings.HasSuffix(p, ".part") && strings.Contains(p, Folder) {
					os.Remove(p)
				}
			}
		}
	}
	s.mu.Lock()
	s.expireLocked()
	s.saveLocked()
	s.mu.Unlock()
	go s.sweep()
	return s
}

// Close stops the sweeper and drops the open uploads.
func (s *Service) Close() {
	s.stopped.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		for id, u := range s.open {
			if !u.busy {
				u.f.Close()
				os.Remove(u.part)
			}
			delete(s.open, id)
		}
		s.saveLocked()
		s.mu.Unlock()
	})
}

func (s *Service) sweep() {
	defer close(s.done)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		s.dropIdleLocked()
		if n++; n%120 == 0 { // hourly
			s.expireLocked()
		}
		s.saveLocked()
		s.mu.Unlock()
	}
}

// Sweep drops idle uploads and expired files now. Tests call it.
func (s *Service) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropIdleLocked()
	s.expireLocked()
	s.saveLocked()
}

func (s *Service) dropIdleLocked() {
	now := s.cfg.Now()
	for id, u := range s.open {
		if !u.busy && now.Sub(u.last) >= UploadIdle {
			u.f.Close()
			os.Remove(u.part)
			delete(s.open, id)
			s.cfg.Logf("attachments: upload %s dropped after %s with no write", id, UploadIdle)
		}
	}
}

// expireLocked deletes files older than KeepFor. A file the agent moved or replaced is left
// alone: only a regular file of the stored size at the stored path goes.
func (s *Service) expireLocked() {
	now := s.cfg.Now()
	for id, r := range s.recs {
		fi, err := os.Lstat(r.Path)
		if err != nil {
			delete(s.recs, id) // gone: the agent moved it, or the owner deleted it
			continue
		}
		if now.Sub(r.At) < KeepFor {
			continue
		}
		if fi.Mode().IsRegular() && fi.Size() == r.Size {
			if err := os.Remove(r.Path); err != nil {
				s.cfg.Logf("attachments: %v", err)
				continue
			}
		}
		delete(s.recs, id)
	}
}

func (s *Service) saveLocked() {
	if s.cfg.StatePath == "" {
		return
	}
	ix := index{Files: make([]Record, 0, len(s.recs))}
	for _, r := range s.recs {
		ix.Files = append(ix.Files, r)
	}
	sort.Slice(ix.Files, func(i, j int) bool { return ix.Files[i].ID < ix.Files[j].ID })
	for _, u := range s.open {
		ix.Parts = append(ix.Parts, u.part)
	}
	sort.Strings(ix.Parts)
	b, err := json.Marshal(ix)
	if err == nil {
		err = writeFileAtomic(s.cfg.StatePath, b)
	}
	if err != nil {
		s.cfg.Logf("attachments: %v", err)
	}
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validID(id string) bool {
	if len(id) != IDLen {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func newID() string {
	var b [IDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// SafeName turns the owner's file name into one that is safe in a path and in message text: no
// folder part, no spaces or shell metacharacters, no leading dot, at most MaxName bytes with
// the extension kept.
func SafeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		case r >= utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > MaxName {
		ext := filepath.Ext(out)
		if len(ext) > 16 {
			ext = ""
		}
		// A dropped extension stays in the stem, which is then cut like any other text.
		stem := out[:len(out)-len(ext)]
		cut := min(MaxName-len(ext), len(stem))
		for cut > 0 && cut < len(stem) && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		out = strings.TrimRight(stem[:cut], ".-") + ext
	}
	if out == "" || out == "." {
		out = "file"
	}
	return out
}

// folderFor makes the session's attachment folder and its ignore file, and returns the folder.
func (s *Service) folderFor(ctx context.Context, session string) (string, error) {
	base := ""
	if s.cfg.Folder != nil {
		base = s.cfg.Folder(ctx, session)
	}
	if base != "" {
		if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
			base = ""
		}
	}
	fallback := ""
	if s.cfg.Fallback != "" {
		fallback = filepath.Join(s.cfg.Fallback, session)
	}
	if base == "" {
		if fallback == "" {
			return "", errf(api.ErrorKindUnavailable, "session %s has no folder to store attachments in", session)
		}
		base = fallback
	}
	dir := filepath.Join(base, Folder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// A folder the hab may not write in (a session started in /): the hab's own folder.
		if fallback == "" || base == fallback {
			return "", errf(api.ErrorKindUnavailable, "make the attachment folder: %v", err)
		}
		s.cfg.Logf("attachments: session %s: %v; storing in %s", session, err, fallback)
		base, dir = fallback, filepath.Join(fallback, Folder)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", errf(api.ErrorKindUnavailable, "make the attachment folder: %v", err)
		}
	}
	top := filepath.Join(base, ".flightdeck")
	// The folder ignores itself: a local, untracked rule that needs no git and edits no tracked
	// ignore file (app.md 18.12). An ignore file the owner put there is left alone.
	ign := filepath.Join(top, ".gitignore")
	if f, err := os.OpenFile(ign, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_, werr := f.WriteString("# Written by Flightdeck: attachments sent from the app. Never committed.\n*\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			s.cfg.Logf("attachments: %v", werr)
		}
	}
	return dir, nil
}

// Put starts an upload.
func (s *Service) Put(ctx context.Context, p api.AttachmentPutParams) (api.AttachmentStored, error) {
	if p.Session == "" || !s.cfg.Session(p.Session) {
		return api.AttachmentStored{}, errf(api.ErrorKindNotFound, "no session %s", p.Session)
	}
	if strings.TrimSpace(p.Name) == "" {
		return api.AttachmentStored{}, errf(api.ErrorKindInvalid, "attachment.put needs a name")
	}
	if p.Size > uint64(s.cfg.MaxSize) {
		return api.AttachmentStored{}, errf(api.ErrorKindInvalid, "the file is %d bytes; the limit is %d MB", p.Size, s.cfg.MaxSize/1_000_000)
	}
	sum := strings.ToLower(p.SHA256)
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
		return api.AttachmentStored{}, errf(api.ErrorKindInvalid, "sha256 %q is not 64 hex digits", p.SHA256)
	}
	s.mu.Lock()
	n := len(s.open)
	s.mu.Unlock()
	if n >= MaxOpen {
		return api.AttachmentStored{}, errf(api.ErrorKindUnavailable, "%d uploads are open; finish one first", MaxOpen)
	}
	dir, err := s.folderFor(ctx, p.Session)
	if err != nil {
		return api.AttachmentStored{}, err
	}
	id := newID()
	name := SafeName(p.Name)
	typ := p.Type
	if typ == "" {
		typ = "application/octet-stream"
	}
	rec := Record{ID: id, Session: p.Session, Path: filepath.Join(dir, id+"-"+name), Name: p.Name, Type: typ,
		Size: int64(p.Size), SHA256: sum, At: s.cfg.Now()}
	if p.Size == 0 {
		if sum != hex.EncodeToString(sha256.New().Sum(nil)) {
			return api.AttachmentStored{}, errf(api.ErrorKindInvalid, "the hash does not match an empty file")
		}
		f, err := os.OpenFile(rec.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return api.AttachmentStored{}, errf(api.ErrorKindUnavailable, "store the file: %v", err)
		}
		f.Close()
		s.mu.Lock()
		s.recs[id] = rec
		s.saveLocked()
		s.mu.Unlock()
		return api.AttachmentStored{ID: id, Path: rec.Path, Chunk: Chunk, Done: true}, nil
	}
	part := filepath.Join(dir, "."+id+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return api.AttachmentStored{}, errf(api.ErrorKindUnavailable, "start the upload: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.open) >= MaxOpen { // another put won the last place meanwhile
		f.Close()
		os.Remove(part)
		return api.AttachmentStored{}, errf(api.ErrorKindUnavailable, "%d uploads are open; finish one first", MaxOpen)
	}
	s.open[id] = &upload{rec: rec, part: part, f: f, h: sha256.New(), last: s.cfg.Now()}
	s.saveLocked()
	return api.AttachmentStored{ID: id, Path: rec.Path, Chunk: Chunk}, nil
}

// Write takes the next run of an upload.
func (s *Service) Write(ctx context.Context, p api.AttachmentWriteParams) (api.AttachmentWritten, error) {
	s.mu.Lock()
	u, ok := s.open[p.ID]
	if !ok {
		r, stored := s.recs[p.ID]
		s.mu.Unlock()
		if stored {
			return api.AttachmentWritten{ID: p.ID, Received: r.Size, Done: true}, nil
		}
		return api.AttachmentWritten{}, errf(api.ErrorKindNotFound, "no upload %s; it may have been dropped after %s with no write", p.ID, UploadIdle)
	}
	switch {
	case u.busy:
		s.mu.Unlock()
		return api.AttachmentWritten{}, errf(api.ErrorKindConflict, "a write to upload %s is still running", p.ID)
	case p.Offset != u.received:
		got := u.received
		s.mu.Unlock()
		return api.AttachmentWritten{}, errf(api.ErrorKindConflict, "upload %s has %d bytes; send the run at offset %d", p.ID, got, got)
	case len(p.Data) == 0 || len(p.Data) > Chunk:
		s.mu.Unlock()
		return api.AttachmentWritten{}, errf(api.ErrorKindInvalid, "a run is 1 to %d bytes, not %d", Chunk, len(p.Data))
	case u.received+int64(len(p.Data)) > u.rec.Size:
		s.mu.Unlock()
		return api.AttachmentWritten{}, errf(api.ErrorKindInvalid, "the run ends at %d, past the size %d", u.received+int64(len(p.Data)), u.rec.Size)
	}
	u.busy = true
	s.mu.Unlock()

	_, err := u.f.Write(p.Data)
	if err == nil {
		u.h.Write(p.Data)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	u.busy = false
	u.last = s.cfg.Now()
	if err != nil {
		s.dropLocked(u)
		return api.AttachmentWritten{}, errf(api.ErrorKindUnavailable, "write upload %s: %v; start it again", p.ID, err)
	}
	u.received += int64(len(p.Data))
	if u.received < u.rec.Size {
		return api.AttachmentWritten{ID: p.ID, Received: u.received}, nil
	}
	// The last run: the hash decides.
	got := hex.EncodeToString(u.h.Sum(nil))
	if err := u.f.Close(); err != nil {
		s.dropLocked(u)
		return api.AttachmentWritten{}, errf(api.ErrorKindUnavailable, "write upload %s: %v; start it again", p.ID, err)
	}
	if got != u.rec.SHA256 {
		os.Remove(u.part)
		delete(s.open, u.rec.ID)
		s.saveLocked()
		return api.AttachmentWritten{}, errf(api.ErrorKindInvalid, "upload %s: the bytes hash to %s, not %s; start it again", p.ID, got, u.rec.SHA256)
	}
	if err := os.Rename(u.part, u.rec.Path); err != nil {
		os.Remove(u.part)
		delete(s.open, u.rec.ID)
		s.saveLocked()
		return api.AttachmentWritten{}, errf(api.ErrorKindUnavailable, "store upload %s: %v", p.ID, err)
	}
	delete(s.open, u.rec.ID)
	u.rec.At = s.cfg.Now()
	s.recs[u.rec.ID] = u.rec
	s.saveLocked()
	return api.AttachmentWritten{ID: p.ID, Received: u.received, Done: true}, nil
}

func (s *Service) dropLocked(u *upload) {
	u.f.Close()
	os.Remove(u.part)
	delete(s.open, u.rec.ID)
	s.saveLocked()
}

// Get returns a run of a stored file, or its thumbnail.
func (s *Service) Get(ctx context.Context, p api.AttachmentGetParams) (api.AttachmentData, error) {
	s.mu.Lock()
	r, ok := s.recs[p.ID]
	s.mu.Unlock()
	if !ok {
		return api.AttachmentData{}, errf(api.ErrorKindNotFound, "no attachment %s", p.ID)
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return api.AttachmentData{}, errf(api.ErrorKindNotFound, "attachment %s is no longer at %s", p.ID, r.Path)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return api.AttachmentData{}, errf(api.ErrorKindNotFound, "attachment %s is no longer at %s", p.ID, r.Path)
	}
	if p.Thumbnail {
		if p.Offset != 0 || p.Limit != 0 {
			return api.AttachmentData{}, errf(api.ErrorKindInvalid, "thumbnail takes no offset or limit")
		}
		if fi.Size() > s.cfg.MaxSize {
			return api.AttachmentData{}, errf(api.ErrorKindInvalid, "attachment %s is too large for a thumbnail", p.ID)
		}
		b, err := Thumbnail(ctx, io.LimitReader(f, s.cfg.MaxSize))
		if err != nil {
			return api.AttachmentData{}, errf(api.ErrorKindInvalid, "no thumbnail for %s (%s): %v", p.ID, r.Type, err)
		}
		return api.AttachmentData{ID: r.ID, Name: r.Name, Type: "image/jpeg", Data: b, Size: int64(len(b)), EOF: true}, nil
	}
	limit := p.Limit
	switch {
	case limit < 0:
		return api.AttachmentData{}, errf(api.ErrorKindInvalid, "limit %d is negative", limit)
	case limit == 0:
		limit = DefaultRun
	case limit > Chunk:
		limit = Chunk
	}
	if p.Offset < 0 || p.Offset > fi.Size() {
		return api.AttachmentData{}, errf(api.ErrorKindInvalid, "offset %d is outside the file's %d bytes", p.Offset, fi.Size())
	}
	n := min(int64(limit), fi.Size()-p.Offset)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, p.Offset); err != nil && !errors.Is(err, io.EOF) {
		return api.AttachmentData{}, errf(api.ErrorKindUnavailable, "read attachment %s: %v", p.ID, err)
	}
	return api.AttachmentData{ID: r.ID, Name: r.Name, Type: r.Type, Data: buf, Size: fi.Size(), Offset: p.Offset,
		EOF: p.Offset+n == fi.Size()}, nil
}

// Paths returns the stored paths of ids for a session.send, in order.
func (s *Service) Paths(session string, ids []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if u, ok := s.open[id]; ok && u.rec.Session == session {
			return nil, errf(api.ErrorKindConflict, "attachment %s is still uploading (%d of %d bytes)", id, u.received, u.rec.Size)
		}
		r, ok := s.recs[id]
		if !ok || r.Session != session {
			return nil, errf(api.ErrorKindNotFound, "no attachment %s for session %s", id, session)
		}
		if _, err := os.Stat(r.Path); err != nil {
			return nil, errf(api.ErrorKindNotFound, "attachment %s is no longer at %s", id, r.Path)
		}
		out = append(out, r.Path)
	}
	return out, nil
}

// Known reports whether id names a stored file. The chat view uses it to show an attachment
// only for a file this hab stored.
func (s *Service) Known(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recs[id]
	return ok
}

// MessageText is the text the adapter delivers for a message with attachments: the owner's
// text, a blank line, then each path on its own line. Every supported harness reads a path in
// the message with its own file tools; Claude Code also takes an image path as an image.
func MessageText(text string, paths []string) string {
	if len(paths) == 0 {
		return text
	}
	list := strings.Join(paths, "\n")
	if strings.TrimSpace(text) == "" {
		return list
	}
	return strings.TrimRight(text, "\n") + "\n\n" + list
}

// SplitMessage undoes MessageText for the chat view: it takes the trailing lines that are
// attachment paths off a user message and returns the rest and their ids, in order. known
// filters ids; nil keeps all.
func SplitMessage(text string, known func(id string) bool) (string, []string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	end := len(lines)
	var ids []string
	for end > 0 {
		line := strings.TrimSpace(lines[end-1])
		if line == "" && len(ids) > 0 {
			end--
			continue
		}
		id := IDOfPath(line)
		if id == "" || !filepath.IsAbs(line) || (known != nil && !known(id)) {
			break
		}
		ids = append(ids, id)
		end--
	}
	if len(ids) == 0 {
		return text, nil
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return strings.TrimRight(strings.Join(lines[:end], "\n"), "\n"), ids
}

// String names a record for logs.
func (r Record) String() string { return fmt.Sprintf("%s (%s, %d bytes)", r.ID, r.Name, r.Size) }
