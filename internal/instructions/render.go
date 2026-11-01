package instructions

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
)

// FormatVersion is the renderer format version. It goes into the digest and the header, so a
// format change marks every rendered file stale.
const FormatVersion = 1

// headerFirst starts every rendered file; a file without it was not written by Flightdeck.
const headerFirst = "<!-- Rendered by Flightdeck. Do not edit; edit the sources and render again."

// CodexWarnBytes is the rendered size above which the Codex target warns: Codex shares a
// 32 KiB budget between the global file and the project files (spec 6.2, D5).
const CodexWarnBytes = 24 << 10

var digestRe = regexp.MustCompile(`digest: sha256:([0-9a-f]{64})`)

// Env holds the directories the targets live in. Nothing here is read from the process
// environment by the package; EnvFrom does that for the command, and tests build one by hand.
type Env struct {
	Home        string
	ClaudeDir   string // $CLAUDE_CONFIG_DIR, else ~/.claude
	CodexHome   string // $CODEX_HOME, else ~/.codex
	OpencodeDir string // $XDG_CONFIG_HOME/opencode, else ~/.config/opencode
	PiDir       string // $PI_CODING_AGENT_DIR, else ~/.pi/agent
	StateDir    string // Flightdeck's state directory
}

// EnvFrom fills an Env from a getenv function, the home directory and the state directory.
func EnvFrom(getenv func(string) string, home, state string) Env {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	cfg := filepath.Join(home, ".config")
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		// The XDG spec says a relative value is ignored.
		cfg = x
	}
	return Env{
		Home:        home,
		ClaudeDir:   or("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")),
		CodexHome:   or("CODEX_HOME", filepath.Join(home, ".codex")),
		OpencodeDir: filepath.Join(cfg, "opencode"),
		PiDir:       or("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent")),
		StateDir:    state,
	}
}

// TargetPath is the one file the renderer may write for a harness (spec 6.2). Project folders
// never appear here (D3).
func (e Env) TargetPath(h Harness) string {
	switch h {
	case Claude:
		return filepath.Join(e.ClaudeDir, "rules", "flightdeck.md")
	case Codex:
		return filepath.Join(e.CodexHome, "AGENTS.md")
	case Opencode:
		return filepath.Join(e.StateDir, "instructions", "opencode.md")
	case Pi:
		return filepath.Join(e.PiDir, "AGENTS.md")
	}
	return ""
}

// Status is what a target's plan allows.
type Status string

const (
	// Ready: the rendered text differs from the file on disk (or there is none); a write may go.
	Ready Status = "ready"
	// UpToDate: the file on disk carries the same digest.
	UpToDate Status = "up-to-date"
	// Nothing: no source applies to this harness, or the harness loads them all natively.
	Nothing Status = "nothing to render"
	// UserLoaded: the user's own setup already delivers instructions here; no write is offered.
	UserLoaded Status = "already loaded by the user's setup"
	// Refused: the target exists and Flightdeck did not write it, or it is not a plain file.
	Refused Status = "refused"
)

// Plan is one target's render: what would be written, against what is there.
type Plan struct {
	Harness  Harness
	Path     string
	Status   Status
	Reason   string // why the status, for every status but Ready and UpToDate
	Enabled  bool   // the user opted in to writing this target
	Digest   string // hex
	Text     string // the full rendered file, header included
	Old      string // the file on disk, when there is one
	NewDir   string // the target's directory, when a write would create it
	Visits   []Visit
	Warnings []string
}

// Skipped returns the visits that put no text in the render: missing, cycle, depth, duplicate
// and too-large files.
func (p Plan) Skipped() []Visit {
	var out []Visit
	for _, v := range p.Visits {
		if v.Marker != Read {
			out = append(out, v)
		}
	}
	return out
}

// Renderer plans and writes the global targets.
type Renderer struct {
	Config Config
	Env    Env
	Host   string           // this host's id, for the sources' hosts allow lists
	Now    func() time.Time // the header's render time; time.Now when nil
}

// PlanAll plans every harness's target. It reads and writes nothing but reads the sources and
// the files beside each target.
func (r *Renderer) PlanAll() []Plan {
	out := make([]Plan, 0, len(Harnesses))
	for _, h := range Harnesses {
		out = append(out, r.Plan(h))
	}
	return out
}

// Plan renders one harness's target and decides what a write would do.
func (r *Renderer) Plan(h Harness) Plan {
	p := r.plan(h)
	if p.Status != Ready && p.Status != UpToDate && p.Path != "" && staleRendered(p.Path) {
		// Our earlier render keeps loading even though no write is offered now (review fix 1).
		w := fmt.Sprintf("%s was rendered by Flightdeck earlier and the harness still loads it; remove it", p.Path)
		if hidden := r.hiddenBy(h); hidden != "" {
			w += fmt.Sprintf(": it hides %s", hidden)
		}
		p.Warnings = append(p.Warnings, w)
	}
	return p
}

func (r *Renderer) plan(h Harness) Plan {
	p := Plan{Harness: h, Path: r.Env.TargetPath(h), Enabled: r.Config.writeEnabled(h)}
	if !filepath.IsAbs(p.Path) {
		p.Status = Refused
		p.Reason = "the target path is not absolute; check the harness's directory variable (a relative or ~ value is not expanded)"
		return p
	}
	sources := r.Config.globalSources(h, r.Host)
	var kept []Source
	for _, s := range sources {
		if h == Claude && r.claudeNative(s.Path) {
			// Claude loads its own files and expands their imports itself (spec 6.2).
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		p.Status = Nothing
		if len(sources) > 0 {
			p.Reason = "Claude Code loads these sources itself"
		} else {
			p.Reason = "no global source applies to this harness on this host"
		}
		return p
	}

	ex := newExpander(r.Env.Home, importHops[h])
	if h == Claude {
		// Claude's own files and their imports already reach Claude. Expanding them first
		// marks every file they reach as seen, so a source they import becomes a duplicate
		// here instead of arriving twice (review fix 3). Their text is not rendered.
		for _, f := range r.claudeNativeFiles() {
			ex.expand(f, false)
		}
	}
	var body strings.Builder
	for _, s := range kept {
		text := ex.expand(s.Path, false)
		if text == "" {
			continue
		}
		if body.Len() > 0 {
			body.WriteString("\n")
		}
		body.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			body.WriteString("\n")
		}
	}
	p.Visits = ex.visits
	if h == Claude && body.Len() == 0 {
		p.Status, p.Reason = Nothing, "Claude Code loads these sources itself, through its own files' imports"
		return p
	}
	p.Digest = digest(h, ex.visits)
	p.Text = header(p.Digest, r.now(), ex.visits) + "\n" + body.String()

	if old, err := os.ReadFile(p.Path); err == nil {
		p.Old = string(old)
	}
	if _, err := os.Stat(filepath.Dir(p.Path)); errors.Is(err, fs.ErrNotExist) {
		p.NewDir = filepath.Dir(p.Path)
	}
	if repo := gitWorkTree(filepath.Dir(p.Path)); repo != "" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("the target sits inside the git work tree at %s; a rendered file may hold private text", repo))
	}
	if h == Codex && len(p.Text) > CodexWarnBytes {
		p.Warnings = append(p.Warnings, fmt.Sprintf("the rendered file is %d bytes; Codex shares a 32 KiB budget with project files and cuts what is over", len(p.Text)))
	}
	if h == Opencode {
		if w := r.opencodeListed(p.Path); w != "" {
			p.Warnings = append(p.Warnings, w)
		}
	}

	if reason := r.userLoader(h, kept); reason != "" {
		p.Status, p.Reason = UserLoaded, reason
		return p
	}
	if reason := checkTarget(p.Path); reason != "" {
		p.Status, p.Reason = Refused, reason
		return p
	}
	if m := digestRe.FindStringSubmatch(p.Old); m != nil && m[1] == p.Digest {
		p.Status = UpToDate
		return p
	}
	p.Status = Ready
	return p
}

func (r *Renderer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// claudeNative reports whether a source is a file Claude Code loads by itself: its global
// CLAUDE.md or a file in its user rules directory.
func (r *Renderer) claudeNative(path string) bool {
	real := realPath(path)
	dir := realPath(r.Env.ClaudeDir)
	if real == filepath.Join(dir, "CLAUDE.md") {
		return true
	}
	rel, err := filepath.Rel(filepath.Join(dir, "rules"), real)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// digest hashes the format version, the harness, then each visited file's real path, marker
// and, when read, its bytes, each field length-prefixed (spec 6.4). The time is not in it.
func digest(h Harness, visits []Visit) string {
	d := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		d.Write(n[:])
		d.Write(b)
	}
	field([]byte(fmt.Sprintf("flightdeck-instructions/%d", FormatVersion)))
	field([]byte(h))
	for _, v := range visits {
		field([]byte(v.Path))
		field([]byte(v.Marker))
		if v.Marker == Read {
			field(v.data)
		}
	}
	return hex.EncodeToString(d.Sum(nil))
}

// header is the rendered file's first lines. It names counts, never paths (D8).
func header(dig string, at time.Time, visits []Visit) string {
	var read, missing int
	for _, v := range visits {
		switch v.Marker {
		case Read:
			read++
		case Missing:
			missing++
		}
	}
	return fmt.Sprintf("%s\n     digest: sha256:%s  renderer: %d  at: %s\n     sources: %d read, %d missing -->\n",
		headerFirst, dig, FormatVersion, at.UTC().Format(time.RFC3339), read, missing)
}

func hasHeader(text string) bool { return strings.HasPrefix(text, headerFirst) }

// checkTarget says why an existing target must not be replaced: it is a link or not a plain
// file, or it carries no Flightdeck header. An absent target is fine.
func checkTarget(path string) string {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return err.Error()
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return "the target is a symbolic link; replacing it would break the link"
	}
	if !info.Mode().IsRegular() {
		return "the target is not a regular file"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if !hasHeader(string(data)) {
		return "the target exists and carries no Flightdeck header; Flightdeck never overwrites a file it did not write"
	}
	return ""
}

// userLoader looks for a setup that already delivers instructions to the harness: a native
// global file Flightdeck did not write, or a config, hook, plugin or extension file that names
// one of the sources (spec 6.2, D7). It returns the reason, or "".
func (r *Renderer) userLoader(h Harness, sources []Source) string {
	var natives, scan []string
	switch h {
	case Claude:
		scan = []string{filepath.Join(r.Env.ClaudeDir, "settings.json"), filepath.Join(r.Env.ClaudeDir, "hooks")}
	case Codex:
		natives = []string{filepath.Join(r.Env.CodexHome, "AGENTS.override.md"), filepath.Join(r.Env.CodexHome, "AGENTS.md")}
		scan = []string{filepath.Join(r.Env.CodexHome, "config.toml"), filepath.Join(r.Env.CodexHome, "hooks.json")}
	case Opencode:
		natives = []string{filepath.Join(r.Env.OpencodeDir, "AGENTS.md")}
		scan = []string{filepath.Join(r.Env.OpencodeDir, "opencode.json"), filepath.Join(r.Env.OpencodeDir, "opencode.jsonc"), filepath.Join(r.Env.OpencodeDir, "plugin"), filepath.Join(r.Env.OpencodeDir, "plugins")}
	case Pi:
		natives = []string{filepath.Join(r.Env.PiDir, "AGENTS.override.md"), filepath.Join(r.Env.PiDir, "AGENTS.md"), filepath.Join(r.Env.PiDir, "CLAUDE.md")}
		scan = []string{filepath.Join(r.Env.PiDir, "settings.json"), filepath.Join(r.Env.PiDir, "extensions")}
	}
	for _, n := range natives {
		data, err := os.ReadFile(n)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 || hasHeader(string(data)) {
			continue
		}
		return fmt.Sprintf("%s is the user's own file and the harness loads it", n)
	}
	needles := sourceNeedles(sources, r.Env.Home)
	for _, f := range scanFiles(scan) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				return fmt.Sprintf("%s names a source (%s); the user's setup loads it", f, n)
			}
		}
	}
	return ""
}

// sourceNeedles is each source path as a config file might spell it: absolute, real, and in
// ~/ form.
func sourceNeedles(sources []Source, home string) []string {
	var out []string
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	for _, s := range sources {
		add(s.Path)
		add(realPath(s.Path))
		if home != "" {
			if rel, err := filepath.Rel(home, s.Path); err == nil && !strings.HasPrefix(rel, "..") {
				add("~/" + filepath.ToSlash(rel))
			}
		}
	}
	return out
}

// scanFiles expands the scan list: a file as itself, a directory as its plain files one level
// down. Files over 1 MiB are left out.
func scanFiles(paths []string) []string {
	var out []string
	keep := func(p string, info fs.FileInfo) {
		if info.Mode().IsRegular() && info.Size() <= 1<<20 {
			out = append(out, p)
		}
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			keep(p, info)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				keep(filepath.Join(p, e.Name()), fi)
			}
		}
	}
	return out
}

// opencodeListed warns when opencode's global config does not list the rendered file in its
// instructions array. Flightdeck does not patch that array yet.
func (r *Renderer) opencodeListed(target string) string {
	for _, name := range []string{"opencode.json", "opencode.jsonc"} {
		data, err := os.ReadFile(filepath.Join(r.Env.OpencodeDir, name))
		if err != nil {
			continue
		}
		for _, n := range pathForms(target, r.Env.Home) {
			if strings.Contains(string(data), n) {
				return ""
			}
		}
	}
	return fmt.Sprintf("opencode loads this file only when its global config lists it in instructions; add %s there", target)
}

// gitWorkTree returns the top of the git work tree holding dir, or "". It walks up from the
// nearest existing directory and looks for a .git entry, so it needs no git binary.
func gitWorkTree(dir string) string {
	dir = realPath(dir)
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

// ErrNotEnabled is returned when a write names a target the user did not opt in to.
var ErrNotEnabled = errors.New("target is not listed in the instructions write list")

// Write renders one target and writes it, but only when the user opted in to that target and
// the plan is Ready. It writes a temporary file in the same directory, then renames it over the
// target, so a harness never reads half a file. The source list goes to the state directory.
func (r *Renderer) Write(h Harness) (Plan, error) {
	if !known(string(h)) {
		return Plan{}, fmt.Errorf("unknown target %q", h)
	}
	p := r.Plan(h)
	if !p.Enabled {
		return p, fmt.Errorf("%s: %w", h, ErrNotEnabled)
	}
	switch p.Status {
	case Ready:
	case UpToDate:
		return p, nil
	default:
		return p, fmt.Errorf("%s: %s: %s", h, p.Status, p.Reason)
	}
	if err := writeAtomic(p.Path, []byte(p.Text), func() error {
		if reason := checkTarget(p.Path); reason != "" {
			return errors.New(reason)
		}
		return nil
	}); err != nil {
		return p, fmt.Errorf("%s: %w", h, err)
	}
	if r.Env.StateDir != "" {
		if err := r.writeRecord(p); err != nil {
			return p, fmt.Errorf("%s: written, but the state record failed: %w", h, err)
		}
	}
	return p, nil
}

// record is the state kept beside the digest: the full source list the header leaves out.
type record struct {
	Harness  Harness       `json:"harness"`
	Target   string        `json:"target"`
	Digest   string        `json:"digest"`
	Renderer int           `json:"renderer"`
	Written  time.Time     `json:"written"`
	Sources  []recordVisit `json:"sources"`
}

type recordVisit struct {
	Path   string `json:"path"`
	Marker Marker `json:"marker"`
	Depth  int    `json:"depth"`
	From   string `json:"from,omitempty"`
}

// RecordPath is where a target's state record lives.
func (e Env) RecordPath(h Harness) string {
	return filepath.Join(e.StateDir, "instructions", string(h)+".json")
}

func (r *Renderer) writeRecord(p Plan) error {
	rec := record{Harness: p.Harness, Target: p.Path, Digest: p.Digest, Renderer: FormatVersion, Written: r.now().UTC()}
	for _, v := range p.Visits {
		rec.Sources = append(rec.Sources, recordVisit{Path: v.Path, Marker: v.Marker, Depth: v.Depth, From: v.From})
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(r.Env.RecordPath(p.Harness), append(data, '\n'), nil)
}

// writeAtomic writes data to a temporary file beside path (owner-only), syncs it, runs check,
// then renames it over path.
func writeAtomic(path string, data []byte, check func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".flightdeck-render-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
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

// Report prints a plan as the dry run shows it: the target, its status, warnings, skipped
// imports, and a unified diff against the file on disk.
func (p Plan) Report(w io.Writer) {
	fmt.Fprintf(w, "== %s: %s\n", p.Harness, p.Path)
	status := string(p.Status)
	if p.Reason != "" {
		status += ": " + p.Reason
	}
	fmt.Fprintf(w, "status: %s\n", status)
	if p.Status == Nothing {
		for _, warn := range p.Warnings {
			fmt.Fprintf(w, "warning: %s\n", warn)
		}
		return
	}
	if p.Enabled {
		fmt.Fprintln(w, "write: enabled")
	} else {
		fmt.Fprintln(w, "write: not enabled (add it to the instructions write list)")
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	for _, v := range p.Skipped() {
		line := fmt.Sprintf("skipped %s: %s", v.Marker, v.Path)
		if v.From != "" {
			line += " (imported from " + v.From + ")"
		}
		fmt.Fprintln(w, line)
	}
	if p.Status != Ready {
		return
	}
	if p.NewDir != "" {
		fmt.Fprintf(w, "note: the directory %s will be created\n", p.NewDir)
	}
	old := "/dev/null"
	if p.Old != "" {
		old = p.Path
	}
	if lines(p.Old)*lines(p.Text) > maxDiffCells {
		fmt.Fprintf(w, "the files differ; too large for a line diff (%d and %d lines)\n", lines(p.Old), lines(p.Text))
		return
	}
	fmt.Fprint(w, detect.UnifiedDiff(old, p.Path+" (rendered)", p.Old, p.Text))
}

func lines(s string) int { return strings.Count(s, "\n") + 1 }

// pathForms is a path as a config file might spell it: absolute, real, and in ~/ form.
func pathForms(p, home string) []string {
	out := []string{p}
	if r := realPath(p); r != p {
		out = append(out, r)
	}
	if home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			out = append(out, "~/"+filepath.ToSlash(rel))
		}
	}
	return out
}

// staleRendered reports whether a plain file Flightdeck rendered sits at path.
func staleRendered(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && hasHeader(string(data))
}

// hiddenBy names the user's own file that a rendered target hides from the harness, or "".
// Only pi has one: its AGENTS.md wins over a CLAUDE.md in the same directory.
func (r *Renderer) hiddenBy(h Harness) string {
	if h != Pi {
		return ""
	}
	f := filepath.Join(r.Env.PiDir, "CLAUDE.md")
	if data, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(data))) > 0 && !hasHeader(string(data)) {
		return f
	}
	return ""
}

// claudeNativeFiles lists the files Claude Code loads by itself at user scope: its CLAUDE.md,
// then every .md under its rules directory, by name, leaving out Flightdeck's own target.
func (r *Renderer) claudeNativeFiles() []string {
	var out []string
	if f := filepath.Join(r.Env.ClaudeDir, "CLAUDE.md"); fileExists(f) {
		out = append(out, f)
	}
	target := r.Env.TargetPath(Claude)
	var rules []string
	filepath.WalkDir(filepath.Join(r.Env.ClaudeDir, "rules"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") && p != target {
			rules = append(rules, p)
		}
		return nil
	})
	return append(out, rules...)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
