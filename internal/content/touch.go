package content

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Touch is one file the session's tool calls touched, merged over the whole conversation.
type Touch struct {
	Path    string // absolute, cleaned
	Image   bool   // an image a tool call read, wrote or viewed
	Changed bool   // the session wrote or edited it
	Message string // the last tool call that touched it
	At      string
	Edits   []Edit // in order; for a diff when the file is outside a repository
}

// Edit is one change a tool call made, as the transcript holds it.
type Edit struct {
	Old, New string
	Write    bool   // the whole file was written: New is its content
	Diff     string // a unified diff the harness recorded (Codex apply_patch), instead of Old and New
	Deleted  bool
}

// Touches derives the files a conversation's tool calls touched (app.md 4.7): Claude Code's
// Write, Edit, MultiEdit, NotebookEdit and image Reads, Codex's apply_patch, view_image and
// image_gen. A relative path is taken from cwd; one that stays relative is left out. A call
// that failed or was denied changed nothing, so it counts only as a read of an image.
func Touches(msgs []api.ChatMessage, cwd string) []Touch {
	var out []Touch
	index := map[string]int{}
	touch := func(m api.ChatMessage, path string) *Touch {
		path = absPath(path, cwd)
		if path == "" {
			return nil
		}
		i, ok := index[path]
		if !ok {
			i = len(out)
			index[path] = i
			out = append(out, Touch{Path: path})
		}
		t := &out[i]
		t.Message, t.At = m.ID, m.At
		return t
	}
	for _, m := range msgs {
		if m.Kind != api.ChatKindTool || m.Tool == nil {
			continue
		}
		tool := m.Tool
		ok := tool.Status != api.ToolStatusError && tool.Status != api.ToolStatusDenied
		var in map[string]json.RawMessage
		_ = json.Unmarshal(tool.Input, &in)
		switch tool.Name {
		case "Write":
			path := str(in, "file_path")
			if t := touch(m, path); t != nil {
				t.Image = t.Image || IsImage(path)
				if ok {
					t.Changed = true
					t.Edits = append(t.Edits, Edit{New: str(in, "content"), Write: true})
				}
			}
		case "Edit":
			if !ok {
				continue
			}
			if t := touch(m, str(in, "file_path")); t != nil {
				t.Changed = true
				t.Edits = append(t.Edits, Edit{Old: str(in, "old_string"), New: str(in, "new_string")})
			}
		case "MultiEdit":
			if !ok {
				continue
			}
			if t := touch(m, str(in, "file_path")); t != nil {
				t.Changed = true
				var edits []struct {
					Old string `json:"old_string"`
					New string `json:"new_string"`
				}
				_ = json.Unmarshal(in["edits"], &edits)
				for _, e := range edits {
					t.Edits = append(t.Edits, Edit{Old: e.Old, New: e.New})
				}
			}
		case "NotebookEdit":
			if !ok {
				continue
			}
			if t := touch(m, str(in, "notebook_path")); t != nil {
				t.Changed = true
			}
		case "Read":
			if path := str(in, "file_path"); IsImage(path) {
				if t := touch(m, path); t != nil {
					t.Image = true
				}
			}
		case "view_image", "image_gen":
			if IsImage(tool.Target) {
				if t := touch(m, tool.Target); t != nil {
					t.Image = true
				}
			}
		case "apply_patch":
			if !ok {
				continue
			}
			paths := make([]string, 0, len(in))
			for path := range in {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				raw := in[path]
				var c struct {
					Type        string `json:"type"`
					UnifiedDiff string `json:"unified_diff"`
					Content     string `json:"content"`
				}
				_ = json.Unmarshal(raw, &c)
				t := touch(m, path)
				if t == nil {
					continue
				}
				t.Changed = true
				t.Image = t.Image || IsImage(path)
				switch {
				case c.Type == "delete":
					t.Edits = append(t.Edits, Edit{Deleted: true})
				case c.UnifiedDiff != "":
					t.Edits = append(t.Edits, Edit{Diff: c.UnifiedDiff})
				case c.Content != "":
					t.Edits = append(t.Edits, Edit{New: c.Content, Write: true})
				}
			}
		}
	}
	return out
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return strings.TrimSpace(s)
}

// absPath makes path absolute against cwd and cleans it; "" when it cannot be.
func absPath(path, cwd string) string {
	path = strings.TrimPrefix(strings.TrimSpace(path), "file://")
	if path == "" || strings.ContainsRune(path, 0) {
		return ""
	}
	if !filepath.IsAbs(path) {
		if cwd == "" || !filepath.IsAbs(cwd) {
			return ""
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}
