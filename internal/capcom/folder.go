package capcom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// MCPServer is the name the agent kit's MCP server has in each harness's settings.
const MCPServer = "flightdeck"

// ReadScope is the part of the hab's read scope (capcom.md 5) the folder's harness settings
// carry as deny rules: absolute paths or patterns the harness's own file tools should refuse.
// The read-scope stage fills it; until then it is empty and no deny rule is written. It is a
// cheap guard only: the read tools are the boundary (capcom.md 5.3).
type ReadScope struct {
	Deny []string
}

// File is one file of a Capcom folder: its path inside the folder and its bytes.
type File struct {
	Path string
	Data []byte
}

// Files returns the manual and the harness settings for a Capcom folder (capcom.md 7.1): the
// manual as the harness's native instruction file, and the harness's project-level settings
// that register `<exe> mcp` as the agent kit's MCP server and allow its tools. exe is gff-hab's
// own resolved path.
func Files(harness, exe string, scope ReadScope) ([]File, error) {
	if exe == "" || !filepath.IsAbs(exe) {
		return nil, fmt.Errorf("capcom: gff-hab's path %q is not absolute", exe)
	}
	switch harness {
	case Claude:
		mcp, err := jsonFile(map[string]any{"mcpServers": map[string]any{
			MCPServer: map[string]any{"type": "stdio", "command": exe, "args": []string{"mcp"}},
		}})
		if err != nil {
			return nil, err
		}
		perms := map[string]any{"allow": []string{"mcp__" + MCPServer}}
		if deny := claudeDeny(scope); len(deny) > 0 {
			perms["deny"] = deny
		}
		settings, err := jsonFile(map[string]any{
			"enabledMcpjsonServers": []string{MCPServer},
			"permissions":           perms,
		})
		if err != nil {
			return nil, err
		}
		return []File{
			{"CLAUDE.md", []byte(Manual)},
			{".mcp.json", mcp},
			{filepath.Join(".claude", "settings.local.json"), settings},
		}, nil
	case Codex:
		// Codex has no deny rules for its file tools; the read tools stay the boundary.
		var b bytes.Buffer
		b.WriteString("# Written by gff-hab at each Capcom start; edits here are replaced.\n")
		err := toml.NewEncoder(&b).Encode(map[string]any{"mcp_servers": map[string]any{
			MCPServer: map[string]any{"command": exe, "args": []string{"mcp"}},
		}})
		if err != nil {
			return nil, err
		}
		return []File{
			{"AGENTS.md", []byte(Manual)},
			{filepath.Join(".codex", "config.toml"), b.Bytes()},
		}, nil
	case Opencode:
		// The read scope's deny rules for opencode come with the read-scope stage.
		cfg := map[string]any{
			"$schema": "https://opencode.ai/config.json",
			"mcp": map[string]any{MCPServer: map[string]any{
				"type": "local", "command": []string{exe, "mcp"}, "enabled": true,
			}},
		}
		oc, err := jsonFile(cfg)
		if err != nil {
			return nil, err
		}
		return []File{
			{"AGENTS.md", []byte(Manual)},
			{"opencode.json", oc},
		}, nil
	}
	return nil, fmt.Errorf("capcom: harness %q cannot run Capcom; want one of %q", harness, Harnesses)
}

// claudeDeny turns the read scope into Claude Code permission rules. An absolute path takes
// Claude's // prefix.
func claudeDeny(scope ReadScope) []string {
	var out []string
	for _, p := range scope.Deny {
		if strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		out = append(out, "Read("+p+")")
	}
	return out
}

func jsonFile(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteFolder makes dir when it is missing and writes the manual and settings into it,
// replacing what was there under those names (capcom.md 9: a hand edit is undone at the next
// start). Any other file, such as an attachment, is left alone.
func WriteFolder(dir, harness, exe string, scope ReadScope) error {
	files, err := Files(harness, exe, scope)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		// A link in the folder's place is replaced, never written through.
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
		if err := writeFile(p, f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Start is how a Capcom session's harness starts in its folder.
type Start struct {
	Harness string   // session.create's harness: codex and opencode get their adapter's start; empty for Claude Code
	Argv    []string // the program and its arguments
	Env     []string // added to the pane's environment
}

// StartFor returns the first start of a harness. Claude Code gets the session id it will use
// (a fresh UUID from the caller), so the resume id is known from the first byte.
func StartFor(harness, claudeID string) (Start, error) {
	switch harness {
	case Claude:
		if claudeID == "" {
			return Start{}, fmt.Errorf("capcom: Claude Code needs a session id")
		}
		return Start{Argv: []string{"claude", "--session-id", claudeID}, Env: []string{"AGENT_KIND=claude"}}, nil
	case Codex:
		return Start{Harness: Codex, Argv: []string{"codex"}}, nil
	case Opencode:
		return Start{Harness: Opencode, Argv: []string{"opencode"}}, nil
	}
	return Start{}, fmt.Errorf("capcom: harness %q cannot run Capcom; want one of %q", harness, Harnesses)
}

// ResumeFor returns the start that resumes a session in its folder. With no resume id known,
// Codex and opencode pick the folder's most recent session.
func ResumeFor(harness, resume string) (Start, error) {
	switch harness {
	case Claude:
		if resume == "" {
			return Start{Argv: []string{"claude", "--continue"}, Env: []string{"AGENT_KIND=claude"}}, nil
		}
		return Start{Argv: []string{"claude", "--resume", resume}, Env: []string{"AGENT_KIND=claude"}}, nil
	case Codex:
		if resume == "" {
			return Start{Harness: Codex, Argv: []string{"codex", "resume", "--last"}}, nil
		}
		return Start{Harness: Codex, Argv: []string{"codex", "resume", resume}}, nil
	case Opencode:
		if resume == "" {
			return Start{Harness: Opencode, Argv: []string{"opencode", "--continue"}}, nil
		}
		return Start{Harness: Opencode, Argv: []string{"opencode", "--session", resume}}, nil
	}
	return Start{}, fmt.Errorf("capcom: harness %q cannot run Capcom; want one of %q", harness, Harnesses)
}

// ResumeCommand is what a user types in the session's folder to resume it from a plain
// terminal (capcom.md 7.4).
func ResumeCommand(harness, resume string) string {
	st, err := ResumeFor(harness, resume)
	if err != nil {
		return ""
	}
	return strings.Join(st.Argv, " ")
}
