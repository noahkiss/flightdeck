package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// Where a member's context_pct comes from (groups.md 9.1, stage B6). Claude Code: the
// status-line input, else the transcript's usage (the context nudge's reading). Codex: the
// rollout's token_count events: info.last_token_usage over info.model_context_window, or the
// window a task_started event named. Codex 0.166 writes the window in both places; its
// token_count before the first turn carries no info, which is no reading. opencode: none yet,
// so an opencode member is supervised by nothing until its reading exists (its conversation is
// in its server, not in a file the hab sizes).

// rolloutTail bounds the rollout read of one reading.
const rolloutTail = 256 << 10

// codexContext reads the end of a Codex rollout and returns the context the last turn used, in
// percent of the model's window. ok false: no token_count with usage and a window yet.
func codexContext(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	from := max(fi.Size()-rolloutTail, 0)
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, false
	}
	r := bufio.NewReaderSize(io.LimitReader(f, rolloutTail), 64<<10)
	if from > 0 {
		r.ReadBytes('\n') // a cut first line
	}
	window, used := 0, -1
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // an incomplete last line waits for its newline
		}
		if !bytes.Contains(line, []byte(`"event_msg"`)) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				Window int    `json:"model_context_window"`
				Info   *struct {
					Window int `json:"model_context_window"`
					Last   *struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
						Total  int `json:"total_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "event_msg" {
			continue
		}
		switch rec.Payload.Type {
		case "task_started":
			if rec.Payload.Window > 0 {
				window = rec.Payload.Window
			}
		case "token_count":
			in := rec.Payload.Info
			if in == nil || in.Last == nil {
				continue
			}
			if in.Window > 0 {
				window = in.Window
			}
			used = in.Last.Total
			if used <= 0 {
				used = in.Last.Input + in.Last.Output
			}
		}
	}
	if window <= 0 || used < 0 {
		return 0, false
	}
	return 100 * float64(used) / float64(window), true
}

// memberReading is a member's context use and its transcript path, from the pane's agent
// state: the nudge's reading for Claude Code, the rollout for Codex.
func memberReading(nudges *nudgeService, find func(harness, native string) (string, error)) func(pane string, st api.AgentState) (*int, string) {
	return func(pane string, st api.AgentState) (*int, string) {
		var pct float64
		var path string
		ok := false
		switch st.Harness {
		case detect.KindClaude:
			pct, path, ok = nudges.Reading(pane)
			if path == "" && find != nil && st.Session != "" {
				path, _ = find(st.Harness, st.Session)
			}
		case detect.KindCodex:
			if find == nil || st.Session == "" {
				return nil, ""
			}
			path, _ = find(st.Harness, st.Session)
			if path != "" {
				pct, ok = codexContext(path)
			}
		default:
			return nil, ""
		}
		if !ok {
			return nil, path
		}
		n := int(math.Round(min(max(pct, 0), 100)))
		return &n, path
	}
}
