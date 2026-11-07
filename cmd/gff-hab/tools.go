package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// The agent kit's tools (capcom.md 4, 3.3): two tables serve the CLI (`gff-hab read` for the
// read tools, `gff-hab act` for the action tools), the MCP server (`gff-hab mcp`) and the printed
// manual (`gff-hab mcp --skill`). Every read tool reads and none writes; the action tools ask the
// hab for a start or a set-up, which the hab runs, after the user's confirmation when the
// setting asks for one.

// toolArg is one argument of a read tool.
type toolArg struct {
	Name     string
	Type     string // "string" or "integer"
	Doc      string
	Required bool
}

// kitTool is one tool. run returns the result for -json and its text.
type kitTool struct {
	Name   string
	Doc    string
	Args   []toolArg
	Action bool // an action tool: it asks the hab to change something
	// Shell is the manual's shell line when the tool has a command of its own; "" is
	// `gff-hab read <tool>` or `gff-hab act <tool>`.
	Shell string
	// Time bounds one MCP call; 0 is the default for a read or an action tool.
	Time time.Duration
	run  func(ctx context.Context, c caller, a toolArgs) (any, string, error)
}

// toolArgs are a call's arguments, checked against the tool's own.
type toolArgs map[string]any

func (a toolArgs) str(name string) string {
	switch v := a[name].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (a toolArgs) num(name string) int64 {
	switch v := a[name].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// errWrite is the refusal of an argument a read tool does not take: read tools never write.
type errWrite struct{ tool, arg string }

func (e errWrite) Error() string {
	return fmt.Sprintf("refused: %s is a read tool; it takes no argument %q and never writes", e.tool, e.arg)
}

// check refuses an argument the tool does not take, a missing required one, and a value of
// the wrong type.
func (t kitTool) check(a toolArgs) error {
	for name := range a {
		if !slices.ContainsFunc(t.Args, func(x toolArg) bool { return x.Name == name }) {
			if t.Action {
				return fmt.Errorf("refused: %s takes no argument %q", t.Name, name)
			}
			return errWrite{t.Name, name}
		}
	}
	for _, x := range t.Args {
		v, ok := a[x.Name]
		if !ok || v == nil {
			if x.Required {
				return fmt.Errorf("%s needs %s", t.Name, x.Name)
			}
			continue
		}
		switch x.Type {
		case "integer":
			switch n := v.(type) {
			case float64:
				if n != float64(int64(n)) {
					return fmt.Errorf("%s: %s must be a whole number", t.Name, x.Name)
				}
			case int, int64, json.Number:
			case string:
				if _, err := strconv.ParseInt(n, 10, 64); err != nil {
					return fmt.Errorf("%s: %s must be a whole number", t.Name, x.Name)
				}
			default:
				return fmt.Errorf("%s: %s must be a whole number", t.Name, x.Name)
			}
		default:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s: %s must be a string", t.Name, x.Name)
			}
			if x.Required && v.(string) == "" {
				return fmt.Errorf("%s needs %s", t.Name, x.Name)
			}
		}
	}
	return nil
}

var (
	argHost     = toolArg{Name: "host", Type: "string", Doc: "A host's label from the hosts tool. Absent: every host."}
	argHostOne  = toolArg{Name: "host", Type: "string", Doc: "A host's label from the hosts tool. Absent: this host."}
	argProject  = toolArg{Name: "project", Type: "string", Doc: "A project name, or a folder."}
	argHarness  = toolArg{Name: "harness", Type: "string", Doc: "claude or codex."}
	argSince    = toolArg{Name: "since", Type: "string", Doc: "RFC 3339 time: only transcripts written at or after it."}
	argLimit    = toolArg{Name: "limit", Type: "integer", Doc: "Most results to return."}
	argSession  = toolArg{Name: "session", Type: "string", Doc: "A session id from sessions or transcript_search, or a Flightdeck session name on that host.", Required: true}
	argWordsReq = toolArg{Name: "words", Type: "string", Doc: "Words that must all appear in a message, any case. Double quotes keep a phrase together.", Required: true}
)

// readTools is every read tool, in the order the manual lists them.
var readTools = []kitTool{
	{Name: "hosts", Doc: "List the connected hosts: online or offline, OS, arch, cores, memory, capabilities, harnesses, toolchains, services and the always-on mark.",
		run: runHosts},
	{Name: "projects", Doc: "List the projects on every host and where each is checked out: folder, branch, head and whether it has changes.",
		Args: []toolArg{{Name: "project", Type: "string", Doc: "Only projects whose name contains this."}}, run: runProjects},
	{Name: "agents", Doc: "List the running agents on every host: session name, harness, project, state and since when.",
		Args: []toolArg{argHost, {Name: "project", Type: "string", Doc: "Only agents in this project."}}, run: runAgents},
	{Name: "sessions", Doc: "List past and current agent sessions from their transcripts, newest first: host, harness, id, folder, project, title, and the live session when one runs.",
		Args: []toolArg{argHost, argProject, argHarness, argSince, {Name: "until", Type: "string", Doc: "RFC 3339 time: only sessions started before it."}, argLimit}, run: runSessions},
	{Name: "transcript_search", Doc: "Search past transcripts on every host by words, newest first. Returns at most 50 hits, each with its host, session and message id and a snippet. Each host gets 5 s; a host that ran out is marked partial, an offline host offline.",
		Args: []toolArg{argWordsReq, argHost, argProject, argHarness, argSince, argLimit}, run: runSearch},
	{Name: "transcript_read", Doc: "Read a page of one session's messages, oldest first. Pass before with the first message id of a page to read the page before it.",
		Args: []toolArg{argSession, argHostOne, argHarness, {Name: "before", Type: "string", Doc: "A message id: read the messages before it."}, argLimit}, run: runTranscript},
	{Name: "notice_read", Doc: "Read a Flightdeck notification by id, and the session it is about.",
		Args: []toolArg{{Name: "id", Type: "string", Doc: "The notification id.", Required: true}, argHostOne}, run: runNotice},
	{Name: "file_read", Doc: "Read a text file on a host, inside that host's read scope. Returns at most 256 KiB per call; pass offset to read on.",
		Args: []toolArg{{Name: "path", Type: "string", Doc: "An absolute path, or one under ~/ on that host.", Required: true}, argHostOne,
			{Name: "offset", Type: "integer", Doc: "Byte offset to start at."}, {Name: "limit", Type: "integer", Doc: "Most bytes to return."}}, run: runFile},
	{Name: "logbook_brief", Doc: "The Logbook brief: what to know about a project now. Tasks in doing and blocked, handed over and ready ones, pinned guides, findings from the last 7 days, open plans and records for every project, one line each, at most 8 KB. In a pane its project is the default.",
		Args: []toolArg{{Name: "project", Type: "string", Doc: "A Logbook project id. Absent: the pane's project."}}, run: runLogbookBrief},
	{Name: "logbook_search", Doc: "Search or list Logbook records: tasks, findings, issues, plans, guides and proposals, on every host (each hab holds them all). With words, best match first; without, newest change first.",
		Args: []toolArg{{Name: "words", Type: "string", Doc: "Words to search for. Absent: list records."}, {Name: "type", Type: "string", Doc: "A record type."},
			{Name: "status", Type: "string", Doc: "A status."}, {Name: "project", Type: "string", Doc: "A Logbook project id."}, {Name: "tag", Type: "string", Doc: "A tag."},
			{Name: "author", Type: "string", Doc: "Records written by this session id, device or harness."}, argLimit}, run: runLogbookSearch},
	{Name: "logbook_read", Doc: "Read one Logbook record with its links both ways, notes and flags.",
		Args: []toolArg{{Name: "id", Type: "string", Doc: "The record id.", Required: true}}, run: runLogbookRead},
	{Name: "suggest_host", Doc: "Rank the connected hosts for new work: hosts that are offline or lack a needed capability or the harness are left out, each with why; the rest go best first, the project checked out and ready on the right branch first, then the host the user named, then the one used last for the project. When none fits it says why and names the host to set the project up on.",
		Args: []toolArg{{Name: "project", Type: "string", Doc: "The project: a name, a remote, or a folder. Absent: work outside a project."},
			{Name: "branch", Type: "string", Doc: "The branch the work wants."},
			{Name: "needs", Type: "string", Doc: "Capability words the host must have, comma separated, such as xcode or gpu."},
			argHarness, {Name: "host", Type: "string", Doc: "A host the user named: preferred when it fits."}}, run: runSuggest},
	{Name: "confirmation_read", Doc: "Read where a start or set-up you asked for stands, by its confirmation id: open, running, done, failed, cancelled, expired or unknown, and what it made.",
		Args: []toolArg{{Name: "id", Type: "string", Doc: "The confirmation id start_session returned.", Required: true}}, run: runConfirmationRead},
}

// actionTools is every action tool, in the order the manual lists them.
var actionTools = []kitTool{
	{Name: "start_session", Action: true, Doc: "Start an agent session in a project folder on a host, seeded with your brief as its first message. With the owner's confirmation setting on, the owner confirms it in Flightdeck first, and may edit it; the tool waits up to 45 s and then says it is still waiting; the answer then reaches you as a message from Flightdeck. Returns the new session's name and host, or why it did not start.",
		Args: []toolArg{{Name: "folder", Type: "string", Doc: "The absolute project folder on that host, from projects or suggest_host.", Required: true},
			{Name: "harness", Type: "string", Doc: "claude, codex or opencode.", Required: true},
			{Name: "host", Type: "string", Doc: "A host's label from the hosts tool. Absent: this host."},
			{Name: "model", Type: "string", Doc: "A model for the harness, as the harness names it."},
			{Name: "name", Type: "string", Doc: "The new session's name. Absent: one made from the harness and the confirmation id."},
			{Name: "seed", Type: "string", Doc: "The first message: the goal, decisions so far, open questions and where they came from. At most 8000 characters. The new session sees nothing else of this conversation."}},
		run: runStart},
	{Name: "spawn", Action: true, Doc: "Start a teammate or sub-agent for your own work: a new agent session in any harness, on this host or another, seeded with your brief. Use it when the user asks for another agent to help you, such as \"a codex reviewer on this diff\". It starts in your own project folder unless you name one. As a teammate (the default), Flightdeck sends you its final message as a channel message from it each time a turn that your seed or your message began ends; channel_wait waits for it, channel_send gives it more work. Detached, nothing comes back on its own. A start on another host may wait for the owner's confirmation, as start_session does. Returns its name and channel address. Run it from your Flightdeck pane.",
		Args: []toolArg{{Name: "harness", Type: "string", Doc: "claude, codex or opencode, or an alias from the owner's table, such as gpt or opus: it picks the harness and its model. An unknown word is refused with the list.", Required: true},
			{Name: "seed", Type: "string", Doc: "The brief: the role, the task, what to look at and what to report. At most 8000 characters. The new session sees nothing else of this conversation.", Required: true},
			{Name: "folder", Type: "string", Doc: "The absolute project folder on that host. Absent: your own folder here, or the same project's folder on the other host."},
			{Name: "host", Type: "string", Doc: "A host's label from the hosts tool. Absent: this host."},
			{Name: "model", Type: "string", Doc: "A model for the harness, as the harness names it. It wins over the alias's model."},
			{Name: "name", Type: "string", Doc: "The new session's name. Absent: one made from the harness and the confirmation id."},
			{Name: "mode", Type: "string", Doc: "teammate (the default): its turn reports come to you. detached: nothing comes back on its own."}},
		run: runSpawn},
	{Name: "hand_off", Action: true, Doc: "Send the user's request to the agent already running on a project, as an agent message from your session. No confirmation is asked. It picks the agent the user named, else an idle one, else the most recently active, and says why. An idle agent gets the message at once; a working or blocked one gets it when idle; one on an offline host gets it when the host is back. Returns where it went and how delivery stands, or that no agent runs there. Run it from a Flightdeck pane: your session is the sender.",
		Args: []toolArg{{Name: "project", Type: "string", Doc: "The project: a name, a remote, or a folder, from projects or agents.", Required: true},
			{Name: "message", Type: "string", Doc: "The request in the user's words, plus the context you gathered. The agent sees nothing else of this conversation.", Required: true},
			{Name: "agent", Type: "string", Doc: "The agent the user named: its session name, or host/name."},
			{Name: "host", Type: "string", Doc: "A host's label from the hosts tool: only agents there."}},
		run: runHandOff},
	{Name: "file_record", Action: true, Doc: "File a Logbook record (a task, finding, issue, plan or guide) as a proposal: it waits for the user's review, and nothing is in Logbook until the user approves it. Until this host has Logbook it answers unavailable and files nothing.",
		Args: []toolArg{{Name: "type", Type: "string", Doc: "task, finding, issue, plan or guide.", Required: true},
			{Name: "title", Type: "string", Doc: "One line, at most 200 characters.", Required: true},
			{Name: "quote", Type: "string", Doc: "The user's words in this conversation the record came from.", Required: true},
			{Name: "body", Type: "string", Doc: "Markdown, at most 256 KiB."},
			{Name: "project", Type: "string", Doc: "The project: a name, a remote, or a folder. Absent: a record that belongs to no project."},
			{Name: "source_message", Type: "string", Doc: "The id of the message the quote is from, when you know it."}},
		run: runFileRecord},
	{Name: "logbook_write", Action: true, Doc: "Write to Logbook, by action: create a record (a task, finding, issue, plan or guide), edit one, add a note, mark a status, claim a task for your session, link two records, or supersede a finding. The write lands at once and the user sees it; a Capcom session's writes wait for review as proposals instead. A finding's title and body never change: supersede it. Each call writes; to retry a call whose answer you did not get, send it again with the same key.",
		Args: []toolArg{{Name: "action", Type: "string", Doc: "create, edit, note, mark, claim, link or supersede.", Required: true},
			{Name: "id", Type: "string", Doc: "The record: for edit, note, mark, claim, link, and the finding that supersede replaces."},
			{Name: "type", Type: "string", Doc: "create: task, finding, issue, plan or guide."},
			{Name: "title", Type: "string", Doc: "create, supersede, edit: one line, at most 200 characters."},
			{Name: "body", Type: "string", Doc: "create, supersede, edit: Markdown, at most 256 KiB."},
			{Name: "text", Type: "string", Doc: "note: the note."},
			{Name: "status", Type: "string", Doc: "mark, edit, create: open, doing, blocked, shelved, done or dropped for a task or issue."},
			{Name: "tags", Type: "string", Doc: "create, edit: tags to add, comma separated."},
			{Name: "link", Type: "string", Doc: "link: kind:id, such as blocks:task-k3m9q2xz. Kinds: supersedes, stage, part_of, blocks, relates, from, duplicates."},
			{Name: "project", Type: "string", Doc: "create: a Logbook project id. Absent: a record that belongs to no project."},
			{Name: "key", Type: "string", Doc: "Your own name for this write, at most 128 bytes: the same key again writes nothing new. Absent: a fresh key, so every call writes."}},
		run: runLogbookWrite},
	{Name: "compact", Action: true, Doc: "Compact your own conversation and carry on: once your turn ends, Flightdeck sends /compact to your harness, waits for the compaction to finish, then sends your follow-up as your first message after it. Write a handoff file first and name it in the follow-up. End your turn right after this call. Claude Code only. Run it from your Flightdeck pane.",
		Args: []toolArg{{Name: "follow_up", Type: "string", Doc: "Your first message after the compaction, such as: read <handoff path> and continue. At most 8000 characters."}},
		run:  runCompact},
	{Name: "set_up_project", Action: true, Doc: "Set a project up on a host: clone or fetch its repository, check out a branch and run its bootstrap. Not built yet: it answers unavailable.",
		Args: []toolArg{{Name: "host", Type: "string", Doc: "A host's label from the hosts tool.", Required: true},
			{Name: "repository", Type: "string", Doc: "The repository to clone or fetch.", Required: true},
			{Name: "branch", Type: "string", Doc: "The branch to check out."}},
		run: runSetup},
}

// allTools is every tool, read tools first.
func allTools() []kitTool {
	return append(append(slices.Clip(readTools), actionTools...), channelTools...)
}

func findIn(tools []kitTool, name string) (kitTool, bool) {
	name = strings.ReplaceAll(name, "-", "_")
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return kitTool{}, false
}

// findTool finds a read tool.
func findTool(name string) (kitTool, bool) { return findIn(readTools, name) }

// --- Runs ---

// hostsResult is the hosts tool's -json result.
type hostsResult struct {
	Catalog api.CatalogList `json:"catalog"`
	Peers   *api.PeerList   `json:"peers,omitempty"`
}

func readCatalog(ctx context.Context, c caller) (api.CatalogList, error) {
	var cl api.CatalogList
	err := c.Call(ctx, "catalog.list", api.CatalogListParams{}, &cl)
	return cl, err
}

func runHosts(ctx context.Context, c caller, _ toolArgs) (any, string, error) {
	cl, err := readCatalog(ctx, c)
	if err != nil {
		return nil, "", err
	}
	res := hostsResult{Catalog: cl}
	var pl api.PeerList
	if err := c.Call(ctx, "peers.list", api.PeersListParams{}, &pl); err == nil {
		res.Peers = &pl
	}
	return res, formatHosts(res), nil
}

func hostName(h api.CatalogEntry) string {
	switch {
	case h.Own:
		return h.Catalog.Host + " (this host)"
	case h.Label != "":
		return h.Label
	}
	return h.Catalog.Host + " (unlisted)"
}

func formatHosts(r hostsResult) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, h := range r.Catalog.Hosts {
		seen[h.Catalog.DeviceID] = true
		c := h.Catalog
		state := "online"
		if h.Stale {
			state = "offline, last seen " + h.SeenAt
		}
		sys := strings.Join(strings.Fields(c.System.OS+" "+c.System.OSVersion+" "+c.System.Arch), " ")
		fmt.Fprintf(&b, "%s: %s; %s, %d cores", hostName(h), state, sys, c.System.Cores)
		if c.System.MemoryBytes > 0 {
			fmt.Fprintf(&b, ", %d GB", c.System.MemoryBytes>>30)
		}
		if c.AlwaysOn {
			b.WriteString("; always on")
		}
		b.WriteByte('\n')
		var caps []string
		for _, x := range c.Capabilities {
			caps = append(caps, x.Name)
		}
		line(&b, "capabilities", caps)
		line(&b, "harnesses", tools(c.Harnesses))
		line(&b, "toolchains", tools(c.Toolchains))
		var svcs []string
		for _, s := range c.Services {
			svcs = append(svcs, s.Name+" "+s.URL)
		}
		line(&b, "services", svcs)
	}
	if r.Peers != nil {
		for _, p := range r.Peers.Peers {
			if p.DeviceID != "" && seen[p.DeviceID] {
				continue
			}
			fmt.Fprintf(&b, "%s: %s, never reached", p.Label, p.State)
			if p.Reason != "" {
				fmt.Fprintf(&b, " (%s)", p.Reason)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func line(b *strings.Builder, name string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(b, "  %s: %s\n", name, strings.Join(items, ", "))
	}
}

func tools(ts []api.CatalogTool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, strings.TrimSpace(t.Name+" "+t.Version))
	}
	return out
}

func runProjects(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	cl, err := readCatalog(ctx, c)
	if err != nil {
		return nil, "", err
	}
	labels := map[string]string{}
	for _, h := range cl.Hosts {
		labels[h.Catalog.DeviceID] = hostName(h)
	}
	var b strings.Builder
	var out []api.CatalogProjectGroup
	for _, p := range cl.Projects {
		if f := a.str("project"); f != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(f)) {
			continue
		}
		out = append(out, p)
		id := p.Remote
		if id == "" && p.RootCommit != "" {
			id = "root " + p.RootCommit[:min(12, len(p.RootCommit))]
		}
		fmt.Fprintf(&b, "%s", p.Name)
		if id != "" {
			fmt.Fprintf(&b, " (%s)", id)
		}
		b.WriteByte('\n')
		for _, l := range p.Locations {
			host := l.Label
			if host == "" {
				host = labels[l.DeviceID]
			}
			if l.Stale {
				host += ", offline"
			}
			for _, f := range l.Folders {
				var bits []string
				if f.Branch != "" {
					bits = append(bits, f.Branch)
				}
				if f.Head != "" {
					bits = append(bits, f.Head[:min(12, len(f.Head))])
				}
				if f.Dirty != nil && *f.Dirty {
					bits = append(bits, "changes")
				}
				fmt.Fprintf(&b, "  %s: %s [%s]\n", host, f.Path, strings.Join(bits, ", "))
			}
		}
	}
	if b.Len() == 0 {
		b.WriteString("no projects\n")
	}
	return api.NonNil(out), b.String(), nil
}

// agentRow is one running agent, for the agents tool's -json result.
type agentRow struct {
	Host string `json:"host"`
	api.CatalogAgent
	Offline bool `json:"offline,omitempty" doc:"The host is offline; the row is from its last catalog."`
}

func runAgents(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	cl, err := readCatalog(ctx, c)
	if err != nil {
		return nil, "", err
	}
	var rows []agentRow
	host, project := a.str("host"), a.str("project")
	for _, h := range cl.Hosts {
		name := h.Label
		if name == "" {
			name = h.Catalog.Host
		}
		if host != "" && host != name && host != h.Catalog.Host {
			continue
		}
		for _, ag := range h.Catalog.Agents {
			if project != "" && !strings.EqualFold(ag.Project, project) {
				continue
			}
			rows = append(rows, agentRow{Host: name, CatalogAgent: ag, Offline: h.Stale})
		}
	}
	var b strings.Builder
	for _, r := range rows {
		proj := r.Project
		if proj == "" {
			proj = "-"
		}
		fmt.Fprintf(&b, "%s/%s: %s, %s, project %s, since %s", r.Host, r.Session, r.Harness, r.State, proj, r.Since)
		if r.Offline {
			b.WriteString(" (host offline: last known)")
		}
		b.WriteByte('\n')
	}
	if len(rows) == 0 {
		b.WriteString("no running agents\n")
	}
	return api.NonNil(rows), b.String(), nil
}

func runSessions(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.TranscriptListParams{Host: a.str("host"), Project: a.str("project"), Harness: a.str("harness"),
		Since: a.str("since"), Until: a.str("until"), Limit: int(a.num("limit"))}
	var r api.TranscriptSessions
	if err := c.Call(ctx, "transcript.list", p, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, s := range r.Sessions {
		where := s.Project
		if where == "" {
			where = s.Folder
		}
		fmt.Fprintf(&b, "%s %s %s, updated %s, %s", s.Host, s.Harness, s.ID, s.Updated, where)
		if s.Title != "" {
			fmt.Fprintf(&b, ": %q", s.Title)
		}
		if s.Live != nil {
			fmt.Fprintf(&b, " [running as %s, %s]", s.Live.Name, s.Live.State)
		}
		b.WriteByte('\n')
	}
	if len(r.Sessions) == 0 {
		b.WriteString("no sessions\n")
	}
	if r.More {
		b.WriteString("more sessions match: narrow with project, harness, since or host\n")
	}
	hostLines(&b, r.Hosts)
	return r, b.String(), nil
}

// hostLines says which hosts did not answer in full, and what each host's scope left out.
func hostLines(b *strings.Builder, hosts []api.ReadHost) {
	for _, h := range hosts {
		if h.State != api.ReadHostStateOk {
			fmt.Fprintf(b, "host %s: %s", h.Label, h.State)
			if h.Reason != "" {
				fmt.Fprintf(b, " (%s)", h.Reason)
			}
			b.WriteByte('\n')
		}
		if h.LeftOut > 0 {
			fmt.Fprintf(b, "host %s: its read scope left out %d transcripts\n", h.Label, h.LeftOut)
		}
	}
}

func runSearch(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.TranscriptSearchParams{Words: a.str("words"), Host: a.str("host"), Project: a.str("project"),
		Harness: a.str("harness"), Since: a.str("since"), Limit: int(a.num("limit"))}
	var r api.TranscriptHits
	if err := c.Call(ctx, "transcript.search", p, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, h := range r.Hits {
		where := h.Project
		if where == "" {
			where = h.Folder
		}
		fmt.Fprintf(&b, "%s %s session %s message %s, %s, %s, %s: %s\n", h.Host, h.Harness, h.Session, h.Message, h.At, where, h.Kind, h.Snippet)
	}
	if len(r.Hits) == 0 {
		b.WriteString("no hits\n")
	}
	if r.More {
		b.WriteString("more hits match: add words, or narrow with project, harness, since or host\n")
	}
	hostLines(&b, r.Hosts)
	return r, b.String(), nil
}

func runTranscript(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.TranscriptReadParams{Session: a.str("session"), Host: a.str("host"), Harness: a.str("harness"),
		Before: a.str("before"), Limit: int(a.num("limit"))}
	var r api.TranscriptPage
	if err := c.Call(ctx, "transcript.read", p, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s session %s in %s\n", r.Host, r.Harness, r.Session, r.Folder)
	if r.More && len(r.Messages) > 0 {
		fmt.Fprintf(&b, "older messages: pass before=%s\n", r.Messages[0].ID)
	}
	for _, m := range r.Messages {
		fmt.Fprintf(&b, "[%s] %s %s", m.At, m.ID, m.Kind)
		switch {
		case m.Tool != nil:
			fmt.Fprintf(&b, " %s %s (%s)", m.Tool.Name, m.Tool.Target, m.Tool.Status)
			if m.Tool.Output != "" {
				fmt.Fprintf(&b, ":\n%s", indent(m.Tool.Output))
			}
		case len(m.Plan) > 0:
			for _, s := range m.Plan {
				fmt.Fprintf(&b, "\n  - [%s] %s", s.Status, s.Text)
			}
		case m.Text != "":
			fmt.Fprintf(&b, ":\n%s", indent(m.Text))
		}
		b.WriteByte('\n')
	}
	return r, b.String(), nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

func runNotice(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.NoticeRecord
	if err := c.Call(ctx, "notice.get", api.NoticeGetParams{ID: a.str("id"), Host: a.str("host")}, &r); err != nil {
		return nil, "", err
	}
	n := r.Notice
	var b strings.Builder
	fmt.Fprintf(&b, "%s notice %s, %s, at %s: %s\n", r.Host, n.ID, n.Level, n.At, n.Title)
	if n.Body != "" {
		fmt.Fprintf(&b, "%s\n", n.Body)
	}
	if r.Session != nil {
		fmt.Fprintf(&b, "about session %s (%s), %s\n", r.Session.Name, r.Session.ID, r.Session.State)
	} else if n.Session != "" {
		fmt.Fprintf(&b, "about session %s, which no longer has a record\n", n.Session)
	}
	return r, b.String(), nil
}

func runFile(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.FileReadParams{Path: a.str("path"), Host: a.str("host"), Offset: a.num("offset"), Limit: int(a.num("limit"))}
	var r api.FileData
	if err := c.Call(ctx, "file.read", p, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, %d bytes, from offset %d\n", r.Host, r.Path, r.Size, r.Offset)
	switch {
	case r.Binary:
		b.WriteString("not a text file: nothing returned\n")
	default:
		b.WriteString(r.Text)
		if r.Text != "" && !strings.HasSuffix(r.Text, "\n") {
			b.WriteByte('\n')
		}
		if r.More {
			fmt.Fprintf(&b, "more: pass offset=%d\n", r.Offset+int64(len(r.Text)))
		}
	}
	return r, b.String(), nil
}

func runLogbookBrief(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.LogbookBrief
	if err := c.Call(ctx, "logbook.brief", api.LogbookBriefParams{Project: a.str("project")}, &r); err != nil {
		return nil, "", err
	}
	return r, r.Text, nil
}

func runLogbookSearch(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	p := api.LogbookFindParams{Words: a.str("words"), Type: a.str("type"), Status: a.str("status"),
		Project: a.str("project"), Tag: a.str("tag"), Author: a.str("author"), Limit: int(a.num("limit"))}
	var r api.LogbookRecords
	if err := c.Call(ctx, "logbook.find", p, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, x := range r.Records {
		fmt.Fprintf(&b, "%s %s %s: %s\n", x.ID, x.Type, x.Status, x.Title)
	}
	if len(r.Records) == 0 {
		b.WriteString("no records\n")
	}
	return r, b.String(), nil
}

func runLogbookRead(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.LogbookRecordResult
	if err := c.Call(ctx, "logbook.get", api.LogbookGetParams{ID: a.str("id")}, &r); err != nil {
		return nil, "", err
	}
	x := r.Record
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s: %s\n", x.ID, x.Type, x.Status, x.Title)
	if x.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", x.Project)
	}
	if len(x.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(x.Tags, ", "))
	}
	if x.Assignee != "" {
		fmt.Fprintf(&b, "assignee: %s\n", x.Assignee)
	}
	for _, s := range x.SupersededBy {
		fmt.Fprintf(&b, "superseded by: %s\n", s)
	}
	for _, l := range x.Links {
		fmt.Fprintf(&b, "link: %s %s\n", l.Kind, l.To)
	}
	for _, l := range x.Backlinks {
		fmt.Fprintf(&b, "linked from: %s %s\n", l.To, l.Kind)
	}
	for _, f := range x.Flags {
		fmt.Fprintf(&b, "flag: %s %s\n", f.Kind, f.Field)
	}
	if x.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(x.Body, "\n"))
	}
	for _, n := range x.Notes {
		fmt.Fprintf(&b, "\nnote %s:\n%s\n", n.At, strings.TrimRight(n.Text, "\n"))
	}
	return r, b.String(), nil
}

// --- Reaching the hab ---

// toolCaller reaches the hab for one read: -socket when given, else GFF_HAB from a pane, else
// $GFF_HAB_SOCKET or the default control socket.
func toolCaller(ctx context.Context, sock string, getenv func(string) string) (caller, func(), error) {
	if sock == "" && getenv(hook.Env) != "" {
		return hookCaller{getenv(hook.Env)}, func() {}, nil
	}
	c, err := open(sock, "")
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, func() { c.Close() }, nil
}

// --- gff-hab read ---

// readCmd is `gff-hab read [-socket path] [-json] <tool> [-arg value]... [words...]`.
func readCmd(args []string, out io.Writer) error {
	return toolCmd("read", readTools, 40*time.Second, args, out)
}

// actCmd is `gff-hab act [-socket path] [-json] <tool> [-arg value]... [words...]`: the action
// tools.
func actCmd(args []string, out io.Writer) error {
	return toolCmd("act", actionTools, actionCallTime, args, out)
}

// toolCmd runs one tool of tools from the command line.
func toolCmd(verb string, tools []kitTool, wait time.Duration, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	sock := socketFlag(fs)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	timeout := fs.Duration("timeout", wait, "give up after this long")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: gff-hab %s [-socket path] [-json] <tool> [-arg value]... [value of the first required argument]\n", verb)
		for _, t := range tools {
			fmt.Fprintf(fs.Output(), "  %s\n", t.Name)
		}
	}
	fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("%s needs a tool", verb)
	}
	t, ok := findIn(tools, fs.Arg(0))
	if !ok {
		return fmt.Errorf("no %s tool %q; gff-hab mcp --skill lists them", verb, fs.Arg(0))
	}
	tfs := flag.NewFlagSet(verb+" "+t.Name, flag.ExitOnError)
	vals := map[string]*string{}
	for _, x := range t.Args {
		vals[x.Name] = tfs.String(x.Name, "", x.Doc)
	}
	tfs.Parse(fs.Args()[1:])
	a := toolArgs{}
	tfs.Visit(func(f *flag.Flag) { a[f.Name] = *vals[f.Name] })
	if rest := tfs.Args(); len(rest) > 0 {
		var first string
		for _, x := range t.Args {
			if x.Required {
				first = x.Name
				break
			}
		}
		if first == "" || a[first] != nil {
			return fmt.Errorf("%s takes no words beyond its flags", t.Name)
		}
		a[first] = strings.Join(rest, " ")
	}
	if err := t.check(a); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, done, err := toolCaller(ctx, *sock, os.Getenv)
	if err != nil {
		return err
	}
	defer done()
	res, text, err := t.run(ctx, c, a)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", b)
		return err
	}
	_, err = io.WriteString(out, text)
	return err
}
