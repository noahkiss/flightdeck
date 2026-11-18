package flightrules

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/opencode"
)

// OpencodeAPI is the part of an opencode server's API the ledger reads. *opencode.Client
// implements it.
type OpencodeAPI interface {
	Location(ctx context.Context) (opencode.Location, error)
	Config(ctx context.Context) ([]opencode.ConfigDoc, error)
	Skills(ctx context.Context) ([]opencode.Skill, error)
	MCPs(ctx context.Context) ([]opencode.MCP, error)
	Messages(ctx context.Context, session, cursor string, limit int) (opencode.MessagePage, error)
}

// opencodePage is how many messages one page asks for.
const opencodePage = 100

// opencodeInstructionsHead starts each file's block in the text opencode sends the model.
const opencodeInstructionsHead = "Instructions from: "

// Opencode reads the ledger of an opencode session through its server (spec 11.2, 11.4). The
// server keeps no record of the files and lists it put in the system prompt. So the source
// runs opencode's instruction discovery and reads the skill and MCP lists from the server on
// each update: attribution computed. The session's messages add what opencode recorded: nested
// instruction files it loaded, invoked skills (recorded), and change notices (unknown).
type Opencode struct {
	session   string
	client    func() (OpencodeAPI, bool) // the session's server, while it runs
	home      string                     // the user's home folder
	configDir string                     // opencode's global config folder
	now       func() time.Time

	seen     map[string]bool   // message ids read
	computed map[string]string // by entry id: the source of each computed entry of the last update

	mu    sync.Mutex
	where map[string]ocWhere // by entry id: where its text is read again
}

// ocWhere is where an entry's text comes from: a file on disk, the skill list, or a part of a
// message.
type ocWhere struct {
	file    string
	skills  bool
	message string
	part    int
	digest  string
}

// NewOpencode returns a source for the opencode session id, read through the server client
// returns while the session's opencode runs. home is the user's home folder and configDir
// opencode's global config folder ($XDG_CONFIG_HOME/opencode, else ~/.config/opencode).
func NewOpencode(session string, client func() (OpencodeAPI, bool), home, configDir string) *Opencode {
	return &Opencode{session: session, client: client, home: home, configDir: configDir, now: time.Now,
		seen: map[string]bool{}, computed: map[string]string{}, where: map[string]ocWhere{}}
}

// Harness implements Source.
func (o *Opencode) Harness() string { return "opencode" }

// Update implements Source: it reads the messages added since the last call, then runs the
// discovery and reads the lists again. A server that is not running leaves the ledger as it is.
func (o *Opencode) Update(ctx context.Context, l *Ledger) error {
	c, ok := o.client()
	if !ok {
		l.Warn("offline", "the session's opencode server is not running; the ledger shows what it read before")
		return nil
	}
	if err := o.messages(ctx, c, l); err != nil {
		return err
	}
	return o.current(ctx, c, l)
}

// messages reads the session's messages that are new since the last call, oldest first.
func (o *Opencode) messages(ctx context.Context, c OpencodeAPI, l *Ledger) error {
	var fresh []json.RawMessage
	cursor := ""
	for {
		page, err := c.Messages(ctx, o.session, cursor, opencodePage)
		if err != nil {
			return err
		}
		known := false
		for _, raw := range page.Data {
			var id struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &id) != nil || id.ID == "" {
				continue
			}
			if o.seen[id.ID] {
				known = true
				break
			}
			fresh = append(fresh, raw)
		}
		if known || page.Cursor.Next == "" || len(page.Data) == 0 {
			break
		}
		cursor = page.Cursor.Next
	}
	for i := len(fresh) - 1; i >= 0; i-- {
		m, ok := parseOcMessage(fresh[i])
		if !ok {
			continue
		}
		o.seen[m.ID] = true
		for j, ob := range ocObservations(m) {
			l.Observe(ob)
			o.keep(ob, ocWhere{message: m.ID, part: j})
		}
	}
	return nil
}

// keep notes where an entry's text is read again.
func (o *Opencode) keep(ob Observation, w ocWhere) {
	if ob.Placeholder {
		return
	}
	w.digest = Digest(ob.Text)
	o.mu.Lock()
	o.where[ob.ID()] = w
	o.mu.Unlock()
}

// current runs the discovery and reads the skill and MCP lists. An entry no longer found is
// gone; a new one is added. An entry still found is left as it is: a poll is not a load.
func (o *Opencode) current(ctx context.Context, c OpencodeAPI, l *Ledger) error {
	loc, err := c.Location(ctx)
	if err != nil {
		return err
	}
	docs, err := c.Config(ctx)
	if err != nil {
		return err
	}
	skills, err := c.Skills(ctx)
	if err != nil {
		return err
	}
	mcps, err := c.MCPs(ctx)
	if err != nil {
		return err
	}
	at := o.now().UTC().Format(time.RFC3339)
	type item struct {
		ob Observation
		w  ocWhere
	}
	var items []item
	files, urls := opencodeDiscover(o.home, o.configDir, loc.Directory, loc.Project.Directory, docs)
	for _, f := range files {
		items = append(items, item{Observation{Kind: api.FlightRuleInstruction, Scope: f.scope, Source: f.path, Text: f.text,
			At: at, Attribution: api.FlightComputed,
			Note: "opencode does not record which files it loaded; Flightdeck ran its discovery and read the file as it is now"}, ocWhere{file: f.path}})
	}
	for _, u := range urls {
		items = append(items, item{Observation{Kind: api.FlightRuleInstruction, Source: u, Placeholder: true, At: at,
			Note: "opencode fetches this address itself; Flightdeck does not, so its text is unknown"}, ocWhere{}})
	}
	if len(skills) > 0 {
		items = append(items, item{Observation{Kind: api.FlightRuleSkills, Scope: api.FlightScopeSkill, Source: "skill listing",
			Text: skillListing(skills), At: at, Attribution: api.FlightComputed,
			Note: fmt.Sprintf("%d skills. %s", len(skills), opencodeListNote)}, ocWhere{skills: true}})
	}
	for _, m := range mcps {
		if m.Status.Status != "connected" || m.Name == "" {
			continue
		}
		items = append(items, item{Observation{Kind: api.FlightRuleMCP, Scope: api.FlightScopeMCP, Source: m.Name, Placeholder: true,
			At: at, Note: "Connected. opencode's server does not return a server's instructions, so their text is unknown"}, ocWhere{}})
	}
	now := map[string]string{}
	for _, it := range items {
		now[it.ob.ID()] = it.ob.Source
	}
	for id, src := range o.computed {
		if _, ok := now[id]; !ok {
			l.Remove(Removal{Kinds: []api.FlightRuleKind{api.FlightRuleInstruction, api.FlightRuleSkills, api.FlightRuleMCP}, Source: src, At: at})
		}
	}
	for _, it := range items {
		if _, ok := o.computed[it.ob.ID()]; ok {
			continue
		}
		l.Observe(it.ob)
		o.keep(it.ob, it.w)
	}
	o.computed = now
	return nil
}

// opencodeListNote is the note on an entry built from a list the server returned.
const opencodeListNote = "Built from the list opencode's server returns; the digest covers that list, not the words the model saw"

// skillListing is the skill list as text: one line per skill, by name.
func skillListing(skills []opencode.Skill) string {
	lines := make([]string, 0, len(skills))
	for _, s := range skills {
		lines = append(lines, fmt.Sprintf("- %s: %s (%s)", s.Name, s.Description, s.Path))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Text implements Source: a file's text read again, the skill list fetched again, or a message
// part read again from the server. A text that changed since is ErrNoEntry.
func (o *Opencode) Text(ctx context.Context, id string) (string, error) {
	o.mu.Lock()
	w, ok := o.where[id]
	o.mu.Unlock()
	if !ok {
		return "", ErrNoEntry
	}
	check := func(text string) (string, error) {
		if Digest(text) != w.digest {
			return "", ErrNoEntry
		}
		return text, nil
	}
	switch {
	case w.file != "":
		data, err := os.ReadFile(w.file)
		if err != nil {
			return "", ErrNoEntry
		}
		return check(string(data))
	}
	c, ok := o.client()
	if !ok {
		return "", fmt.Errorf("the session's opencode server is not running")
	}
	if w.skills {
		skills, err := c.Skills(ctx)
		if err != nil {
			return "", err
		}
		return check(skillListing(skills))
	}
	cursor := ""
	for {
		page, err := c.Messages(ctx, o.session, cursor, opencodePage)
		if err != nil {
			return "", err
		}
		for _, raw := range page.Data {
			m, ok := parseOcMessage(raw)
			if !ok || m.ID != w.message {
				continue
			}
			obs := ocObservations(m)
			if w.part >= len(obs) {
				return "", ErrNoEntry
			}
			return check(obs[w.part].Text)
		}
		if page.Cursor.Next == "" || len(page.Data) == 0 {
			return "", ErrNoEntry
		}
		cursor = page.Cursor.Next
	}
}

// ocMessage is the part of an opencode message the ledger reads.
type ocMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created float64 `json:"created"`
	} `json:"time"`
	Text        string `json:"text"`
	Description string `json:"description"`
	Name        string `json:"name"`
	Metadata    struct {
		Instruction struct {
			Paths []string `json:"paths"`
		} `json:"instruction"`
		Notice  string   `json:"notice"`
		Sources []string `json:"instructionSources"`
	} `json:"metadata"`
}

func parseOcMessage(raw json.RawMessage) (ocMessage, bool) {
	var m ocMessage
	if json.Unmarshal(raw, &m) != nil || m.ID == "" {
		return m, false
	}
	return m, true
}

// ocObservations is what one message carried. Its order is stable: Text finds a part by index.
func ocObservations(m ocMessage) []Observation {
	at := ""
	if ms := m.Time.Created; ms > 0 && !math.IsInf(ms, 0) {
		at = time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339Nano)
	}
	switch {
	case m.Type == "skill":
		o := Observation{Kind: api.FlightRuleSkill, Scope: api.FlightScopeSkill, Source: m.Name, Text: m.Text, At: at,
			Attribution: api.FlightRecorded}
		if m.Name == "" {
			o.Source, o.Attribution, o.Note = "skill", api.FlightUnknown, "opencode recorded an invoked skill without its name"
		}
		return []Observation{o}
	case m.Type == "synthetic" && len(m.Metadata.Instruction.Paths) > 0:
		const note = "Loaded when the agent read a file in this file's folder"
		parts, ok := splitInstructionBlocks(m.Text, m.Metadata.Instruction.Paths)
		if !ok {
			return []Observation{{Kind: api.FlightRuleInstruction, Scope: api.FlightScopeProject, Source: "nested instructions",
				Text: m.Text, At: at, Attribution: api.FlightUnknown,
				Note: "opencode named " + strings.Join(m.Metadata.Instruction.Paths, ", ") + " for this text, which Flightdeck could not split by file"}}
		}
		out := make([]Observation, 0, len(parts))
		for i, p := range m.Metadata.Instruction.Paths {
			out = append(out, Observation{Kind: api.FlightRuleInstruction, Scope: api.FlightScopeProject, Source: p,
				Text: parts[i], At: at, Attribution: api.FlightRecorded, Note: note})
		}
		return out
	case m.Type == "system" && m.Metadata.Notice == "instructions":
		what := strings.Join(m.Metadata.Sources, ", ")
		if what == "" {
			what = "the instructions"
		}
		return []Observation{{Kind: api.FlightRuleInstruction, Scope: api.FlightScopeSession, Source: "instructions changed",
			Text: m.Text, At: at, Attribution: api.FlightUnknown,
			Note: "opencode recorded a change notice for " + what + "; it names the files only in its text, and may hold a diff rather than the files"}}
	}
	return nil
}

// splitInstructionBlocks cuts text made of "Instructions from: <path>" blocks, one per path in
// order, into each file's text. ok is false when the text is not made that way.
func splitInstructionBlocks(text string, paths []string) ([]string, bool) {
	starts := make([]int, len(paths))
	from := 0
	for i, p := range paths {
		head := opencodeInstructionsHead + p + "\n"
		j := strings.Index(text[from:], head)
		if j < 0 || (i == 0 && j != 0) {
			return nil, false
		}
		starts[i] = from + j
		from += j + len(head)
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		begin := starts[i] + len(opencodeInstructionsHead) + len(p) + 1
		end := len(text)
		if i+1 < len(paths) {
			end = starts[i+1]
			out[i] = strings.TrimSuffix(text[begin:end], "\n\n")
			continue
		}
		out[i] = text[begin:end]
	}
	return out, true
}

// ocFile is one instruction file opencode's discovery finds, with its text.
type ocFile struct {
	path  string
	scope api.FlightRuleScope
	text  string
}

// opencodeDiscover runs opencode 2.3's instruction discovery as it stands now (its
// packages/core config instruction plugin): the global AGENTS.md in its config folder, else
// ~/.claude/CLAUDE.md; then, when dir is inside the project, AGENTS.md (else CLAUDE.md) in each
// folder from dir up to the home folder when dir is under it, else to the project root; then
// the `instructions` entries of each config document. An http or https entry is returned in
// urls: opencode fetches it, Flightdeck does not.
func opencodeDiscover(home, configDir, dir, project string, docs []opencode.ConfigDoc) (files []ocFile, urls []string) {
	seen := map[string]bool{}
	add := func(p string, scope api.FlightRuleScope) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if seen[p] {
			return
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		seen[p] = true
		files = append(files, ocFile{path: p, scope: scope, text: string(data)})
	}
	for _, g := range []string{filepath.Join(configDir, "AGENTS.md"), filepath.Join(home, ".claude", "CLAUDE.md")} {
		if info, err := os.Stat(g); err == nil && info.Mode().IsRegular() {
			add(g, api.FlightScopeUser)
			break
		}
	}
	inProject := dir != "" && project != "" && within(project, dir)
	if inProject {
		stop := project
		if home != "" && within(home, dir) {
			stop = home
		}
		for d := dir; ; d = filepath.Dir(d) {
			if fileExists(filepath.Join(d, "AGENTS.md")) {
				add(filepath.Join(d, "AGENTS.md"), api.FlightScopeProject)
			} else if fileExists(filepath.Join(d, "CLAUDE.md")) {
				add(filepath.Join(d, "CLAUDE.md"), api.FlightScopeProject)
			}
			if d == stop || filepath.Dir(d) == d {
				break
			}
		}
	}
	for _, doc := range docs {
		if doc.Type != "document" {
			continue
		}
		scope := api.FlightScopeProject
		if configDir != "" && within(configDir, doc.Path) {
			scope = api.FlightScopeUser
		}
		for _, src := range doc.Info.Instructions {
			if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
				if !slices.Contains(urls, src) {
					urls = append(urls, src)
				}
				continue
			}
			if strings.HasPrefix(src, "~/") {
				src = filepath.Join(home, src[2:])
			}
			var matches []string
			switch {
			case filepath.IsAbs(src):
				matches, _ = filepath.Glob(src)
			case inProject:
				// A relative pattern is searched from the location up to the project root.
				for d := dir; ; d = filepath.Dir(d) {
					m, _ := filepath.Glob(filepath.Join(d, src))
					matches = append(matches, m...)
					if d == project || filepath.Dir(d) == d {
						break
					}
				}
			default:
				matches, _ = filepath.Glob(filepath.Join(configDir, src))
			}
			sort.Strings(matches)
			for _, p := range matches {
				add(p, scope)
			}
		}
	}
	return files, urls
}

// within says whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
