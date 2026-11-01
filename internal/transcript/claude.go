package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
)

// The Claude Code versions whose transcript shapes the parser was written and tested against.
// The format is undocumented; a version outside this range still reads, and the reader reports
// drift (harness-adapters.md, Edge cases: transcript format drift).
const (
	ClaudeTestedMin = "2.1.296"
	ClaudeTestedMax = "2.1.316"
)

// Limits on what one chat message carries (api.ChatTool).
const (
	maxToolInput  = 64 << 10
	maxToolOutput = 4 << 10
	maxTarget     = 200
)

// claudeSkipped lists the record types the parser knows and leaves out of the chat view. They
// carry context (attachment: the context ledger reads them later), titles, modes, request
// bookkeeping and file history. Every other unknown type becomes a placeholder message.
var claudeSkipped = map[string]bool{
	"attachment": true, "summary": true, "custom-title": true, "ai-title": true,
	"last-prompt": true, "mode": true, "permission-mode": true, "queue-operation": true,
	"file-history-snapshot": true, "file-history-delta": true, "api-request": true,
	"api-request-blob": true, "api-request-shape": true, "bridge-session": true,
	"atis-latch": true, "frame-link": true, "tools-used": true, "pr-link": true,
	"cost-state": true, "artifact-autoreact-ledger": true, "artifact-comment-monitor": true,
	"tag": true, "agent-name": true,
}

// claudeSystemSkipped lists system subtypes that are bookkeeping, not conversation.
var claudeSystemSkipped = map[string]bool{
	"turn_duration": true, "stop_hook_summary": true, "scheduled_task_fire": true,
}

// claudeRecord is the part of a Claude Code transcript record the parser reads.
type claudeRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	Timestamp        string          `json:"timestamp"`
	Version          string          `json:"version"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Content          json.RawMessage `json:"content"` // system records
	Level            string          `json:"level"`
	Message          *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	CompactMetadata *struct {
		Trigger   string `json:"trigger"`
		PreTokens int    `json:"preTokens"`
	} `json:"compactMetadata"`
	PermissionDecision *struct {
		Decision   string `json:"decision"`
		Source     string `json:"source"`
		GrantScope string `json:"grantScope"`
	} `json:"permissionDecision"`
}

// claudeBlock is one content block of a user or assistant message.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeParser turns Claude Code records into chat messages.
type claudeParser struct {
	c       *conversation
	at      string            // the last timestamp seen, for records that carry none
	tools   map[string]string // tool_use id to message id
	r       *Reader
	changed func(*item)
}

func newClaudeParser(r *Reader) lineParser {
	return &claudeParser{c: r.c, tools: map[string]string{}, r: r, changed: r.changed}
}

func (p *claudeParser) emit(msg api.ChatMessage, end int64) {
	if msg.At == "" {
		msg.At = p.at
	}
	p.changed(p.c.put(msg, end))
}

func (p *claudeParser) placeholder(id, at, text string, end int64) {
	p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindEvent, Event: api.ChatEventUnknown, At: at, Text: text}, end)
}

// line handles one complete transcript line that ends at offset end.
func (p *claudeParser) line(data []byte, end int64) {
	lineID := "line-" + strconv.FormatInt(end, 10)
	if data == nil {
		p.r.warn("oversize", "transcript line ending at byte %d is longer than %d bytes; shown as a placeholder", end, maxLine)
		p.placeholder(lineID, "", "A transcript record too large to read", end)
		return
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}
	var rec claudeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		p.r.warn("unreadable", "unreadable transcript line ending at byte %d: %v", end, err)
		p.placeholder(lineID, "", "An unreadable transcript record", end)
		return
	}
	if rec.Timestamp != "" {
		p.at = rec.Timestamp
	}
	if rec.Version != "" {
		p.r.checkVersion(rec.Version)
	}
	id := rec.UUID
	if id == "" {
		id = lineID
	}
	switch {
	case rec.IsSidechain:
		// A subagent's records: a later chat-view feature (spec, Claude Code notes).
	case rec.Type == "user":
		p.user(&rec, id, end)
	case rec.Type == "assistant":
		p.assistant(&rec, id, end)
	case rec.Type == "system":
		p.system(&rec, id, end)
	case claudeSkipped[rec.Type]:
	default:
		p.r.warn("type:"+rec.Type, "unknown Claude Code record type %q (version %q); shown as a placeholder", rec.Type, rec.Version)
		p.placeholder(id, rec.Timestamp, fmt.Sprintf("Unsupported Claude Code record: %s", rec.Type), end)
	}
}

var (
	commandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	commandArgs = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
	commandOut  = regexp.MustCompile(`(?s)<local-command-(?:stdout|stderr)>(.*?)</local-command-(?:stdout|stderr)>`)
)

// userText classifies the text of a user record: the owner's words, a slash command, its local
// output, an interrupt, or meta text that stays out of the chat view.
func userText(text string) (kind api.ChatKind, event api.ChatEvent, out string, keep bool) {
	t := strings.TrimSpace(text)
	switch {
	case t == "":
		return "", "", "", false
	case strings.HasPrefix(t, "[Request interrupted by user"):
		return api.ChatKindEvent, api.ChatEventInterrupt, "Interrupted by the owner", true
	case strings.HasPrefix(t, "<local-command-caveat>"):
		return "", "", "", false
	case commandName.MatchString(t):
		cmd := strings.TrimSpace(commandName.FindStringSubmatch(t)[1])
		if m := commandArgs.FindStringSubmatch(t); m != nil && strings.TrimSpace(m[1]) != "" {
			cmd += " " + strings.TrimSpace(m[1])
		}
		return api.ChatKindEvent, api.ChatEventCommand, cmd, true
	case commandOut.MatchString(t):
		return api.ChatKindEvent, api.ChatEventCommand, strings.TrimSpace(commandOut.FindStringSubmatch(t)[1]), true
	}
	return api.ChatKindUser, "", t, true
}

func (p *claudeParser) user(rec *claudeRecord, id string, end int64) {
	if rec.IsMeta || rec.IsCompactSummary {
		return // context the model sees, not the owner's words; the boundary marks a compaction
	}
	if rec.Message == nil {
		p.r.warn("user-message", "user record %s has no message; shown as a placeholder", id)
		p.placeholder(id, rec.Timestamp, "A user message in an unknown shape", end)
		return
	}
	raw := rec.Message.Content
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if kind, ev, text, ok := userText(s); ok {
			p.emit(api.ChatMessage{ID: id, Kind: kind, Event: ev, At: rec.Timestamp, Text: text}, end)
		}
		return
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		p.r.warn("user-content", "user record %s: content is neither text nor blocks; shown as a placeholder", id)
		p.placeholder(id, rec.Timestamp, "A user message in an unknown shape", end)
		return
	}
	var texts []string
	for i, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			texts = append(texts, "[image]")
		case "tool_result":
			p.toolResult(rec, id, b, end)
		default:
			p.r.warn("user-block:"+b.Type, "unknown user content block %q; shown as a placeholder", b.Type)
			p.placeholder(blockID(id, i), rec.Timestamp, "Unsupported content block: "+b.Type, end)
		}
	}
	if len(texts) > 0 {
		if kind, ev, text, ok := userText(strings.Join(texts, "\n\n")); ok {
			p.emit(api.ChatMessage{ID: id, Kind: kind, Event: ev, At: rec.Timestamp, Text: text}, end)
		}
	}
}

// toolResult completes the tool call it answers, and adds a permission line when the owner was
// asked.
func (p *claudeParser) toolResult(rec *claudeRecord, id string, b claudeBlock, end int64) {
	out, truncated := toolOutput(b.Content)
	msgID, ok := p.tools[b.ToolUseID]
	var msg api.ChatMessage
	if it := p.c.get(msgID); ok && it != nil && it.msg.Tool != nil {
		msg = it.msg
		t := *msg.Tool
		msg.Tool = &t
	} else {
		// A result whose call this transcript does not hold: show it on its own.
		msgID = "result-" + b.ToolUseID
		msg = api.ChatMessage{ID: msgID, Kind: api.ChatKindTool, At: rec.Timestamp, Tool: &api.ChatTool{Name: "unknown"}}
	}
	status := api.ToolStatusDone
	pd := rec.PermissionDecision
	switch {
	case pd != nil && pd.Decision == "reject":
		status = api.ToolStatusDenied
	case b.IsError:
		status = api.ToolStatusError
	}
	msg.Tool.Status, msg.Tool.Output, msg.Tool.OutputTruncated = status, out, truncated
	p.emit(msg, end)
	if pd != nil && strings.HasPrefix(pd.Source, "user") {
		verb := "Allowed"
		if pd.Decision == "reject" {
			verb = "Denied"
		}
		text := verb + " " + msg.Tool.Name
		if pd.GrantScope != "" && pd.Decision != "reject" {
			text += " (" + pd.GrantScope + ")"
		}
		p.emit(api.ChatMessage{ID: id + "-permission-" + b.ToolUseID, Kind: api.ChatKindEvent,
			Event: api.ChatEventPermission, At: rec.Timestamp, Text: text}, end)
	}
}

func (p *claudeParser) assistant(rec *claudeRecord, id string, end int64) {
	if rec.Message == nil {
		p.placeholder(id, rec.Timestamp, "An agent message in an unknown shape", end)
		return
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
		p.r.warn("assistant-content", "assistant record %s: content is not blocks; shown as a placeholder", id)
		p.placeholder(id, rec.Timestamp, "An agent message in an unknown shape", end)
		return
	}
	for i, b := range blocks {
		bid := blockID(id, i)
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				p.emit(api.ChatMessage{ID: bid, Kind: api.ChatKindAgent, At: rec.Timestamp, Text: b.Text}, end)
			}
		case "thinking", "redacted_thinking":
			// Not shown in the chat view.
		case "tool_use":
			tool := &api.ChatTool{Name: b.Name, Target: toolTarget(b.Input), Status: api.ToolStatusRunning}
			if len(b.Input) > 0 && len(b.Input) <= maxToolInput {
				tool.Input = api.RawJSON(b.Input)
			}
			// A rewrite of a call that already has its result keeps the result.
			if old := p.c.get(p.tools[b.ID]); old != nil && old.msg.Tool != nil {
				tool.Status, tool.Output, tool.OutputTruncated = old.msg.Tool.Status, old.msg.Tool.Output, old.msg.Tool.OutputTruncated
			}
			p.tools[b.ID] = bid
			p.emit(api.ChatMessage{ID: bid, Kind: api.ChatKindTool, At: rec.Timestamp, Tool: tool}, end)
		default:
			p.r.warn("assistant-block:"+b.Type, "unknown assistant content block %q; shown as a placeholder", b.Type)
			p.placeholder(bid, rec.Timestamp, "Unsupported content block: "+b.Type, end)
		}
	}
}

func (p *claudeParser) system(rec *claudeRecord, id string, end int64) {
	var content string
	if raw := bytes.TrimSpace(rec.Content); len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &content) != nil {
		if rec.Subtype == "informational" || rec.Subtype == "api_error" || rec.Subtype == "local_command" {
			p.r.warn("system-content:"+rec.Subtype, "system record %s (%s): content is not text; shown as a placeholder", id, rec.Subtype)
			p.placeholder(id, rec.Timestamp, "A Claude Code system record in an unknown shape: "+rec.Subtype, end)
			return
		}
	}
	msg := api.ChatMessage{ID: id, Kind: api.ChatKindEvent, At: rec.Timestamp}
	switch rec.Subtype {
	case "compact_boundary":
		msg.Event, msg.Text = api.ChatEventCompaction, "Conversation compacted"
		if m := rec.CompactMetadata; m != nil && m.Trigger != "" {
			msg.Text += " (" + m.Trigger + ")"
		}
	case "informational", "api_error":
		msg.Event, msg.Text = api.ChatEventNotice, strings.TrimSpace(content)
	case "local_command":
		msg.Event = api.ChatEventCommand
		_, _, text, ok := userText(content)
		if ok && text == "" || strings.HasPrefix(strings.TrimSpace(content), "<local-command-caveat>") {
			return // a command that printed nothing, or the caveat the model sees
		}
		msg.Text = text
	default:
		if claudeSystemSkipped[rec.Subtype] {
			return
		}
		p.r.warn("system:"+rec.Subtype, "unknown Claude Code system record %q; shown as a placeholder", rec.Subtype)
		msg.Event, msg.Text = api.ChatEventUnknown, "Unsupported Claude Code system record: "+rec.Subtype
	}
	if msg.Text == "" {
		p.r.warn("system-empty:"+rec.Subtype, "system record %s (%s) has no text; shown as a placeholder", id, rec.Subtype)
		p.placeholder(id, rec.Timestamp, "A Claude Code system record with no text: "+rec.Subtype, end)
		return
	}
	p.emit(msg, end)
}

// blockID names the message of content block i of a record. Claude Code writes one block per
// record, so most messages carry the record's uuid alone.
func blockID(id string, i int) string {
	if i == 0 {
		return id
	}
	return id + "-" + strconv.Itoa(i)
}

// toolTarget picks a short target from a tool input: a file, a command, a pattern, a URL.
func toolTarget(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path", "path", "command", "pattern", "url", "query", "description"} {
		var s string
		if json.Unmarshal(m[k], &s) == nil && strings.TrimSpace(s) != "" {
			s = strings.TrimSpace(s)
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[:i]
			}
			t, _ := cut(s, maxTarget)
			return t
		}
	}
	return ""
}

// toolOutput reads a tool_result's content, a string or text blocks, and keeps its first part.
func toolOutput(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var blocks []claudeBlock
		if json.Unmarshal(raw, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, b.Text)
				case "image":
					parts = append(parts, "[image]")
				}
			}
			s = strings.Join(parts, "\n")
		}
	}
	return cut(s, maxToolOutput)
}

// cut keeps at most n bytes of s, on a rune boundary, and reports whether it cut.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// compareVersions compares dotted numeric versions; ok is false when either does not parse.
func compareVersions(a, b string) (cmp int, ok bool) {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
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
