package mcpreg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/instructions"
)

// maxConfigBytes bounds a harness config file the renderer reads.
const maxConfigBytes = 8 << 20

// Env holds the files the targets live in. EnvFrom reads the process environment for the
// command; tests build one by hand.
type Env struct {
	ClaudeJSON  string // $CLAUDE_CONFIG_DIR/.claude.json, else ~/.claude.json
	CodexHome   string // $CODEX_HOME, else ~/.codex
	OpencodeDir string // $XDG_CONFIG_HOME/opencode, else ~/.config/opencode
	StateDir    string // Flightdeck's state directory
}

// EnvFrom fills an Env from a getenv function, the home directory and the state directory.
func EnvFrom(getenv func(string) string, home, state string) Env {
	ie := instructions.EnvFrom(getenv, home, state)
	claude := filepath.Join(home, ".claude.json")
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		claude = filepath.Join(d, ".claude.json")
	}
	return Env{ClaudeJSON: claude, CodexHome: ie.CodexHome, OpencodeDir: ie.OpencodeDir, StateDir: state}
}

// Status is what a harness's plan allows.
type Status string

const (
	Ready    Status = "ready"             // the config or the record changes
	UpToDate Status = "up-to-date"        // nothing changes
	Nothing  Status = "nothing to render" // no server for this harness and none owned
	Refused  Status = "refused"           // the file cannot be edited safely
)

// Action is what a write does to one server.
type Action string

const (
	Add         Action = "add"
	Update      Action = "update"
	Remove      Action = "remove"
	Keep        Action = "keep"
	Forget      Action = "forget"       // owned, already gone from the file and the registry
	NotRendered Action = "not rendered" // the harness cannot take this server
	Blocked     Action = "refused"      // not ours, or edited by hand without -force
)

// ServerPlan is one server's line in a plan.
type ServerPlan struct {
	Name   string
	Action Action
	Reason string
	Notes  []string
	value  map[string]any // the rendered entry, for add, update and keep
	inFile bool
}

// Plan is one harness's render against its config file.
type Plan struct {
	Harness  Harness
	Path     string
	Status   Status
	Reason   string
	Enabled  bool
	Servers  []ServerPlan
	Old, New string // the diff's two sides
	DiffName string // what the diff shows, when it is not the whole file
	Warnings []string
	NewDir   string // the target's directory, when a write would create it
	raw      string // the file as read, to spot a change before the rename
	exists   bool
	mode     os.FileMode
	owned    map[string]string
}

// Renderer plans and writes the registry into each harness's config.
type Renderer struct {
	Config Config
	Env    Env
	Host   string    // this host's id, for the servers' hosts lists
	Force  []string  // owned servers a write may replace or remove though edited by hand
	Claude ClaudeCLI // runs Claude's own config writer; nil runs "claude"
	Now    func() time.Time
}

// ClaudeCLI runs `claude <args>` and returns its combined output.
type ClaudeCLI func(ctx context.Context, args ...string) ([]byte, error)

// ExecClaude runs a claude binary.
func ExecClaude(bin string) ClaudeCLI {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdin = nil
		return cmd.CombinedOutput()
	}
}

// PlanAll plans every harness with a renderer.
func (r *Renderer) PlanAll() []Plan {
	out := make([]Plan, 0, len(Rendered))
	for _, h := range Rendered {
		out = append(out, r.Plan(h))
	}
	return out
}

// TargetPath is the config file a harness's servers go to, and why it cannot be used, if so.
func (e Env) TargetPath(h Harness) (string, string) {
	switch h {
	case instructions.Claude:
		return e.ClaudeJSON, ""
	case instructions.Codex:
		return filepath.Join(e.CodexHome, "config.toml"), ""
	case instructions.Opencode:
		jsonc, plain := filepath.Join(e.OpencodeDir, "opencode.jsonc"), filepath.Join(e.OpencodeDir, "opencode.json")
		a, b := lexists(jsonc), lexists(plain)
		switch {
		case a && b:
			return jsonc, "both opencode.jsonc and opencode.json exist; keep one"
		case b:
			return plain, ""
		}
		return jsonc, ""
	}
	return "", ""
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// RecordPath is where a harness's ownership record lives.
func (e Env) RecordPath(h Harness) string {
	return filepath.Join(e.StateDir, "mcp", string(h)+".json")
}

// record is the state of what Flightdeck owns in one harness's config (spec 7.2).
type record struct {
	Harness Harness           `json:"harness"`
	Target  string            `json:"target"`
	Written time.Time         `json:"written"`
	Servers map[string]string `json:"servers"` // name -> entry digest
	// Pending holds the digests a write is about to put in the file. It is saved before the
	// rename (or before Claude's CLI runs) and cleared after, so a write cut short in between
	// still knows its entries.
	Pending map[string]string `json:"pending,omitempty"`
}

// loadRecord reads what Flightdeck owns in the file at path. A record written for another
// file (a changed $CODEX_HOME or $CLAUDE_CONFIG_DIR) owns nothing here; the plan warns.
func (r *Renderer) loadRecord(h Harness, path string, p *Plan) (map[string]string, map[string]string, error) {
	data, err := os.ReadFile(r.Env.RecordPath(h))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, nil, fmt.Errorf("record %s: %w", r.Env.RecordPath(h), err)
	}
	if rec.Target != path {
		if len(rec.Servers)+len(rec.Pending) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("the last render wrote %s, not this file; nothing here counts as Flightdeck's, and the servers there are left alone (a write here forgets them)", rec.Target))
		}
		return map[string]string{}, nil, nil
	}
	if rec.Servers == nil {
		rec.Servers = map[string]string{}
	}
	return rec.Servers, rec.Pending, nil
}

// current reads a harness's config file: the servers it holds, by name.
type current struct {
	servers map[string]map[string]any
	root    map[string]any
	toml    *tomlDoc
}

func readCurrent(h Harness, text string) (current, error) {
	c := current{servers: map[string]map[string]any{}}
	var servers any
	switch h {
	case instructions.Claude:
		if strings.TrimSpace(text) == "" {
			return c, nil
		}
		if err := json.Unmarshal([]byte(text), &c.root); err != nil {
			return c, err
		}
		servers = c.root["mcpServers"]
	case instructions.Codex:
		root, err := decodeTOML(text)
		if err != nil {
			return c, err
		}
		if c.toml, err = parseTOMLDoc(text); err != nil {
			return c, err
		}
		c.root = root
		servers = root["mcp_servers"]
	case instructions.Opencode:
		n, err := parseJSONC(text)
		if err != nil {
			return c, err
		}
		if n == nil {
			return c, nil
		}
		root, ok := n.value.(map[string]any)
		if !ok {
			return c, errors.New("the file is not an object")
		}
		c.root = root
		if mcp, ok := root["mcp"].(map[string]any); ok {
			servers = mcp["servers"]
		} else if root["mcp"] != nil {
			return c, errors.New("mcp is not an object")
		}
	}
	if servers == nil {
		return c, nil
	}
	m, ok := servers.(map[string]any)
	if !ok {
		return c, errors.New("the server table is not an object")
	}
	for name, v := range m {
		e, ok := v.(map[string]any)
		if !ok {
			e = map[string]any{"": v} // not a server object; still a name the user holds
		}
		c.servers[name] = e
	}
	return c, nil
}

// Plan renders one harness's servers and decides what a write would do. It writes nothing.
func (r *Renderer) Plan(h Harness) Plan {
	p := Plan{Harness: h, Enabled: r.Config.writeEnabled(h)}
	if !slices.Contains(Rendered, h) {
		p.Status, p.Reason = Refused, "no renderer for "+string(h)+" yet"
		return p
	}
	path, why := r.Env.TargetPath(h)
	p.Path = path
	if why != "" {
		p.Status, p.Reason = Refused, why
		return p
	}
	if !filepath.IsAbs(path) {
		p.Status, p.Reason = Refused, "the target path is not absolute; check the harness's directory variable"
		return p
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			p.Status, p.Reason = Refused, "the target is not a plain file (a link is never replaced)"
			return p
		}
		if info.Size() > maxConfigBytes {
			p.Status, p.Reason = Refused, "the target is larger than 8 MiB"
			return p
		}
		data, err := os.ReadFile(path)
		if err != nil {
			p.Status, p.Reason = Refused, err.Error()
			return p
		}
		p.raw, p.exists, p.mode = string(data), true, info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		p.Status, p.Reason = Refused, err.Error()
		return p
	}
	cur, err := readCurrent(h, p.raw)
	if err != nil {
		p.Status, p.Reason = Refused, "cannot read the file: "+err.Error()
		return p
	}
	owned, pending, err := r.loadRecord(h, path, &p)
	if err != nil {
		p.Status, p.Reason = Refused, err.Error()
		return p
	}
	p.owned = map[string]string{}
	for n := range pending {
		if _, ok := owned[n]; !ok {
			owned[n] = ""
		}
	}

	desired := map[string]ServerPlan{}
	for _, s := range r.Config.Servers {
		if !s.applies(h, r.Host) {
			continue
		}
		e, err := render(h, s)
		if err != nil {
			desired[s.Name] = ServerPlan{Name: s.Name, Action: NotRendered, Reason: err.Error()}
			continue
		}
		desired[s.Name] = ServerPlan{Name: s.Name, value: e.value, Notes: e.notes}
	}
	names := map[string]bool{}
	for n := range desired {
		names[n] = true
	}
	for n := range owned {
		names[n] = true
	}
	changes := map[string]map[string]any{} // name -> new entry; nil removes
	recordChanged := len(pending) > 0      // a write cut short: the record still needs its end
	for _, name := range sortedKeys(names) {
		sp, wanted := desired[name]
		if !wanted || sp.Action == NotRendered {
			sp.Name = name
		}
		dig, isOwned := owned[name]
		have, inFile := cur.servers[name]
		if pd, ok := pending[name]; ok {
			// A write that stopped between its record and its rename: either entry is ours.
			if !isOwned || inFile && entryDigest(have) == pd {
				dig = pd
			}
			isOwned = true
		}
		if isOwned {
			p.owned[name] = dig
		}
		sp.inFile = inFile
		edited := inFile && isOwned && entryDigest(have) != dig
		force := slices.Contains(r.Force, name)
		editable := !inFile || h != instructions.Codex || len(cur.toml.serverSections(name)) > 0
		switch {
		case wanted && sp.Action != NotRendered:
			switch {
			case !inFile:
				sp.Action = Add
				if isOwned {
					sp.Notes = append(sp.Notes, "removed from the file by hand since the last render; it is added back")
				}
			case !isOwned:
				sp.Action, sp.Reason = Blocked, "the config already has a server with this name that Flightdeck did not write"
			case canonical(have) == canonical(sp.value):
				sp.Action = Keep
				recordChanged = recordChanged || edited
			case !editable:
				sp.Action, sp.Reason = Blocked, "defined in a form the editor cannot change (use a [mcp_servers."+name+"] table)"
			case edited && !force:
				sp.Action, sp.Reason = Blocked, "edited by hand since the last render; -force "+name+" replaces it"
			default:
				sp.Action = Update
			}
		case isOwned:
			// Owned, and the registry no longer gives it to this harness on this host.
			if wanted {
				sp.Notes = append(sp.Notes, sp.Reason)
				sp.Reason = ""
			}
			switch {
			case !inFile:
				sp.Action = Forget
				recordChanged = true
			case !editable:
				sp.Action, sp.Reason = Blocked, "defined in a form the editor cannot change"
			case edited && !force:
				sp.Action, sp.Reason = Blocked, "edited by hand since the last render; -force "+name+" removes it"
			default:
				sp.Action = Remove
			}
		}
		switch sp.Action {
		case Add, Update:
			changes[name] = sp.value
		case Remove:
			changes[name] = nil
		case "":
			continue
		}
		p.Servers = append(p.Servers, sp)
	}
	if len(p.Servers) == 0 {
		p.Status = Nothing
		return p
	}
	if err := r.edit(&p, cur, changes); err != nil {
		p.Status, p.Reason = Refused, err.Error()
		return p
	}
	if len(changes) > 0 || recordChanged {
		p.Status = Ready
	} else {
		p.Status = UpToDate
	}
	if top := gitWorkTree(filepath.Dir(path)); top != "" && len(changes) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s is inside the git work tree %s; a rendered server list may name private hosts", path, top))
	}
	if d := filepath.Dir(path); len(changes) > 0 && !p.exists && !lexists(d) && p.Harness != instructions.Claude {
		p.NewDir = d
	}
	return p
}

// edit fills the plan's diff sides, and checks a Codex or opencode edit by decoding it.
func (r *Renderer) edit(p *Plan, cur current, changes map[string]map[string]any) error {
	switch p.Harness {
	case instructions.Claude:
		old := map[string]any{}
		for n, v := range cur.servers {
			old[n] = v
		}
		next := map[string]any{}
		for n, v := range old {
			next[n] = v
		}
		for n, v := range changes {
			if v == nil {
				delete(next, n)
			} else {
				next[n] = v
			}
		}
		p.Old, p.New, p.DiffName = jsonText(old, "")+"\n", jsonText(next, "")+"\n", "mcpServers"
		return nil
	case instructions.Codex:
		texts := map[string]string{}
		for n, v := range changes {
			if v != nil {
				texts[n] = tomlSectionText(n, v)
			} else {
				texts[n] = ""
			}
		}
		p.Old, p.New = p.raw, cur.toml.apply(texts)
		got, err := decodeTOML(p.New)
		if err != nil {
			return fmt.Errorf("the edit does not decode (%v); nothing is written", err)
		}
		return checkEdit(cur.root, got, changes, "mcp_servers")
	case instructions.Opencode:
		text := p.raw
		for _, n := range sortedKeys(changes) {
			var err error
			if text, err = setServer(text, n, changes[n]); err != nil {
				return err
			}
		}
		p.Old, p.New = p.raw, text
		n, err := parseJSONC(text)
		if err != nil || n == nil {
			return fmt.Errorf("the edit does not parse (%v); nothing is written", err)
		}
		got, _ := n.value.(map[string]any)
		return checkEdit(cur.root, got, changes, "mcp", "servers")
	}
	return nil
}

// checkEdit compares the edited file's values with the old ones plus the changes: nothing
// else may differ.
func checkEdit(old, got map[string]any, changes map[string]map[string]any, path ...string) error {
	want, _ := normalize(old).(map[string]any)
	if want == nil {
		want = map[string]any{}
	}
	m := want
	for _, k := range path {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	for n, v := range changes {
		if v == nil {
			delete(m, n)
		} else {
			m[n] = normalize(v)
		}
	}
	// Removing the last table may drop the empty parent from a TOML file.
	if len(path) == 1 && len(m) == 0 {
		if _, ok := got[path[0]]; !ok {
			delete(want, path[0])
		}
	}
	if canonical(want) != canonical(got) {
		return errors.New("the edit would change more than the servers Flightdeck owns; nothing is written")
	}
	return nil
}

// gitWorkTree returns the top of the git work tree holding dir, or "".
func gitWorkTree(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ErrNotEnabled is returned when a write names a harness the user did not opt in to.
var ErrNotEnabled = errors.New("harness is not listed in the mcp write list")

// ErrChanged is returned when the target changed between the plan and the rename.
var ErrChanged = errors.New("the file changed while rendering; run again")

// Write plans one harness and applies it, but only when the user opted in and the plan is
// Ready. Servers refused in the plan are left as they are.
func (r *Renderer) Write(ctx context.Context, h Harness) (Plan, error) {
	if !knownHarness(string(h)) {
		return Plan{}, fmt.Errorf("unknown harness %q", h)
	}
	if !slices.Contains(Rendered, h) {
		return Plan{}, fmt.Errorf("%s: no renderer for %s yet", h, h)
	}
	p := r.Plan(h)
	if !p.Enabled {
		return p, fmt.Errorf("%s: %w", h, ErrNotEnabled)
	}
	switch p.Status {
	case Ready:
	case UpToDate, Nothing:
		return p, nil
	default:
		return p, fmt.Errorf("%s: %s: %s", h, p.Status, p.Reason)
	}
	// The record comes first: a write cut short after it still knows its own entries.
	if err := r.writePending(p); err != nil {
		return p, fmt.Errorf("%s: the state record failed, nothing written: %w", h, err)
	}
	if h == instructions.Claude {
		got, err := r.writeClaude(ctx, p)
		if got != nil {
			if rerr := r.writeRecord(p, got); rerr != nil {
				err = errors.Join(err, fmt.Errorf("the state record failed: %w", rerr))
			}
		}
		if err != nil {
			return p, fmt.Errorf("%s: %w", h, err)
		}
		return p, nil
	}
	if p.New != p.raw {
		if err := writeKeepMode(p.Path, []byte(p.New), p.mode, p.exists, func() error {
			info, err := os.Lstat(p.Path)
			switch {
			case errors.Is(err, os.ErrNotExist):
				if p.exists {
					return ErrChanged
				}
				return nil
			case err != nil:
				return err
			case !info.Mode().IsRegular():
				return errors.New("the target is no longer a plain file")
			}
			data, err := os.ReadFile(p.Path)
			if err != nil {
				return err
			}
			if !p.exists || string(data) != p.raw {
				return ErrChanged
			}
			return nil
		}); err != nil {
			// Nothing was renamed: the record goes back to what Flightdeck owned.
			if rerr := r.saveRecord(record{Harness: p.Harness, Target: p.Path, Servers: nonEmpty(p.owned)}); rerr != nil {
				err = errors.Join(err, rerr)
			}
			return p, fmt.Errorf("%s: %w", h, err)
		}
	}
	digests := map[string]string{}
	for _, sp := range p.Servers {
		if sp.value != nil && (sp.Action == Add || sp.Action == Update || sp.Action == Keep) {
			digests[sp.Name] = entryDigest(sp.value)
		}
	}
	if err := r.writeRecord(p, digests); err != nil {
		return p, fmt.Errorf("%s: written, but the state record failed: %w", h, err)
	}
	return p, nil
}

// writeClaude drives Claude's own writer per changed server (spec 8.3, D15), then reads the
// file back for the digests of what Claude stored. When a command fails it still returns the
// digests of the servers done so far and of those not reached, so the record keeps what
// Flightdeck owns.
func (r *Renderer) writeClaude(ctx context.Context, p Plan) (map[string]string, error) {
	cli := r.Claude
	if cli == nil {
		cli = ExecClaude("claude")
	}
	run := func(args ...string) error {
		out, err := cli(ctx, args...)
		if err != nil {
			line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			return fmt.Errorf("claude %s: %v: %s", strings.Join(args[:2], " "), err, line)
		}
		return nil
	}
	done := map[string]bool{}
	var failed error
	for _, sp := range p.Servers {
		if sp.Action != Add && sp.Action != Update && sp.Action != Remove {
			continue
		}
		if sp.Action != Add {
			if failed = run("mcp", "remove", "--scope", "user", sp.Name); failed != nil {
				break
			}
		}
		if sp.Action != Remove {
			if failed = run("mcp", "add-json", "--scope", "user", sp.Name, jsonText(sp.value, "")); failed != nil {
				break
			}
		}
		done[sp.Name] = true
	}
	data, err := os.ReadFile(p.Path)
	if err == nil {
		var cur current
		if cur, err = readCurrent(instructions.Claude, string(data)); err == nil {
			out := map[string]string{}
			for _, sp := range p.Servers {
				have, inFile := cur.servers[sp.Name]
				switch {
				case !inFile:
					if done[sp.Name] && sp.Action != Remove {
						failed = errors.Join(failed, fmt.Errorf("claude did not store %s", sp.Name))
					}
				case sp.Action == Keep || done[sp.Name] && sp.Action != Remove:
					out[sp.Name] = entryDigest(have)
				case sp.Action == Update || sp.Action == Remove:
					// Not reached: the entry is still the one Flightdeck owned.
					if d := p.owned[sp.Name]; d != "" {
						out[sp.Name] = d
					}
				}
			}
			return out, failed
		}
	}
	return nil, errors.Join(failed, fmt.Errorf("reading %s back: %w", p.Path, err))
}

func (r *Renderer) writeRecord(p Plan, digests map[string]string) error {
	servers := map[string]string{}
	for _, sp := range p.Servers {
		if d, ok := digests[sp.Name]; ok {
			servers[sp.Name] = d
			continue
		}
		switch sp.Action {
		case Blocked, NotRendered:
			if d := p.owned[sp.Name]; d != "" {
				servers[sp.Name] = d
			}
		}
	}
	return r.saveRecord(record{Harness: p.Harness, Target: p.Path, Servers: servers})
}

// writePending saves, before a write, what Flightdeck owns now and the entries it is about to
// write.
func (r *Renderer) writePending(p Plan) error {
	servers := nonEmpty(p.owned)
	pending := map[string]string{}
	for _, sp := range p.Servers {
		if sp.Action == Add || sp.Action == Update {
			pending[sp.Name] = entryDigest(sp.value)
		}
	}
	return r.saveRecord(record{Harness: p.Harness, Target: p.Path, Servers: servers, Pending: pending})
}

func nonEmpty(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func (r *Renderer) saveRecord(rec record) error {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	rec.Written = now().UTC()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeKeepMode(r.Env.RecordPath(rec.Harness), append(data, '\n'), 0o600, false, nil)
}

// writeKeepMode writes data to a temporary file beside path with the given mode (0600 for a
// new file), syncs it, runs check, then renames it over path.
func writeKeepMode(path string, data []byte, mode os.FileMode, exists bool, check func() error) error {
	if !exists || mode == 0 {
		mode = 0o600
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".flightdeck-mcp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}

// maxDiffCells bounds the line-diff table; past it the report says the files differ.
const maxDiffCells = 4 << 20

// Report prints a plan as the dry run shows it.
func (p Plan) Report(w io.Writer) {
	fmt.Fprintf(w, "== %s: %s\n", p.Harness, p.Path)
	status := string(p.Status)
	if p.Reason != "" {
		status += ": " + p.Reason
	}
	fmt.Fprintf(w, "status: %s\n", status)
	if p.Status == Nothing || p.Status == Refused && len(p.Servers) == 0 {
		return
	}
	if p.Enabled {
		fmt.Fprintln(w, "write: enabled")
	} else {
		fmt.Fprintln(w, "write: not enabled (add it to the mcp write list)")
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	for _, sp := range p.Servers {
		line := fmt.Sprintf("server %s: %s", sp.Name, sp.Action)
		if sp.Reason != "" {
			line += ": " + sp.Reason
		}
		fmt.Fprintln(w, line)
		for _, n := range sp.Notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
	}
	if p.Status != Ready || p.Old == p.New {
		return
	}
	if p.NewDir != "" {
		fmt.Fprintf(w, "note: the directory %s will be created\n", p.NewDir)
	}
	oldName, newName := p.Path, p.Path+" (rendered)"
	if p.DiffName != "" {
		oldName, newName = p.Path+" "+p.DiffName, p.Path+" "+p.DiffName+" (rendered)"
	} else if !p.exists {
		oldName = "/dev/null"
	}
	if lines(p.Old)*lines(p.New) > maxDiffCells {
		fmt.Fprintf(w, "the files differ; too large for a line diff (%d and %d lines)\n", lines(p.Old), lines(p.New))
		return
	}
	fmt.Fprint(w, detect.UnifiedDiff(oldName, newName, redactSecrets(p.Old), redactSecrets(p.New)))
}

// secretValue finds a quoted value under a key named like a secret, in TOML or JSON text.
var secretValue = regexp.MustCompile(`(?i)("?([A-Za-z0-9_.-]*(?:token|key|secret|password|authorization)[A-Za-z0-9_.-]*)"?\s*[:=]\s*)("(?:[^"\\\n]|\\.)*"|'[^'\n]*')`)

// redactSecrets hides, for the dry run's diff only, values the user's own entries may hold
// under secret-looking keys. References (${NAME}, {env:NAME}, a variable named by an
// *_env_var key) stay visible.
func redactSecrets(text string) string {
	return secretValue.ReplaceAllStringFunc(text, func(m string) string {
		sm := secretValue.FindStringSubmatch(m)
		val := sm[3]
		if strings.Contains(strings.ToLower(sm[2]), "env_var") || strings.Contains(val, "${") || strings.Contains(val, "{env:") || len(val) <= 2 {
			return m
		}
		return sm[1] + `"<hidden>"`
	})
}

func lines(s string) int { return bytes.Count([]byte(s), []byte("\n")) + 1 }
