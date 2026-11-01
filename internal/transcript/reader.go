// Package transcript reads a harness's conversation transcript in place and serves it as chat
// messages (docs/specs/harness-adapters.md, Claude Code: conversation). It tails the file the
// harness writes and never copies, moves or rewrites it.
//
// Message ids are stable: they come from the transcript's own record ids, or from byte offsets
// for records with none. A cursor names a byte offset in the file's contents, so a client
// resumes with only what was added or changed, even across a gff-hab restart.
package transcript

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Reader reads one Claude Code transcript. It is safe for concurrent use.
type Reader struct {
	path     string
	session  string // the gff-hab session id put on every message
	logf     func(string, ...any)
	onChange func([]api.ChatMessage)

	mu      sync.Mutex
	t       *tail
	c       *conversation
	p       *claudeParser
	loaded  bool // the first read is done; changes after it are reported to onChange
	warned  map[string]bool
	drift   string
	resets  int     // replacements seen
	pending []*item // changed during the current refresh
}

// Config is what NewClaude needs. Path is required.
type Config struct {
	Path    string // the session JSONL
	Session string // gff-hab session id for the messages
	Logf    func(string, ...any)
	// OnChange receives the messages a read added or changed, in conversation order. The first
	// read of a file reports nothing: a client takes a page. After a replacement it receives
	// one message of kind event, event reset, and nothing for the contents read again.
	// It is called with the reader's lock held and must not call back into the reader.
	OnChange func([]api.ChatMessage)
	// FromEmpty says a client already saw this conversation as empty (the file did not exist
	// yet): the first read reports every message it finds to OnChange.
	FromEmpty bool
}

// NewClaude returns a reader of a Claude Code session transcript. It reads nothing until the
// first call; a file that does not exist yet reads as an empty conversation.
func NewClaude(cfg Config) *Reader {
	r := &Reader{path: cfg.Path, session: cfg.Session, logf: cfg.Logf, onChange: cfg.OnChange, warned: map[string]bool{}}
	if r.logf == nil {
		r.logf = func(string, ...any) {}
	}
	r.reset()
	r.loaded = cfg.FromEmpty
	return r
}

func (r *Reader) reset() {
	r.t = &tail{path: r.path}
	r.c = newConversation()
	r.p = &claudeParser{c: r.c, tools: map[string]string{}, r: r, changed: func(it *item) { r.pending = append(r.pending, it) }}
	r.loaded = false
}

// warn logs a problem once per key for this reader.
func (r *Reader) warn(key, format string, args ...any) {
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	r.logf("transcript %s: "+format, append([]any{r.path}, args...)...)
}

// checkVersion records drift when a record's Claude Code version is outside the tested range.
// Drift follows the newest record: a record inside the range clears it.
func (r *Reader) checkVersion(v string) {
	lo, ok1 := compareVersions(v, ClaudeTestedMin)
	hi, ok2 := compareVersions(v, ClaudeTestedMax)
	switch {
	case !ok1 || !ok2:
		r.drift = fmt.Sprintf("Claude Code version %q is unreadable; reading in tolerant mode", v)
	case lo < 0 || hi > 0:
		r.drift = fmt.Sprintf("Claude Code %s is outside the tested range %s to %s; reading in tolerant mode", v, ClaudeTestedMin, ClaudeTestedMax)
	default:
		r.drift = ""
		return
	}
	r.warn("version:"+v, "%s", r.drift)
}

// Drift describes why the reader runs in tolerant mode, or is empty.
func (r *Reader) Drift() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drift
}

// Refresh reads what the harness appended since the last read.
func (r *Reader) Refresh() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refresh(context.Background())
}

// Size is how many bytes of the file the reader has read.
func (r *Reader) Size() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t.read
}

// refresh reads the new lines. When ctx ends partway, the lines read so far are kept and the
// next refresh goes on from there; a first read that ends early still reports nothing.
func (r *Reader) refresh(ctx context.Context) error {
	lines, err := r.t.next(ctx)
	if errors.Is(err, errReplaced) {
		r.resets++
		r.logf("transcript %s: the file was replaced or truncated (%d times so far); reading it again from the start", r.path, r.resets)
		seen := r.loaded
		r.reset()
		r.drift = ""
		if seen && r.onChange != nil {
			// Messages a client holds may be gone or moved: tell it to take the tail again.
			r.onChange([]api.ChatMessage{{ID: "reset-" + strconv.Itoa(r.resets), Session: r.session,
				Kind: api.ChatKindEvent, Event: api.ChatEventReset, At: time.Now().UTC().Format(time.RFC3339),
				Text: "The transcript was replaced; the conversation was read again"}})
		}
		lines, err = r.t.next(ctx)
	}
	if errors.Is(err, fs.ErrNotExist) {
		// Not written yet: the harness may create it after its first hook. A client sees an
		// empty conversation, so what the file holds once it appears is reported.
		r.loaded = true
		return nil
	}
	r.pending = r.pending[:0]
	for _, l := range lines {
		r.p.line(l.data, l.end)
	}
	if r.loaded && len(r.pending) > 0 && r.onChange != nil {
		r.onChange(r.messages(dedupe(r.pending)))
	}
	r.pending = r.pending[:0]
	if err == nil {
		r.loaded = true
	}
	return err
}

// dedupe keeps the last report of each item, in conversation order.
func dedupe(items []*item) []*item {
	seen := map[*item]bool{}
	var out []*item
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out
}

func (r *Reader) messages(items []*item) []api.ChatMessage {
	out := make([]api.ChatMessage, 0, len(items))
	for _, it := range items {
		m := it.msg
		m.Session = r.session
		out = append(out, m)
	}
	return out
}

func (r *Reader) withSession(msgs []api.ChatMessage) []api.ChatMessage {
	for i := range msgs {
		msgs[i].Session = r.session
	}
	return msgs
}

// Page reads the file's new lines, then returns up to limit messages before the message with id
// before (the tail when before is empty), oldest first, whether older ones exist, and a cursor
// for Since. The cursor is empty while the file holds no complete line: there is nothing to
// resume from, and the client takes a page again. When ctx ends during the read, Page returns
// its error.
func (r *Reader) Page(ctx context.Context, before string, limit int) (msgs []api.ChatMessage, more bool, cursor string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(ctx); err != nil {
		return nil, false, "", err
	}
	msgs, more, err = r.c.page(before, limit)
	if err != nil {
		return nil, false, "", err
	}
	if msgs == nil {
		msgs = []api.ChatMessage{}
	}
	return r.withSession(msgs), more, formatCursor(r.t.gen, r.t.done), nil
}

// Since reads the file's new lines, then returns the messages added or changed after cursor,
// oldest first, the cursor to pass next, and whether more changes wait after it. A cursor from
// other file contents, or past the end, is ErrStaleCursor: the client takes the tail again.
func (r *Reader) Since(ctx context.Context, cursor string, limit int) (msgs []api.ChatMessage, more bool, next string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(ctx); err != nil {
		return nil, false, "", err
	}
	from, err := parseCursor(cursor, r.t.gen, r.t.done)
	if err != nil {
		return nil, false, "", err
	}
	msgs, k, more := r.c.since(from, r.t.done, limit)
	if msgs == nil {
		msgs = []api.ChatMessage{}
	}
	return r.withSession(msgs), more, formatCursor(r.t.gen, k), nil
}

// claudeSessionID is the shape of a Claude Code session id that may name a file.
var claudeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ErrNoTranscript: no transcript file exists for the session id yet.
var ErrNoTranscript = errors.New("no transcript for this session yet")

// FindClaude finds the transcript of a Claude Code session under the Claude config directory:
// projects/<encoded cwd>/<session id>.jsonl. When several project folders hold one (a session
// resumed from another folder), the most recently written wins.
func FindClaude(configDir, sessionID string) (string, error) {
	if !claudeSessionID.MatchString(sessionID) {
		return "", fmt.Errorf("session id %q cannot name a transcript", sessionID)
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	var best string
	var bestMod int64
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if mod := info.ModTime().UnixNano(); best == "" || mod > bestMod {
			best, bestMod = m, mod
		}
	}
	if best == "" {
		return "", ErrNoTranscript
	}
	return best, nil
}

// ClaudeConfigDir is where Claude Code keeps its data: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ClaudeConfigDir(getenv func(string) string) (string, error) {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}
