package transcript

// Conversations started outside Flightdeck (gff-client.md 6.5): find one by its resume id, read
// the folder it ran in and the first line the owner typed, and list the newest. Everything here
// reads; nothing writes, moves or copies a transcript.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Harness names, as the detection manifests have them.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// outsideHead bounds how much of a transcript the conversation readers look at.
const outsideHead = 256 << 10

// titleRunes bounds a conversation's title.
const titleRunes = 80

// Conversation is one harness conversation on disk.
type Conversation struct {
	Harness string
	ID      string
	// Cwd is the folder the harness ran in, "" when the transcript names none.
	Cwd string
	// Title is the first line the owner typed, cut to 80 characters, "" when none was read.
	Title   string
	Updated time.Time
	Path    string
	// SDK marks a Claude Code transcript that claude -p wrote.
	SDK bool
}

// ValidID reports whether id can name a transcript: letters, digits, - and _.
func ValidID(id string) bool { return claudeSessionID.MatchString(id) }

// ClaudeProjectDir is the name Claude Code gives the transcript folder of a working folder:
// each character other than an ASCII letter or digit becomes -.
func ClaudeProjectDir(cwd string) string {
	b := []byte(cwd)
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

// ClaudeConversation finds a Claude Code conversation by id under the config folder.
// ErrNoTranscript when none holds it.
func ClaudeConversation(configDir, id string) (Conversation, error) {
	path, err := FindClaude(configDir, id)
	if err != nil {
		return Conversation{}, err
	}
	return readClaude(path, id)
}

// CodexConversation finds a Codex conversation by id under the Codex home. ErrNoTranscript
// when none holds it.
func CodexConversation(home, id string) (Conversation, error) {
	path, err := FindCodex(home, id)
	if err != nil {
		return Conversation{}, err
	}
	c, err := readCodex(path)
	if err == nil && c.ID == "" {
		c.ID = id
	}
	return c, err
}

// RecentClaude lists the newest Claude Code conversations by last write, at most limit, with
// the ones claude -p wrote left out.
func RecentClaude(configDir string, limit int) ([]Conversation, error) {
	return recent(filepath.Join(configDir, "projects", "*", "*.jsonl"), limit, func(path string) (Conversation, bool) {
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if !ValidID(id) {
			return Conversation{}, false
		}
		c, err := readClaude(path, id)
		return c, err == nil && !c.SDK && c.Cwd != ""
	})
}

// RecentCodex lists the newest Codex conversations by last write, at most limit.
func RecentCodex(home string, limit int) ([]Conversation, error) {
	return recent(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*.jsonl"), limit, func(path string) (Conversation, bool) {
		c, err := readCodex(path)
		return c, err == nil && c.ID != "" && ValidID(c.ID)
	})
}

// recent reads the files that match pattern, newest first, until limit of them read.
func recent(pattern string, limit int, read func(string) (Conversation, bool)) ([]Conversation, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	type file struct {
		path string
		mod  time.Time
	}
	files := make([]file, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		files = append(files, file{m, fi.ModTime()})
	}
	slices.SortFunc(files, func(a, b file) int { return b.mod.Compare(a.mod) })
	var out []Conversation
	for _, f := range files {
		if len(out) >= limit {
			break
		}
		if c, ok := read(f.path); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// head opens path and returns its first outsideHead bytes as whole lines, and its last write.
func head(path string) ([][]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	sc := bufio.NewScanner(io.LimitReader(f, outsideHead))
	sc.Buffer(make([]byte, 0, 64<<10), outsideHead)
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, bytes.Clone(sc.Bytes()))
	}
	// A line cut by the limit fails to scan or to decode; the lines before it stand.
	return lines, fi.ModTime(), nil
}

// readClaude reads a Claude Code transcript's folder, title and kind.
func readClaude(path, id string) (Conversation, error) {
	lines, mod, err := head(path)
	if err != nil {
		return Conversation{}, err
	}
	c := Conversation{Harness: HarnessClaude, ID: id, Updated: mod, Path: path}
	dir := filepath.Base(filepath.Dir(path))
	for _, l := range lines {
		var rec struct {
			Type       string          `json:"type"`
			Cwd        string          `json:"cwd"`
			Entrypoint string          `json:"entrypoint"`
			IsMeta     bool            `json:"isMeta"`
			Sidechain  bool            `json:"isSidechain"`
			Message    json.RawMessage `json:"message"`
		}
		if json.Unmarshal(l, &rec) != nil {
			continue
		}
		// The cwd that names the transcript's folder is where --resume finds it; else the
		// first one seen.
		if rec.Cwd != "" && (c.Cwd == "" || ClaudeProjectDir(c.Cwd) != dir && ClaudeProjectDir(rec.Cwd) == dir) {
			c.Cwd = rec.Cwd
		}
		if strings.HasPrefix(rec.Entrypoint, "sdk") {
			c.SDK = true
		}
		if c.Title == "" && rec.Type == "user" && !rec.IsMeta && !rec.Sidechain {
			c.Title = claudeTyped(rec.Message)
		}
	}
	return c, nil
}

// claudeTyped returns the text of a user message the owner typed, or "".
func claudeTyped(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return title(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" {
			if t := title(b.Text); t != "" {
				return t
			}
		}
	}
	return ""
}

// readCodex reads a Codex rollout's id, folder and title.
func readCodex(path string) (Conversation, error) {
	lines, mod, err := head(path)
	if err != nil {
		return Conversation{}, err
	}
	c := Conversation{Harness: HarnessCodex, Updated: mod, Path: path}
	for _, l := range lines {
		var rec struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(l, &rec) != nil {
			continue
		}
		switch rec.Type {
		case "session_meta":
			var p struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(rec.Payload, &p) == nil && c.ID == "" {
				c.ID, c.Cwd = p.ID, p.Cwd
			}
		case "response_item":
			if c.Title != "" {
				continue
			}
			var p struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(rec.Payload, &p) != nil || p.Type != "message" || p.Role != "user" {
				continue
			}
			for _, b := range p.Content {
				if b.Type == "input_text" {
					if t := title(b.Text); t != "" {
						c.Title = t
						break
					}
				}
			}
		}
	}
	if c.ID == "" && c.Cwd == "" {
		return c, errors.New("transcript: no session_meta line")
	}
	return c, nil
}

// title is the first non-empty line of text the owner typed, cut to 80 characters. Text that
// starts with < is a wrapper the harness added (a command, the environment), not typed.
func title(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > titleRunes {
		r := []rune(line)
		line = string(r[:titleRunes-1]) + "…"
	}
	return line
}

// ClaudeRunning is one entry of Claude Code's registry of running sessions:
// <config>/sessions/<pid>.json.
type ClaudeRunning struct {
	PID       int
	SessionID string
	// ProcStart is the process start Claude Code recorded for the pid, as it wrote it: on
	// macOS ps's lstart in UTC, on Linux the start tick of /proc/<pid>/stat. "" when absent.
	ProcStart string
}

// ClaudeRegistry reads Claude Code's registry of running sessions. Entries that do not read
// are skipped; whether each pid is alive is the caller's question.
func ClaudeRegistry(configDir string) []ClaudeRunning {
	matches, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*.json"))
	var out []ClaudeRunning
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil || len(b) > 1<<20 {
			continue
		}
		var e struct {
			PID       int             `json:"pid"`
			SessionID string          `json:"sessionId"`
			ProcStart json.RawMessage `json:"procStart"`
		}
		if json.Unmarshal(b, &e) == nil && e.PID > 0 && e.SessionID != "" {
			out = append(out, ClaudeRunning{PID: e.PID, SessionID: e.SessionID, ProcStart: procStartText(e.ProcStart)})
		}
	}
	return out
}

// procStartText reads a registry entry's procStart: a string, or a number written as one.
// Anything else reads as absent.
func procStartText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
