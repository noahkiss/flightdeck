package mcpreg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// entry is one server rendered in a harness's native shape (spec 7.1), as the JSON or TOML
// values the harness's file holds. notes are things the render left out on purpose.
type entry struct {
	value map[string]any
	notes []string
}

// notRendered says why a server has no entry for a harness.
type notRendered struct{ reason string }

func (n notRendered) Error() string { return n.reason }

// render builds a server's entry for one harness, or a notRendered error.
func render(h Harness, s Server) (entry, error) {
	for _, m := range []map[string]Value{s.Env, s.Headers} {
		for _, v := range m {
			if v.NeedsLaunch() {
				return entry{}, notRendered{"needs secret resolution at launch (gff-hab mcp launch), which a later stage builds"}
			}
		}
	}
	switch h {
	case instructions.Claude:
		return renderClaude(s)
	case instructions.Codex:
		return renderCodex(s)
	case instructions.Opencode:
		return renderOpencode(s)
	}
	return entry{}, notRendered{"no renderer for " + string(h) + " yet"}
}

func literals(s Server) []string {
	out := append([]string{s.Command, s.Cwd, s.URL}, s.Args...)
	for _, m := range []map[string]Value{s.Env, s.Headers} {
		for _, v := range m {
			out = append(out, v.Literal, v.Prefix)
		}
	}
	return out
}

func renderClaude(s Server) (entry, error) {
	for _, l := range literals(s) {
		if strings.Contains(l, "${") {
			return entry{}, notRendered{"a plain value holds ${, which Claude would expand"}
		}
	}
	if s.Cwd != "" {
		return entry{}, notRendered{"Claude has no working-folder key for a server"}
	}
	var e entry
	ref := func(v Value) string {
		if v.IsLiteral() {
			return v.Literal
		}
		return v.Prefix + "${" + v.Env + "}"
	}
	if s.Transport == Stdio {
		e.value = map[string]any{"type": "stdio", "command": s.Command, "args": strs(s.Args)}
		if len(s.Env) > 0 {
			e.value["env"] = valueMap(s.Env, ref)
		}
	} else {
		e.value = map[string]any{"type": "http", "url": s.URL}
		if len(s.Headers) > 0 {
			e.value["headers"] = valueMap(s.Headers, ref)
		}
		if s.Auth == "oauth" {
			e.notes = append(e.notes, "sign in through /mcp in Claude")
		}
	}
	if s.Timeout > 0 {
		e.notes = append(e.notes, "Claude has no per-server timeout; timeout left out")
	}
	return e, nil
}

func renderCodex(s Server) (entry, error) {
	var e entry
	if s.Transport == Stdio {
		e.value = map[string]any{"command": s.Command}
		if len(s.Args) > 0 {
			e.value["args"] = strs(s.Args)
		}
		if s.Cwd != "" {
			e.value["cwd"] = s.Cwd
		}
		env := map[string]any{}
		var pass []any
		for _, k := range sortedKeys(s.Env) {
			v := s.Env[k]
			switch {
			case v.IsLiteral():
				env[k] = v.Literal
			case v.Env == k && v.Prefix == "":
				pass = append(pass, k)
			default:
				return entry{}, notRendered{fmt.Sprintf("env %s: Codex passes a variable only under its own name, with no prefix", k)}
			}
		}
		if len(env) > 0 {
			e.value["env"] = env
		}
		if len(pass) > 0 {
			e.value["env_vars"] = pass
		}
	} else {
		e.value = map[string]any{"url": s.URL}
		lit, byEnv := map[string]any{}, map[string]any{}
		for _, k := range sortedKeys(s.Headers) {
			v := s.Headers[k]
			switch {
			case v.IsLiteral():
				lit[k] = v.Literal
			case v.Prefix == "":
				byEnv[k] = v.Env
			case strings.EqualFold(k, "Authorization") && v.Prefix == "Bearer ":
				e.value["bearer_token_env_var"] = v.Env
			default:
				return entry{}, notRendered{fmt.Sprintf("header %s: Codex takes a header from a variable only whole, or as a bearer token", k)}
			}
		}
		if len(lit) > 0 {
			e.value["http_headers"] = lit
		}
		if len(byEnv) > 0 {
			e.value["env_http_headers"] = byEnv
		}
		if s.Auth == "oauth" {
			e.notes = append(e.notes, "sign in with codex mcp login "+s.Name)
		}
	}
	if s.Timeout > 0 {
		e.value["startup_timeout_sec"] = s.Timeout
	}
	return e, nil
}

func renderOpencode(s Server) (entry, error) {
	for _, l := range literals(s) {
		if strings.Contains(l, "{env:") || strings.Contains(l, "{file:") {
			return entry{}, notRendered{"a plain value holds {env: or {file:, which opencode would expand"}
		}
	}
	var e entry
	ref := func(v Value) string {
		if v.IsLiteral() {
			return v.Literal
		}
		return v.Prefix + "{env:" + v.Env + "}"
	}
	if s.Transport == Stdio {
		e.value = map[string]any{"type": "local", "command": strs(append([]string{s.Command}, s.Args...))}
		if len(s.Env) > 0 {
			e.value["environment"] = valueMap(s.Env, ref)
		}
		if s.Cwd != "" {
			e.value["cwd"] = s.Cwd
		}
	} else {
		e.value = map[string]any{"type": "remote", "url": s.URL}
		if len(s.Headers) > 0 {
			e.value["headers"] = valueMap(s.Headers, ref)
		}
		if s.Auth == "oauth" {
			e.notes = append(e.notes, "sign in with opencode mcp auth "+s.Name)
		}
	}
	if s.Timeout > 0 {
		e.value["timeout"] = s.Timeout * 1000
	}
	return e, nil
}

func strs(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func valueMap(m map[string]Value, ref func(Value) string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = ref(v)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// canonical is a value's JSON with sorted keys and plain numbers, so the same entry gives the
// same bytes whether it came from our render, a JSON file or a TOML file.
func canonical(v any) string {
	b, err := json.Marshal(normalize(v))
	if err != nil {
		return ""
	}
	return string(b)
}

// normalize turns the decoders' types into one set: maps of any, slices of any, float64.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, y := range x {
			out[k] = normalize(y)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = normalize(y)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = normalize(y)
		}
		return out
	case []string:
		return strs(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return v
}

// entryDigest is the digest the state record keeps for an entry.
func entryDigest(v any) string {
	sum := sha256.Sum256([]byte(canonical(v)))
	return hex.EncodeToString(sum[:])
}
