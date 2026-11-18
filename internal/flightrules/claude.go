package flightrules

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// The Claude Code versions whose attachment shapes this reader was written against. A record
// from a version outside the range still reads; the ledger warns once.
const (
	ClaudeTestedMin = "2.1.310"
	ClaudeTestedMax = "2.1.327"
)

// maxLine bounds one transcript line. A longer line is skipped with a warning.
var maxLine = 32 << 20

// claudeIgnored lists attachment types that carry conversation, tool or UI state rather than
// context the harness loaded: files the user mentioned, queued input, reminders, hook results
// that add no text. Every other unknown type becomes a placeholder entry.
var claudeIgnored = map[string]bool{
	"file": true, "directory": true, "already_read_file": true, "compact_file_reference": true,
	"pdf_reference": true, "plan_file_reference": true, "at_mention_reference": true,
	"mcp_resource": true, "selected_lines_in_ide": true, "selected_lines_in_diff": true,
	"queued_command": true, "todo_reminder": true, "task_reminder": true, "task_status": true,
	"diagnostics": true, "plan_mode": true, "plan_mode_reentry": true, "goal_status": true,
	"hook_success": true, "hook_blocking_error": true, "hook_non_blocking_error": true,
	"hook_cancelled": true, "hook_error_during_execution": true, "hook_stopped_continuation": true,
	"hook_permission_decision": true, "hook_deferred_tool": true, "async_hook_response": true,
	"async_hook_response_batch": true, "teammate_shutdown_batch": true, "agent_ask_notice": true,
}

// Claude reads the ledger of a Claude Code session from its transcript, in place: the
// attachment records that carry instruction files, nested memory, skill listings, invoked
// skills, MCP server instructions and hook context (spec 10, 11.2).
type Claude struct {
	path string
	info fs.FileInfo // the file read so far, to see a replacement
	off  int64       // bytes read: the end of the last complete line
}

// NewClaude returns a source that reads the Claude Code transcript at path. A file that does not
// exist yet reads as an empty ledger.
func NewClaude(path string) *Claude { return &Claude{path: path} }

// Harness implements Source.
func (c *Claude) Harness() string { return "claude" }

// Update implements Source: it reads the lines added since the last call.
func (c *Claude) Update(ctx context.Context, l *Ledger) error {
	f, err := os.Open(c.path)
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
	if c.info != nil && (!os.SameFile(c.info, info) || info.Size() < c.off) {
		l.Reset()
		c.off = 0
		l.Warn("replaced", "the transcript was replaced or cut; the ledger was read again from the start")
	}
	c.info = info
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return err
	}
	n, err := scanLines(ctx, f, c.off, func(line []byte, end int64) {
		if line == nil {
			l.Warn("oversize:"+strconv.FormatInt(end, 10), "a transcript line ending at byte %d is longer than %d bytes and was skipped", end, maxLine)
			return
		}
		claudeLine(line, l.Warn, l.Observe, l.Remove)
	})
	c.off = n
	return err
}

// Text implements Source: it reads the transcript again and returns the first text whose entry
// id is id.
func (c *Claude) Text(ctx context.Context, id string) (string, error) {
	f, err := os.Open(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoEntry
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	var found *string
	nop := func(string, string, ...any) {}
	_, err = scanLines(ctx, f, 0, func(line []byte, _ int64) {
		if found != nil || line == nil {
			return
		}
		claudeLine(line, nop, func(o Observation) {
			if found == nil && !o.Placeholder && o.ID() == id {
				t := o.Text
				found = &t
			}
		}, func(Removal) {})
	})
	if found != nil {
		return *found, nil
	}
	if err != nil {
		return "", err
	}
	return "", ErrNoEntry
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

// claudeRecord is the part of a transcript record the ledger reads.
type claudeRecord struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	Version     string          `json:"version"`
	IsSidechain bool            `json:"isSidechain"`
	Attachment  json.RawMessage `json:"attachment"`
}

// claudeFile is one file of an instructions attachment, or the content of a nested_memory one.
type claudeFile struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Content    string `json:"content"`
	StartOnly  bool   `json:"startOnly"`
	CutForRoom bool   `json:"cutForRoom"`
}

type claudeAttachment struct {
	Type string `json:"type"`
	// instructions
	Files   []claudeFile `json:"files"`
	Removed []string     `json:"removed"`
	Reason  string       `json:"reason"`
	// instructions from before the per-file shape, nested_memory, skill_listing,
	// hook_additional_context: a string, an object or a list of strings
	Content json.RawMessage `json:"content"`
	// nested_memory
	Path string `json:"path"`
	// skill_listing
	SkillCount int  `json:"skillCount"`
	Shortened  bool `json:"shortened"`
	// invoked_skills
	Skills []struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"skills"`
	// mcp_instructions_delta
	AddedNames   []string          `json:"addedNames"`
	AddedBlocks  []json.RawMessage `json:"addedBlocks"`
	RemovedNames []string          `json:"removedNames"`
	// hook_additional_context
	HookName  string `json:"hookName"`
	HookEvent string `json:"hookEvent"`
}

// claudeScope maps an instruction file's type to its kind and scope.
func claudeScope(t string) (api.FlightRuleKind, api.FlightRuleScope, bool) {
	switch t {
	case "User":
		return api.FlightRuleInstruction, api.FlightScopeUser, true
	case "Project":
		return api.FlightRuleInstruction, api.FlightScopeProject, true
	case "Local":
		return api.FlightRuleInstruction, api.FlightScopeLocal, true
	case "Managed":
		return api.FlightRuleInstruction, api.FlightScopeManaged, true
	case "AutoMem", "AutoMemPinned":
		return api.FlightRuleMemory, api.FlightScopeMemory, true
	}
	return api.FlightRuleInstruction, "", false
}

var fileKinds = []api.FlightRuleKind{api.FlightRuleInstruction, api.FlightRuleMemory}

// claudeLine reads one transcript line and reports what it carried.
func claudeLine(line []byte, warn func(key, format string, args ...any), observe func(Observation), remove func(Removal)) {
	if !bytes.Contains(line, []byte(`"attachment"`)) {
		return
	}
	var rec claudeRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		warn("unreadable", "a transcript line could not be read: %v", err)
		return
	}
	if rec.Type != "attachment" || len(rec.Attachment) == 0 {
		return
	}
	if rec.IsSidechain {
		return // a subagent's context, not this session's
	}
	if rec.Version != "" {
		checkClaudeVersion(rec.Version, warn)
	}
	var a claudeAttachment
	if err := json.Unmarshal(rec.Attachment, &a); err != nil {
		warn("attachment:"+a.Type, "an attachment record of type %q has an unknown shape: %v", a.Type, err)
		observe(placeholder(attachmentType(rec.Attachment), rec.Timestamp))
		return
	}
	at := rec.Timestamp
	switch a.Type {
	case "instructions":
		if a.Files == nil && len(a.Content) > 0 {
			// An older shape: the text with no file. The source cannot be proven.
			text := contentText(a.Content)
			observe(Observation{Kind: api.FlightRuleInstruction, Source: "instructions", Text: text, At: at,
				Reason: a.Reason, Attribution: api.FlightUnknown,
				Note: "This Claude Code version recorded instruction text without naming its files"})
			return
		}
		for _, f := range a.Files {
			kind, scope, ok := claudeScope(f.Type)
			o := Observation{Kind: kind, Scope: scope, Source: f.Path, Text: f.Content, At: at, Reason: a.Reason,
				Attribution: api.FlightRecorded, Note: fileNote(f)}
			if !ok {
				o.Note = joinNote(fmt.Sprintf("Claude Code gave the file type %q, which Flightdeck does not know", f.Type), o.Note)
			}
			observe(o)
		}
		for _, p := range a.Removed {
			remove(Removal{Kinds: fileKinds, Source: p, At: at})
		}
	case "nested_memory":
		f := claudeFile{Path: a.Path}
		if len(a.Content) > 0 && a.Content[0] == '{' {
			_ = json.Unmarshal(a.Content, &f)
		} else {
			f.Content = contentText(a.Content)
		}
		if f.Path == "" {
			f.Path = a.Path
		}
		kind, scope, _ := claudeScope(f.Type)
		observe(Observation{Kind: kind, Scope: scope, Source: f.Path, Text: f.Content, At: at,
			Attribution: api.FlightRecorded,
			Note:        joinNote("Loaded when the agent worked in this file's folder", fileNote(f))})
	case "skill_listing":
		note := fmt.Sprintf("%d skills", a.SkillCount)
		if a.Shortened {
			note += "; shortened to fit"
		}
		observe(Observation{Kind: api.FlightRuleSkills, Scope: api.FlightScopeSkill, Source: "skill listing",
			Text: contentText(a.Content), At: at, Attribution: api.FlightRecorded, Note: note})
	case "invoked_skills":
		for _, s := range a.Skills {
			observe(Observation{Kind: api.FlightRuleSkill, Scope: api.FlightScopeSkill, Source: s.Name, Path: s.Path,
				Text: s.Content, At: at, Attribution: api.FlightRecorded})
		}
	case "mcp_instructions_delta":
		for i, name := range a.AddedNames {
			var block string
			if i < len(a.AddedBlocks) && json.Unmarshal(a.AddedBlocks[i], &block) == nil {
				observe(Observation{Kind: api.FlightRuleMCP, Scope: api.FlightScopeMCP, Source: name, Text: block,
					At: at, Attribution: api.FlightRecorded})
				continue
			}
			observe(Observation{Kind: api.FlightRuleMCP, Scope: api.FlightScopeMCP, Source: name, Placeholder: true,
				At: at, Note: "Claude Code recorded this server's instructions in a shape Flightdeck does not read"})
		}
		for _, name := range a.RemovedNames {
			remove(Removal{Kinds: []api.FlightRuleKind{api.FlightRuleMCP}, Source: name, At: at})
		}
	case "hook_additional_context":
		name := a.HookName
		if name == "" {
			name = a.HookEvent
		}
		o := Observation{Kind: api.FlightRuleHook, Scope: api.FlightScopeSession, Source: name, Event: a.HookEvent,
			Text: contentText(a.Content), At: at, Attribution: api.FlightRecorded}
		if name == "" {
			o.Source, o.Attribution = "hook", api.FlightUnknown
			o.Note = "Claude Code recorded hook context without the hook's name or event"
		}
		observe(o)
	default:
		if claudeIgnored[a.Type] {
			return
		}
		warn("attachment:"+a.Type, "Claude Code recorded %q attachments; the ledger shows them as placeholders", a.Type)
		observe(placeholder(a.Type, at))
	}
}

// placeholder is the entry of an attachment type the ledger does not read.
func placeholder(typ, at string) Observation {
	if typ == "" {
		typ = "unknown"
	}
	return Observation{Kind: api.FlightRuleSystem, Scope: api.FlightScopeSession, Source: "attachment " + typ,
		Placeholder: true, At: at, Attribution: api.FlightUnknown,
		Note: fmt.Sprintf("Claude Code recorded a %s attachment, which Flightdeck does not read yet", typ)}
}

// attachmentType reads only the type of an attachment whose other fields did not decode.
func attachmentType(raw json.RawMessage) string {
	var t struct {
		Type any `json:"type"`
	}
	_ = json.Unmarshal(raw, &t)
	if s, ok := t.Type.(string); ok {
		return s
	}
	return ""
}

// contentText reads a content field that is a string, or a list of strings joined by blank
// lines. Anything else reads as its JSON text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, "\n\n")
	}
	return string(raw)
}

func fileNote(f claudeFile) string {
	switch {
	case f.StartOnly:
		return "Only the start of the file reached the agent"
	case f.CutForRoom:
		return "Cut to fit beside the session's other instruction files"
	}
	return ""
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + ". " + b
}

// checkClaudeVersion warns once per version outside the tested range.
func checkClaudeVersion(v string, warn func(key, format string, args ...any)) {
	lo, ok1 := compareVersions(v, ClaudeTestedMin)
	hi, ok2 := compareVersions(v, ClaudeTestedMax)
	switch {
	case !ok1 || !ok2:
		warn("version:"+v, "Claude Code version %q is unreadable; reading its records in tolerant mode", v)
	case lo < 0 || hi > 0:
		warn("version:"+v, "Claude Code %s is outside the tested range %s to %s; reading its records in tolerant mode", v, ClaudeTestedMin, ClaudeTestedMax)
	}
}

// compareVersions compares two dotted numeric versions. ok is false when either is not one.
func compareVersions(a, b string) (int, bool) {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		var err error
		if i < len(pa) {
			if x, err = strconv.Atoi(pa[i]); err != nil {
				return 0, false
			}
		}
		if i < len(pb) {
			if y, err = strconv.Atoi(pb[i]); err != nil {
				return 0, false
			}
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}
