package flightrules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/instructions"
)

// The Codex versions whose rollout shapes this reader was written against: record types and
// field names from the public source, checked against a rollout that Codex 0.169.0 wrote from
// hand-written files. A rollout from a version outside the range still reads; the ledger warns
// once.
const (
	CodexTestedMin = "0.166.0"
	CodexTestedMax = "0.169.0"
)

// Codex's markers and file names (codex-rs core/src/agents_md.rs and its context fragments).
const (
	codexAgentsHead     = "# AGENTS.md instructions"
	codexAgentsOpen     = "<INSTRUCTIONS>\n"
	codexAgentsClose    = "\n</INSTRUCTIONS>"
	codexReplaced       = "These AGENTS.md instructions replace all previously provided AGENTS.md instructions.\n\n"
	codexRemoved        = "The previously provided AGENTS.md instructions no longer apply."
	codexSkillsOpen     = "<skills_instructions>"
	codexSkillsClose    = "</skills_instructions>"
	codexProjectDocSep  = "\n\n--- project-doc ---\n\n"
	codexAgentsFile     = "AGENTS.md"
	codexAgentsOverride = "AGENTS.override.md"
)

// Block kinds: the parts of a rollout the ledger reads.
const (
	codexKindAgents = "agents_md"
	codexKindSkills = "skills"
	codexKindBase   = "base"
)

// codexUnknownSource is the source of AGENTS.md text that no file Flightdeck found holds.
const codexUnknownSource = "AGENTS.md instructions"

// Codex reads the ledger of a Codex session from its rollout, in place (spec 11.2, 11.4).
// Codex records the AGENTS.md text it sent as one block with no file names. The source runs
// Codex's discovery for the block's folder and finds each file's text inside the block:
// attribution matched. Text no file holds is one entry with attribution unknown.
type Codex struct {
	path string
	home string // Codex's home: the global AGENTS.md and config.toml
	t    tail
	r    codexReader
	last []string // the AGENTS.md sources of the newest block, to mark them gone when it goes

	mu    sync.Mutex
	spans map[string]codexSpan // by entry id: where its text sits in a block
}

// codexSpan is where an entry's text sits: a block, by its digest, and a byte range in it.
type codexSpan struct {
	block      string
	start, end int
}

// NewCodex returns a source that reads the Codex rollout at path. home is Codex's home folder
// ($CODEX_HOME, else ~/.codex). A file that does not exist yet reads as an empty ledger.
func NewCodex(path, home string) *Codex {
	return &Codex{path: path, home: home, t: tail{path: path, what: "rollout"}, spans: map[string]codexSpan{}}
}

// Harness implements Source.
func (c *Codex) Harness() string { return "codex" }

// Update implements Source: it reads the lines added since the last call.
func (c *Codex) Update(ctx context.Context, l *Ledger) error {
	reset := func() {
		c.r, c.last = codexReader{}, nil
		c.mu.Lock()
		c.spans = map[string]codexSpan{}
		c.mu.Unlock()
	}
	return c.t.read(ctx, l, reset, func(line []byte) {
		c.r.line(line, l.Warn, func(b codexBlock) { c.block(b, l) })
	})
}

// block turns one block into ledger entries.
func (c *Codex) block(b codexBlock, l *Ledger) {
	switch b.kind {
	case codexKindBase:
		c.observe(l, b, 0, len(b.text), Observation{Kind: api.FlightRuleSystem, Scope: api.FlightScopeSession,
			Source: "base instructions", At: b.at, Attribution: api.FlightRecorded})
	case codexKindSkills:
		kinds := []api.FlightRuleKind{api.FlightRuleSkills}
		l.Remove(Removal{Kinds: kinds, Source: "skill listing", At: b.at})
		if b.removed {
			return
		}
		c.observe(l, b, 0, len(b.text), Observation{Kind: api.FlightRuleSkills, Scope: api.FlightScopeSkill,
			Source: "skill listing", At: b.at, Attribution: api.FlightRecorded})
	case codexKindAgents:
		// A new block replaces the one before (Codex says so to the model), so every source
		// of the old block goes; a source the new block carries again comes back.
		for _, s := range c.last {
			l.Remove(Removal{Kinds: fileKinds, Source: s, At: b.at})
		}
		c.last = nil
		if b.removed {
			return
		}
		dir := b.dir
		if dir == "" {
			dir = c.r.cwd // a block from more than one environment names no folder
		}
		files, err := codexDiscover(c.home, dir)
		if err != nil {
			l.Warn("discover:"+err.Error(), "Codex's AGENTS.md discovery failed: %v", err)
		}
		// The renderer's Codex target is the global AGENTS.md. Unless an override file hides
		// it, a block that starts with a render header starts with that file.
		rendered := filepath.Join(c.home, codexAgentsFile)
		if _, err := os.Lstat(filepath.Join(c.home, codexAgentsOverride)); err == nil {
			rendered = ""
		}
		parts, missed := splitAgents(b.text, files, rendered)
		for _, p := range parts {
			o := Observation{Kind: api.FlightRuleInstruction, At: b.at}
			switch {
			case p.file != nil && p.header:
				o.Scope, o.Source, o.Attribution = p.file.scope, p.file.path, api.FlightMatched
				o.Note = "Codex recorded one block of AGENTS.md text. It starts with Flightdeck's render header, so this part is the rendered global file; the file holds another render now"
			case p.file != nil:
				o.Scope, o.Source, o.Attribution = p.file.scope, p.file.path, api.FlightMatched
				o.Note = "Codex recorded one block of AGENTS.md text; Flightdeck found this file's current text in it"
			default:
				o.Source, o.Attribution = codexUnknownSource, api.FlightUnknown
				o.Note = "Codex does not record which file this text came from, and no AGENTS.md file Flightdeck found holds it now"
				if len(missed) > 0 {
					o.Note += fmt.Sprintf(". Files Codex would load now whose text differs: %s", strings.Join(missed, ", "))
				}
			}
			c.observe(l, b, p.start, p.end, o)
			if !containsString(c.last, o.Source) {
				c.last = append(c.last, o.Source)
			}
		}
	}
}

// observe adds the observation of the text at b.text[start:end] and keeps where it sits.
func (c *Codex) observe(l *Ledger, b codexBlock, start, end int, o Observation) {
	o.Text = b.text[start:end]
	l.Observe(o)
	c.mu.Lock()
	c.spans[o.ID()] = codexSpan{block: Digest(b.text), start: start, end: end}
	c.mu.Unlock()
}

// Text implements Source: it reads the rollout again and returns the entry's part of the first
// block it came from.
func (c *Codex) Text(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	sp, ok := c.spans[id]
	c.mu.Unlock()
	if !ok {
		return "", ErrNoEntry
	}
	f, err := os.Open(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoEntry
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	var r codexReader
	var found *string
	nop := func(string, string, ...any) {}
	_, err = scanLines(ctx, f, 0, func(line []byte, _ int64) {
		if found != nil || line == nil {
			return
		}
		r.line(line, nop, func(b codexBlock) {
			if found == nil && !b.removed && sp.end <= len(b.text) && Digest(b.text) == sp.block {
				t := b.text[sp.start:sp.end]
				found = &t
			}
		})
	})
	if found != nil {
		return *found, nil
	}
	if err != nil {
		return "", err
	}
	return "", ErrNoEntry
}

// codexBlock is one piece of context a rollout carried: the AGENTS.md text, the skill listing or
// the base instructions. removed says the block left the context.
type codexBlock struct {
	kind    string
	text    string
	dir     string // AGENTS.md: the folder Codex loaded it for
	at      string
	removed bool
}

// codexReader turns rollout lines into blocks. Codex 0.166 and later write each block twice: as
// the message the model saw and in a world_state record; the reader reports a block once, when
// its text differs from the last one of its kind.
type codexReader struct {
	cwd   string
	state map[string]any    // the world_state sections the reader follows, patched in turn
	last  map[string]string // by block kind: the digest of the last block, "" once removed
}

// codexRecord is one rollout line.
type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSections are the world_state sections that hold text; the others hold hashes only.
var codexSections = []string{"agents_md", "host_skills"}

// line reads one rollout line and reports each new block.
func (r *codexReader) line(line []byte, warn func(key, format string, args ...any), emit func(codexBlock)) {
	if !bytes.Contains(line, []byte(`"session_meta"`)) && !bytes.Contains(line, []byte(`"world_state"`)) &&
		!bytes.Contains(line, []byte(`AGENTS.md instructions`)) && !bytes.Contains(line, []byte(codexSkillsOpen)) {
		return
	}
	var rec codexRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		warn("unreadable", "a rollout line could not be read: %v", err)
		return
	}
	if r.last == nil {
		r.last, r.state = map[string]string{}, map[string]any{}
	}
	send := func(b codexBlock) {
		b.at = rec.Timestamp
		if b.removed {
			if r.last[b.kind] == "" {
				return
			}
			r.last[b.kind] = ""
			emit(b)
			return
		}
		d := Digest(b.text)
		if r.last[b.kind] == d {
			return
		}
		r.last[b.kind] = d
		emit(b)
	}
	switch rec.Type {
	case "session_meta":
		var m struct {
			Cwd     string `json:"cwd"`
			Version string `json:"cli_version"`
			Base    struct {
				Text string `json:"text"`
			} `json:"base_instructions"`
		}
		if json.Unmarshal(rec.Payload, &m) != nil {
			warn("session_meta", "a session_meta record has an unknown shape")
			return
		}
		if r.cwd == "" {
			r.cwd = m.Cwd
		}
		if m.Version != "" {
			checkVersion("Codex", m.Version, CodexTestedMin, CodexTestedMax, warn)
		}
		if m.Base.Text != "" {
			send(codexBlock{kind: codexKindBase, text: m.Base.Text})
		}
	case "world_state":
		var w struct {
			Full  bool                       `json:"full"`
			State map[string]json.RawMessage `json:"state"`
		}
		if json.Unmarshal(rec.Payload, &w) != nil {
			warn("world_state", "a world_state record has an unknown shape")
			return
		}
		for _, k := range codexSections {
			raw, ok := w.State[k]
			if !ok {
				if w.Full {
					delete(r.state, k)
				}
				continue
			}
			var v any
			if json.Unmarshal(raw, &v) != nil {
				continue
			}
			if w.Full {
				r.state[k] = v
			} else {
				r.state[k] = mergePatch(r.state[k], v)
			}
		}
		agents, _ := r.state["agents_md"].(map[string]any)
		text, _ := agents["text"].(string)
		dir, _ := agents["directory"].(string)
		if text != "" {
			send(codexBlock{kind: codexKindAgents, text: text, dir: dir})
		} else if _, ok := r.state["agents_md"]; ok || w.Full {
			send(codexBlock{kind: codexKindAgents, removed: true})
		}
		skills, _ := r.state["host_skills"].(map[string]any)
		if body, _ := skills["body"].(string); body != "" {
			send(codexBlock{kind: codexKindSkills, text: body})
		} else if _, ok := r.state["host_skills"]; ok || w.Full {
			send(codexBlock{kind: codexKindSkills, removed: true})
		}
	case "response_item":
		var m struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(rec.Payload, &m) != nil || m.Type != "message" {
			return
		}
		for _, c := range m.Content {
			switch {
			case m.Role == "user" && strings.HasPrefix(c.Text, codexAgentsHead):
				if b, ok := parseAgentsMessage(c.Text); ok {
					send(b)
				}
			case m.Role == "developer" && strings.HasPrefix(c.Text, codexSkillsOpen) && strings.HasSuffix(c.Text, codexSkillsClose):
				body := strings.TrimSuffix(strings.TrimPrefix(c.Text, codexSkillsOpen), codexSkillsClose)
				if body != "" {
					send(codexBlock{kind: codexKindSkills, text: body})
				}
			}
		}
	}
}

// parseAgentsMessage reads the AGENTS.md message: "# AGENTS.md instructions[ for <dir>]", a
// blank line, then the text between <INSTRUCTIONS> markers.
func parseAgentsMessage(s string) (codexBlock, bool) {
	head, body, ok := strings.Cut(s, "\n\n")
	if !ok || !strings.HasPrefix(body, codexAgentsOpen) || !strings.HasSuffix(body, codexAgentsClose) {
		return codexBlock{}, false
	}
	b := codexBlock{kind: codexKindAgents}
	b.dir = strings.TrimPrefix(strings.TrimPrefix(head, codexAgentsHead), " for ")
	b.text = strings.TrimSuffix(strings.TrimPrefix(body, codexAgentsOpen), codexAgentsClose)
	if b.text == codexRemoved {
		return codexBlock{kind: codexKindAgents, removed: true}, true
	}
	b.text = strings.TrimPrefix(b.text, codexReplaced)
	return b, b.text != ""
}

// mergePatch applies an RFC 7386 merge patch, as Codex writes world_state patches.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
			continue
		}
		t[k] = mergePatch(t[k], v)
	}
	return t
}

// codexFile is one AGENTS.md file Codex's discovery finds, with the text it would send.
type codexFile struct {
	path  string
	scope api.FlightRuleScope
	text  string
}

// codexDiscover runs Codex's AGENTS.md discovery as it stands now: the global file in home
// (AGENTS.override.md, else AGENTS.md, trimmed), then the first AGENTS.override.md, AGENTS.md
// or fallback name in each folder from the project root down to dir. The project root is the
// nearest folder with a root marker (.git unless config.toml sets project_root_markers); with
// none, dir alone. dir "" finds the global file only.
func codexDiscover(home, dir string) ([]codexFile, error) {
	var out []codexFile
	for _, name := range []string{codexAgentsOverride, codexAgentsFile} {
		p := filepath.Join(home, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if t := strings.TrimSpace(string(data)); t != "" {
			out = append(out, codexFile{path: p, scope: api.FlightScopeUser, text: t})
			break
		}
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return out, nil
	}
	var cfg struct {
		Markers   *[]string `toml:"project_root_markers"`
		Fallbacks []string  `toml:"project_doc_fallback_filenames"`
	}
	var cfgErr error
	if data, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		if _, err := toml.Decode(string(data), &cfg); err != nil {
			cfgErr = fmt.Errorf("%s: %w", filepath.Join(home, "config.toml"), err)
			cfg.Markers, cfg.Fallbacks = nil, nil
		}
	}
	markers := []string{".git"}
	if cfg.Markers != nil {
		markers = *cfg.Markers
	}
	dirs := []string{dir}
	if root := codexRoot(dir, markers); root != "" {
		dirs = nil
		for d := dir; ; d = filepath.Dir(d) {
			dirs = append([]string{d}, dirs...)
			if d == root || filepath.Dir(d) == d {
				break
			}
		}
	}
	names := []string{codexAgentsOverride, codexAgentsFile}
	for _, n := range cfg.Fallbacks {
		if n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00") && !containsString(names, n) {
			names = append(names, n)
		}
	}
	for _, d := range dirs {
		for _, n := range names {
			p := filepath.Join(d, n)
			if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
				continue
			}
			data, err := os.ReadFile(p)
			if err == nil && strings.TrimSpace(string(data)) != "" {
				out = append(out, codexFile{path: p, scope: api.FlightScopeProject, text: string(data)})
			}
			break
		}
	}
	return out, cfgErr
}

// codexRoot is the nearest folder from dir up that holds one of the markers, or "".
func codexRoot(dir string, markers []string) string {
	for d := dir; ; d = filepath.Dir(d) {
		for _, m := range markers {
			if _, err := os.Lstat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// agentsPart is one part of an AGENTS.md block: a file's text, or text no file holds (file nil).
// header marks the rendered global file, known by its render header rather than its text.
type agentsPart struct {
	start, end int
	file       *codexFile
	header     bool
}

// splitAgents finds each file's text in the block, in discovery order, where a part may start:
// at the block's start or after a blank line, and end at its end or before one. The rest of
// the block, without Codex's separators, is parts with no file. missed names the files whose
// text is not in the block.
//
// rendered, when not "", is the path of the global file Flightdeck renders. When no file
// matched at the block's start and the block starts with a render header, the block's first
// part (up to Codex's project-doc separator) is that file: Codex puts the global file first,
// and the header proves a render.
func splitAgents(block string, files []codexFile, rendered string) (parts []agentsPart, missed []string) {
	var found []agentsPart
	matched := map[string]bool{}
	cur := 0
	for i := range files {
		f := &files[i]
		at := findPart(block, f.text, cur)
		if at < 0 {
			continue
		}
		found = append(found, agentsPart{start: at, end: at + len(f.text), file: f})
		matched[f.path] = true
		cur = at + len(f.text)
	}
	if rendered != "" && !matched[rendered] && instructions.HeaderDigest(block) != "" {
		end := strings.Index(block, codexProjectDocSep)
		if end < 0 {
			end = len(block)
		}
		if len(found) == 0 || found[0].start >= end {
			g := &codexFile{path: rendered, scope: api.FlightScopeUser}
			found = append([]agentsPart{{start: 0, end: end, file: g, header: true}}, found...)
			matched[rendered] = true
		}
	}
	for _, f := range files {
		if !matched[f.path] {
			missed = append(missed, f.path)
		}
	}
	gap := func(a, b int) {
		for {
			switch {
			case strings.HasPrefix(block[a:b], codexProjectDocSep):
				a += len(codexProjectDocSep)
			case strings.HasPrefix(block[a:b], "\n\n"):
				a += 2
			default:
				goto tail
			}
		}
	tail:
		for {
			switch {
			case strings.HasSuffix(block[a:b], codexProjectDocSep):
				b -= len(codexProjectDocSep)
			case strings.HasSuffix(block[a:b], "\n\n"):
				b -= 2
			default:
				if strings.TrimSpace(block[a:b]) != "" {
					parts = append(parts, agentsPart{start: a, end: b})
				}
				return
			}
		}
	}
	prev := 0
	for _, p := range found {
		gap(prev, p.start)
		parts = append(parts, p)
		prev = p.end
	}
	gap(prev, len(block))
	return parts, missed
}

// findPart returns where text sits in block at or after from, on part boundaries, or -1.
func findPart(block, text string, from int) int {
	if text == "" {
		return -1
	}
	for from <= len(block) {
		i := strings.Index(block[from:], text)
		if i < 0 {
			return -1
		}
		at := from + i
		end := at + len(text)
		if (at == 0 || strings.HasSuffix(block[:at], "\n\n")) && (end == len(block) || strings.HasPrefix(block[end:], "\n\n")) {
			return at
		}
		from = at + 1
	}
	return -1
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
