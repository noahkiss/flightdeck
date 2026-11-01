package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// The Codex versions whose rollout shapes the parser was written and tested against. Codex
// 0.159 and later write each finished conversation item as an item_completed event; the reader
// builds the chat view from those. The format is undocumented; a version outside this range
// still reads, and the reader reports drift.
const (
	CodexTestedMin = "0.159.0"
	CodexTestedMax = "0.166.0"
)

// codexSkippedLines lists the rollout line types the parser knows and leaves out. response_item
// lines are what the model saw (the same conversation, with context and tool plumbing); the
// chat view reads the item_completed events instead.
var codexSkippedLines = map[string]bool{
	"turn_context": true, "response_item": true, "world_state": true, "token_usage_record": true,
	"inter_agent_communication_metadata": true,
}

// codexSkippedEvents lists event_msg types that are bookkeeping, progress, or a second copy of
// an item the chat view reads from item_completed. An event_msg type in neither this list nor
// the parser's switch is logged once and left out: Codex has many UI events, and a placeholder
// for each new one would bury the conversation.
var codexSkippedEvents = map[string]bool{
	"token_count": true, "task_started": true, "task_complete": true, "thread_settings_applied": true,
	"item_started": true, "agent_reasoning": true, "agent_reasoning_raw_content": true,
	"agent_message": true, "user_message": true, "exec_command_begin": true, "exec_command_end": true,
	"patch_apply_begin": true, "patch_apply_end": true, "web_search_begin": true, "web_search_end": true,
	"mcp_tool_call_begin": true, "mcp_tool_call_end": true, "image_generation_begin": true,
	"image_generation_end": true, "sub_agent_activity": true, "context_compacted": true,
	"thread_goal_updated": true, "thread_name_updated": true, "turn_diff": true, "plan_update": true,
}

// codexSkippedItems lists item_completed item types left out of the chat view.
var codexSkippedItems = map[string]bool{
	"Reasoning": true, "ContextCompaction": true, // the compacted line marks a compaction
}

// codexLine is one rollout line.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexEvent is the part of an event_msg payload the parser reads.
type codexEvent struct {
	Type    string          `json:"type"`
	Item    json.RawMessage `json:"item"`
	Reason  string          `json:"reason"`  // turn_aborted
	Message string          `json:"message"` // error, warning
}

// codexItem is a finished conversation item: the fields of each item type the parser reads.
type codexItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Kind    string `json:"kind"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// CommandExecution
	Command          json.RawMessage `json:"command"`
	ExitCode         *int            `json:"exit_code"`
	AggregatedOutput string          `json:"aggregated_output"`
	// FileChange
	Changes map[string]json.RawMessage `json:"changes"`
	Stdout  string                     `json:"stdout"`
	Stderr  string                     `json:"stderr"`
	// McpToolCall
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"` // McpToolCall: content blocks; Extension: image bytes
	// Extension, ImageView, SubAgentActivity
	Query     string `json:"query"`
	SavedPath string `json:"savedPath"`
	Path      string `json:"path"`
	AgentPath string `json:"agent_path"`
}

// codexParser turns Codex rollout lines into chat messages.
type codexParser struct {
	c       *conversation
	r       *Reader
	at      string
	changed func(*item)
}

func newCodexParser(r *Reader) lineParser {
	return &codexParser{c: r.c, r: r, changed: r.changed}
}

func (p *codexParser) emit(msg api.ChatMessage, end int64) {
	if msg.At == "" {
		msg.At = p.at
	}
	p.changed(p.c.put(msg, end))
}

func (p *codexParser) placeholder(id, text string, end int64) {
	p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindEvent, Event: api.ChatEventUnknown, Text: text}, end)
}

// line handles one complete rollout line that ends at offset end.
func (p *codexParser) line(data []byte, end int64) {
	lineID := "line-" + strconv.FormatInt(end, 10)
	if data == nil {
		p.r.warn("oversize", "rollout line ending at byte %d is longer than %d bytes; shown as a placeholder", end, maxLine)
		p.placeholder(lineID, "A transcript record too large to read", end)
		return
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}
	var l codexLine
	if err := json.Unmarshal(data, &l); err != nil {
		p.r.warn("unreadable", "unreadable rollout line ending at byte %d: %v", end, err)
		p.placeholder(lineID, "An unreadable transcript record", end)
		return
	}
	if l.Timestamp != "" {
		p.at = l.Timestamp
	}
	switch {
	case l.Type == "session_meta":
		var m struct {
			CliVersion string `json:"cli_version"`
		}
		if json.Unmarshal(l.Payload, &m) == nil && m.CliVersion != "" {
			p.r.checkVersion(m.CliVersion)
		}
	case l.Type == "event_msg":
		p.event(l.Payload, lineID, end)
	case l.Type == "compacted":
		p.emit(api.ChatMessage{ID: lineID, Kind: api.ChatKindEvent, Event: api.ChatEventCompaction, Text: "Conversation compacted"}, end)
	case codexSkippedLines[l.Type]:
	default:
		p.r.warn("type:"+l.Type, "unknown Codex rollout line type %q; shown as a placeholder", l.Type)
		p.placeholder(lineID, "Unsupported Codex record: "+l.Type, end)
	}
}

func (p *codexParser) event(raw json.RawMessage, lineID string, end int64) {
	var e codexEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		p.r.warn("event-shape", "event_msg ending at byte %d: %v; shown as a placeholder", end, err)
		p.placeholder(lineID, "A Codex event in an unknown shape", end)
		return
	}
	switch e.Type {
	case "item_completed":
		p.item(e.Item, lineID, end)
	case "turn_aborted":
		msg := api.ChatMessage{ID: lineID, Kind: api.ChatKindEvent, Event: api.ChatEventInterrupt, Text: "Interrupted by the owner"}
		if e.Reason != "" && e.Reason != "interrupted" {
			msg.Event, msg.Text = api.ChatEventNotice, "Turn aborted: "+e.Reason
		}
		p.emit(msg, end)
	case "error", "warning", "stream_error":
		text := strings.TrimSpace(e.Message)
		if text == "" {
			text = "Codex reported an " + e.Type + " with no text"
		}
		p.emit(api.ChatMessage{ID: lineID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: text}, end)
	default:
		if !codexSkippedEvents[e.Type] {
			p.r.warn("event:"+e.Type, "unknown Codex event %q; left out", e.Type)
		}
	}
}

func (p *codexParser) item(raw json.RawMessage, lineID string, end int64) {
	var it codexItem
	if err := json.Unmarshal(raw, &it); err != nil || it.Type == "" {
		p.r.warn("item-shape", "item_completed ending at byte %d has no readable item; shown as a placeholder", end)
		p.placeholder(lineID, "A Codex item in an unknown shape", end)
		return
	}
	id := it.ID
	if id == "" {
		id = lineID
	}
	switch it.Type {
	case "UserMessage", "AgentMessage":
		var texts []string
		for _, b := range it.Content {
			switch strings.ToLower(b.Type) {
			case "text", "input_text", "output_text":
				texts = append(texts, b.Text)
			case "image", "local_image", "input_image":
				texts = append(texts, "[image]")
			default:
				p.r.warn("content:"+b.Type, "unknown Codex message content %q; left out", b.Type)
			}
		}
		text := strings.TrimSpace(strings.Join(texts, "\n\n"))
		if text == "" {
			return
		}
		kind := api.ChatKindUser
		if it.Type == "AgentMessage" {
			kind = api.ChatKindAgent
		}
		p.emit(api.ChatMessage{ID: id, Kind: kind, Text: text}, end)
	case "CommandExecution":
		cmd := codexCommand(it.Command)
		tool := &api.ChatTool{Name: "exec", Target: firstLine(cmd), Status: codexStatus(it.Status, it.ExitCode != nil && *it.ExitCode != 0)}
		if in, err := json.Marshal(map[string]string{"command": cmd}); err == nil && len(in) <= maxToolInput {
			tool.Input = in
		}
		tool.Output, tool.OutputTruncated = cut(it.AggregatedOutput, maxToolOutput)
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	case "FileChange":
		paths := make([]string, 0, len(it.Changes))
		for f := range it.Changes {
			paths = append(paths, f)
		}
		sort.Strings(paths)
		target := ""
		if len(paths) > 0 {
			target = paths[0]
			if len(paths) > 1 {
				target += fmt.Sprintf(" and %d more", len(paths)-1)
			}
		}
		tool := &api.ChatTool{Name: "apply_patch", Status: codexStatus(it.Status, false)}
		tool.Target, _ = cut(target, maxTarget)
		if in, err := json.Marshal(it.Changes); err == nil && len(in) <= maxToolInput {
			tool.Input = in
		}
		tool.Output, tool.OutputTruncated = cut(strings.TrimSpace(it.Stdout+"\n"+it.Stderr), maxToolOutput)
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	case "McpToolCall":
		var res struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(it.Result, &res) // no result, or another shape: no output
		tool := &api.ChatTool{Name: "mcp__" + it.Server + "__" + it.Tool, Target: toolTarget(it.Arguments), Status: codexStatus(it.Status, res.IsError)}
		if len(it.Arguments) > 0 && string(it.Arguments) != "null" && len(it.Arguments) <= maxToolInput {
			tool.Input = api.RawJSON(it.Arguments)
		}
		var parts []string
		for _, b := range res.Content {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "image":
				parts = append(parts, "[image]")
			}
		}
		tool.Output, tool.OutputTruncated = cut(strings.Join(parts, "\n"), maxToolOutput)
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	case "Extension":
		// Built-in tools: web search, image generation. The image itself (base64) is never read.
		tool := &api.ChatTool{Name: it.Kind, Status: codexStatus(it.Status, false)}
		switch it.Kind {
		case "web.search":
			tool.Name = "web_search"
		case "image_gen.generation":
			tool.Name = "image_gen"
		case "":
			tool.Name = "extension"
		}
		tool.Target, _ = cut(firstLine(firstNonEmpty(it.Query, it.SavedPath)), maxTarget)
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	case "ImageView":
		tool := &api.ChatTool{Name: "view_image", Status: api.ToolStatusDone}
		tool.Target, _ = cut(strings.TrimPrefix(it.Path, "file://"), maxTarget)
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	case "SubAgentActivity":
		if it.Kind != "started" && it.Kind != "completed" {
			return // interacted: a message to a running subagent, shown by its tool call
		}
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindEvent, Event: api.ChatEventNotice,
			Text: strings.TrimSpace("Subagent " + it.AgentPath + " " + it.Kind)}, end)
	case "CollabAgentToolCall":
		tool := &api.ChatTool{Name: "agent_" + it.Tool, Status: codexStatus(it.Status, false)}
		p.emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: tool}, end)
	default:
		if codexSkippedItems[it.Type] {
			return
		}
		p.r.warn("item:"+it.Type, "unknown Codex item type %q; shown as a placeholder", it.Type)
		p.placeholder(id, "Unsupported Codex item: "+it.Type, end)
	}
}

// codexStatus maps an item's status to a tool status. failed says the item reports a failure
// another way (a non-zero exit code, an error result).
func codexStatus(status string, failed bool) api.ToolStatus {
	switch status {
	case "declined":
		return api.ToolStatusDenied
	case "failed":
		return api.ToolStatusError
	case "inProgress", "in_progress":
		return api.ToolStatusRunning
	}
	if failed {
		return api.ToolStatusError
	}
	return api.ToolStatusDone
}

// codexCommand reads a command execution's command: an argv, where a shell run (sh -lc
// script) shows as its script, or one string.
func codexCommand(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil || len(argv) == 0 {
		return ""
	}
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	t, _ := cut(s, maxTarget)
	return t
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// FindCodex finds the rollout of a Codex session under the Codex home:
// sessions/YYYY/MM/DD/rollout-<time>-<session id>.jsonl. When several hold the id, the most
// recently written wins.
func FindCodex(home, sessionID string) (string, error) {
	if !claudeSessionID.MatchString(sessionID) {
		return "", fmt.Errorf("session id %q cannot name a transcript", sessionID)
	}
	return newestFile(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
}

// CodexHome is where Codex keeps its data: $CODEX_HOME, else ~/.codex.
func CodexHome(getenv func(string) string) (string, error) {
	if d := getenv("CODEX_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}
