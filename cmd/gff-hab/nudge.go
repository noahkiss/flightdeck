package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// The context nudge (harness-adapters.md, Context nudge): when an agent's context passes a level
// the owner set, gff-hab adds a short note to its context through Claude Code's hook output.

// defaultContextWindow is the window size, in tokens, assumed for a transcript reading when the
// pane sent no status line that names one.
const defaultContextWindow = 200000

// nudgeTail bounds the transcript read of one hook.
const nudgeTail = 256 << 10

// nudgeService keeps each pane's context percentage and the levels it was told about.
type nudgeService struct {
	// value reads a hab-side setting (settings.Service.Value).
	value func(id string) json.RawMessage

	mu    sync.Mutex
	panes map[string]*nudgePane
}

type nudgePane struct {
	session string  // the native session the levels belong to
	percent float64 // from the status line; -1 when none since the last reset
	window  int     // the context window size from the status line; 0 when none
	noted   int     // the highest level noted; 0 for none
}

func newNudgeService(value func(string) json.RawMessage) *nudgeService {
	return &nudgeService{value: value, panes: map[string]*nudgePane{}}
}

func (n *nudgeService) paneLocked(pane string) *nudgePane {
	p := n.panes[pane]
	if p == nil {
		p = &nudgePane{percent: -1}
		n.panes[pane] = p
	}
	return p
}

// nudgePayload holds the fields the nudge reads from a hook or status-line payload.
type nudgePayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`
	AgentID        string `json:"agent_id"`
	ContextWindow  *struct {
		UsedPercentage    *float64 `json:"used_percentage"`
		ContextWindowSize int      `json:"context_window_size"`
	} `json:"context_window"`
}

// Observe hears every envelope of a pane, the status line's too.
func (n *nudgeService) Observe(pane string, env harness.Envelope) {
	if env.Harness != detect.KindClaude {
		return
	}
	var p nudgePayload
	if json.Unmarshal(env.Payload, &p) != nil || p.AgentID != "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	np := n.paneLocked(pane)
	if p.SessionID != "" && np.session != "" && p.SessionID != np.session {
		*np = nudgePane{percent: -1} // a new conversation in the pane
	}
	if p.SessionID != "" {
		np.session = p.SessionID
	}
	switch {
	case env.Event == hook.StatusLineEvent:
		if cw := p.ContextWindow; cw != nil {
			if cw.ContextWindowSize > 0 {
				np.window = cw.ContextWindowSize
			}
			if cw.UsedPercentage != nil && *cw.UsedPercentage >= 0 {
				np.percent = *cw.UsedPercentage
			}
		}
	case env.Event == "PreCompact", env.Event == "SessionStart" && p.Source == "compact":
		np.percent, np.noted = -1, 0
	}
}

// Forget drops a pane that is gone.
func (n *nudgeService) Forget(pane string) {
	n.mu.Lock()
	delete(n.panes, pane)
	n.mu.Unlock()
}

// settingInt reads an integer setting, or def.
func (n *nudgeService) settingInt(id string, def int) int {
	var v int
	if n.value == nil || json.Unmarshal(n.value(id), &v) != nil {
		return def
	}
	return v
}

// nudgeEvent reports whether a hook can carry the context nudge: Claude Code's UserPromptSubmit
// and PostToolUse, two of the context events (hook.ContextEvent).
func nudgeEvent(harnessKind, event string) bool {
	return harnessKind == detect.KindClaude && (event == "UserPromptSubmit" || event == "PostToolUse")
}

// Context is the nudge's part of the hook server's ContextHandler (joinContext): the note to
// add, or "".
func (n *nudgeService) Context(_ context.Context, pane string, env harness.Envelope) string {
	if !nudgeEvent(env.Harness, env.Event) {
		return ""
	}
	first := n.settingInt(settings.ContextNudge, 0)
	if first <= 0 {
		return ""
	}
	step := max(n.settingInt(settings.ContextNudgeStep, 10), 1)
	var p nudgePayload
	if json.Unmarshal(env.Payload, &p) != nil || p.AgentID != "" {
		return "" // a subagent's hook gets none
	}
	n.mu.Lock()
	np := n.paneLocked(pane)
	pct, window := np.percent, np.window
	n.mu.Unlock()
	if pct < 0 && p.TranscriptPath != "" {
		if window <= 0 {
			window = defaultContextWindow
		}
		if tokens, ok := transcriptContext(p.TranscriptPath); ok {
			pct = 100 * float64(tokens) / float64(window)
		}
	}
	if pct < 0 {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	np = n.paneLocked(pane)
	level := nudgeLevel(pct, first, step)
	if level == 0 {
		np.noted = 0 // back under the first level
		return ""
	}
	if level <= np.noted {
		return ""
	}
	np.noted = level
	text := settings.DefaultNudgeText
	if n.value != nil {
		var s string
		if json.Unmarshal(n.value(settings.ContextNudgeText), &s) == nil && s != "" {
			text = s
		}
	}
	return strings.ReplaceAll(text, "{percent}", strconv.Itoa(int(math.Round(min(pct, 100)))))
}

// joinContext is the hook server's one ContextHandler: each part's text in order (the Logbook
// habit, then the context nudge), joined by a blank line; "" when none is due. Every part runs,
// so each keeps its own count of the events it saw.
func joinContext(parts ...hook.ContextHandler) hook.ContextHandler {
	return func(ctx context.Context, pane string, env harness.Envelope) string {
		var texts []string
		for _, part := range parts {
			if t := part(ctx, pane, env); t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n\n")
	}
}

// nudgeLevel is the highest level pct reached: first, then each step above it, below 100. 0
// means under the first level.
func nudgeLevel(pct float64, first, step int) int {
	if pct < float64(first) {
		return 0
	}
	level := first + int((pct-float64(first))/float64(step))*step
	for level >= 100 {
		level -= step
	}
	return max(level, first)
}

// transcriptContext reads the last 256 KiB of a Claude Code transcript and returns the context
// the last main-thread assistant record used: input, cache creation and cache read tokens. A
// compact_boundary after it means the context was compacted since: no reading.
func transcriptContext(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, false
	}
	from := max(fi.Size()-nudgeTail, 0)
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, false
	}
	r := bufio.NewReaderSize(io.LimitReader(f, nudgeTail), 64<<10)
	if from > 0 {
		r.ReadBytes('\n') // a cut first line
	}
	tokens, ok := 0, false
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return tokens, ok
		}
		switch {
		case bytes.Contains(line, []byte(`"compact_boundary"`)):
			tokens, ok = 0, false
		case bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"assistant"`)):
			var rec struct {
				Type        string `json:"type"`
				IsSidechain bool   `json:"isSidechain"`
				Message     struct {
					Usage *struct {
						Input         int `json:"input_tokens"`
						CacheCreation int `json:"cache_creation_input_tokens"`
						CacheRead     int `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "assistant" && !rec.IsSidechain && rec.Message.Usage != nil {
				u := rec.Message.Usage
				tokens, ok = u.Input+u.CacheCreation+u.CacheRead, true
			}
		}
	}
}
