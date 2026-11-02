package readtools

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// Harness words of the transcripts this package reads.
const (
	Claude = "claude"
	Codex  = "codex"
)

// headBytes bounds how much of a transcript's start is read for its folder, start and title.
const headBytes = 256 << 10

// titleChars bounds a session title.
const titleChars = 100

// Entry is one harness session found in its native transcript file.
type Entry struct {
	Harness string
	ID      string // the harness's own session id
	Path    string
	Folder  string // the working folder the transcript recorded
	Title   string
	Started time.Time // zero when no record had a time
	Updated time.Time // the file's modification time
	Size    int64
}

// Transcripts finds the transcripts of Claude Code (projects/<folder>/<id>.jsonl under its
// config folder) and Codex (sessions/YYYY/MM/DD/rollout-*.jsonl under its home), read in place.
// It keeps the head facts of each file until its size or time changes. Until the transcript
// index (mesh.md 9) exists, this scan is the session list.
type Transcripts struct {
	ClaudeDir string // "" for none
	CodexHome string // "" for none

	mu    sync.Mutex
	heads map[string]Entry
}

// Roots are the folders the transcripts live in: the default allowed roots for them.
func (t *Transcripts) Roots() []string {
	var out []string
	if t.ClaudeDir != "" {
		out = append(out, filepath.Join(t.ClaudeDir, "projects"))
	}
	if t.CodexHome != "" {
		out = append(out, filepath.Join(t.CodexHome, "sessions"), filepath.Join(t.CodexHome, "archived_sessions"))
	}
	return out
}

// List returns every transcript, newest written first.
func (t *Transcripts) List(ctx context.Context) []Entry {
	type file struct {
		harness, path string
	}
	var files []file
	if t.ClaudeDir != "" {
		ms, _ := filepath.Glob(filepath.Join(t.ClaudeDir, "projects", "*", "*.jsonl"))
		for _, m := range ms {
			files = append(files, file{Claude, m})
		}
	}
	if t.CodexHome != "" {
		for _, pat := range []string{
			filepath.Join(t.CodexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"),
			filepath.Join(t.CodexHome, "archived_sessions", "rollout-*.jsonl"),
		} {
			ms, _ := filepath.Glob(pat)
			for _, m := range ms {
				files = append(files, file{Codex, m})
			}
		}
	}
	t.mu.Lock()
	if t.heads == nil {
		t.heads = map[string]Entry{}
	}
	old := t.heads
	t.mu.Unlock()
	heads := make(map[string]Entry, len(files))
	out := make([]Entry, 0, len(files))
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		fi, err := os.Stat(f.path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		e, ok := old[f.path]
		if !ok || e.Size != fi.Size() || !e.Updated.Equal(fi.ModTime()) {
			e = readHead(f.harness, f.path)
			e.Size, e.Updated = fi.Size(), fi.ModTime()
		}
		heads[f.path] = e
		out = append(out, e)
	}
	t.mu.Lock()
	t.heads = heads
	t.mu.Unlock()
	slices.SortStableFunc(out, func(a, b Entry) int { return b.Updated.Compare(a.Updated) })
	return out
}

// Find returns the transcript of a harness session id, newest written when several hold it.
func (t *Transcripts) Find(harness, id string) (string, error) {
	switch harness {
	case Claude:
		if t.ClaudeDir == "" {
			return "", transcript.ErrNoTranscript
		}
		return transcript.FindClaude(t.ClaudeDir, id)
	case Codex:
		if t.CodexHome == "" {
			return "", transcript.ErrNoTranscript
		}
		return transcript.FindCodex(t.CodexHome, id)
	}
	return "", transcript.ErrNoTranscript
}

// Open returns a reader of one transcript. Its messages carry the harness's session id.
func Open(harness, path, id string) *transcript.Reader {
	cfg := transcript.Config{Path: path, Session: id}
	if harness == Codex {
		return transcript.NewCodex(cfg)
	}
	return transcript.NewClaude(cfg)
}

// readHead reads a transcript's first records for its id, folder, start and title.
func readHead(harness, path string) Entry {
	e := Entry{Harness: harness, Path: path}
	if harness == Claude {
		e.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return e
	}
	defer f.Close()
	sc := bufio.NewScanner(&limited{f, headBytes})
	sc.Buffer(make([]byte, 64<<10), headBytes)
	for sc.Scan() {
		if harness == Claude {
			claudeHead(&e, sc.Bytes())
		} else {
			codexHead(&e, sc.Bytes())
		}
		if e.Folder != "" && e.Title != "" && !e.Started.IsZero() && e.ID != "" {
			break
		}
	}
	return e
}

// limited reads at most n bytes.
type limited struct {
	f *os.File
	n int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.f.Read(p)
	l.n -= int64(n)
	return n, err
}

func claudeHead(e *Entry, line []byte) {
	var r struct {
		Type      string `json:"type"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
		IsMeta    bool   `json:"isMeta"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	if e.Folder == "" && r.Cwd != "" {
		e.Folder = r.Cwd
	}
	if e.Started.IsZero() && r.Timestamp != "" {
		e.Started, _ = time.Parse(time.RFC3339Nano, r.Timestamp)
	}
	if e.Title == "" && r.Type == "user" && !r.IsMeta && r.Message.Role == "user" {
		var s string
		if json.Unmarshal(r.Message.Content, &s) != nil {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(r.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						s = b.Text
						break
					}
				}
			}
		}
		e.Title = title(s)
	}
}

func codexHead(e *Entry, line []byte) {
	var r struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			ID      string `json:"id"`
			Cwd     string `json:"cwd"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Item struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	if e.Started.IsZero() && r.Timestamp != "" {
		e.Started, _ = time.Parse(time.RFC3339Nano, r.Timestamp)
	}
	switch {
	case r.Type == "session_meta":
		if e.ID == "" {
			e.ID = r.Payload.ID
		}
		if e.Folder == "" {
			e.Folder = r.Payload.Cwd
		}
	case r.Type == "turn_context" && e.Folder == "":
		e.Folder = r.Payload.Cwd
	case r.Type == "response_item" && r.Payload.Type == "message" && r.Payload.Role == "user" && e.Title == "":
		for _, c := range r.Payload.Content {
			if c.Type == "input_text" {
				if t := title(c.Text); t != "" {
					e.Title = t
					break
				}
			}
		}
	case r.Type == "event_msg" && r.Payload.Item.Type == "UserMessage" && e.Title == "":
		for _, c := range r.Payload.Item.Content {
			if c.Type == "text" {
				if t := title(c.Text); t != "" {
					e.Title = t
					break
				}
			}
		}
	}
}

// title is the first line of what the user typed, shortened; "" for text a harness added
// (context blocks in angle brackets, instruction files, caveats).
func title(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "# AGENTS.md") || strings.HasPrefix(s, "Caveat:") {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return cutRunes(s, titleChars)
}

// cutRunes shortens s to n runes, with an ellipsis when it cut.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// messageText is what a search matches in one message: its text, a tool's name, target and
// output, and a plan's steps.
func messageText(m api.ChatMessage) string {
	var b strings.Builder
	b.WriteString(m.Text)
	if t := m.Tool; t != nil {
		for _, s := range []string{t.Name, t.Target, t.Output} {
			if s != "" {
				b.WriteByte('\n')
				b.WriteString(s)
			}
		}
	}
	for _, p := range m.Plan {
		b.WriteByte('\n')
		b.WriteString(p.Text)
	}
	return b.String()
}
