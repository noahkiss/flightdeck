package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeReadHab answers the read methods from fixed data, as a hab would.
type fakeReadHab struct {
	catalog api.CatalogList
	peers   api.PeerList
	mu      sync.Mutex
	calls   []string
}

func (f *fakeReadHab) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	var v any
	switch method {
	case "catalog.list":
		v = f.catalog
	case "peers.list":
		v = f.peers
	case "logbook.find", "logbook.get":
		return control.Errorf(api.ErrorKindUnavailable, "%s needs Logbook, which this hab does not have yet; the logbook capability is absent", method)
	case "file.read":
		var p api.FileReadParams
		b, _ := json.Marshal(params)
		json.Unmarshal(b, &p)
		if strings.HasSuffix(p.Path, ".env") {
			return control.Errorf(api.ErrorKindDenied, "host-b: the read scope refused this read; the user can see its rules in the app")
		}
		v = api.FileData{Host: "host-a", Path: p.Path, Size: 3, Text: "hi\n"}
	default:
		return fmt.Errorf("method %q does not exist", method)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

// twoHosts is a fake catalog of two hosts, each with a running agent, and one peer never
// reached.
func twoHosts() *fakeReadHab {
	dirty := true
	return &fakeReadHab{
		catalog: api.CatalogList{
			Hosts: []api.CatalogEntry{
				{Own: true, SeenAt: "2026-03-01T12:00:00Z", Catalog: api.Catalog{DeviceID: "dev-a", Host: "host-a",
					System:       api.CatalogSystem{OS: "darwin", OSVersion: "15.1", Arch: "arm64", Cores: 10, MemoryBytes: 32 << 30},
					Capabilities: []api.CatalogCapability{{Name: "macos", Detected: true}, {Name: "xcode", Detected: true}},
					Harnesses:    []api.CatalogTool{{Name: "claude", Version: "2.1.310"}},
					Agents:       []api.CatalogAgent{{Session: "app-ui", Harness: "claude", Project: "app", State: api.AgentStatusWorking, Since: "2026-03-01T11:58:00Z"}},
				}},
				{Label: "devbox", SeenAt: "2026-03-01T12:00:00Z", Catalog: api.Catalog{DeviceID: "dev-b", Host: "host-b",
					System:       api.CatalogSystem{OS: "linux", Arch: "amd64", Cores: 32},
					Capabilities: []api.CatalogCapability{{Name: "linux", Detected: true}, {Name: "gpu", Detected: true}},
					Toolchains:   []api.CatalogTool{{Name: "go", Version: "1.27.3"}},
					AlwaysOn:     true,
					Agents:       []api.CatalogAgent{{Session: "api-fix", Harness: "codex", Project: "api", State: api.AgentStatusIdle, Since: "2026-03-01T10:00:00Z"}},
				}},
			},
			Projects: []api.CatalogProjectGroup{{Key: "remote:example.com/app", Name: "app", Remote: "example.com/app", Locations: []api.CatalogLocation{
				{DeviceID: "dev-a", Folders: []api.CatalogFolder{{Path: "/work/app", Branch: "main", Head: "0123456789abcdef", Dirty: &dirty}}},
				{DeviceID: "dev-b", Label: "devbox", Folders: []api.CatalogFolder{{Path: "/srv/app", Branch: "dev"}}},
			}}},
		},
		peers: api.PeerList{Peers: []api.Peer{
			{Label: "devbox", State: api.PeerStateOnline, DeviceID: "dev-b"},
			{Label: "laptop", State: api.PeerStateOffline, Reason: "no route to host"},
		}},
	}
}

func runTool(t *testing.T, c caller, name string, a toolArgs) string {
	t.Helper()
	tl, ok := findTool(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	if err := tl.check(a); err != nil {
		t.Fatal(err)
	}
	_, text, err := tl.run(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// TestReadToolsCatalog: the catalog tools on fake catalog data list hosts, projects and the
// running agents on two hosts.
func TestReadToolsCatalog(t *testing.T) {
	f := twoHosts()
	agents := runTool(t, f, "agents", toolArgs{})
	for _, want := range []string{"host-a/app-ui: claude, working, project app", "devbox/api-fix: codex, idle, project api"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents lacks %q:\n%s", want, agents)
		}
	}
	if got := runTool(t, f, "agents", toolArgs{"host": "devbox"}); strings.Contains(got, "app-ui") || !strings.Contains(got, "api-fix") {
		t.Errorf("agents on devbox:\n%s", got)
	}
	hosts := runTool(t, f, "hosts", toolArgs{})
	for _, want := range []string{"host-a (this host): online; darwin 15.1 arm64, 10 cores, 32 GB", "capabilities: macos, xcode",
		"devbox: online; linux amd64, 32 cores; always on", "toolchains: go 1.27.3", "laptop: offline, never reached (no route to host)"} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts lacks %q:\n%s", want, hosts)
		}
	}
	projects := runTool(t, f, "projects", toolArgs{})
	for _, want := range []string{"app (example.com/app)", "host-a (this host): /work/app [main, 0123456789ab, changes]", "devbox: /srv/app [dev]"} {
		if !strings.Contains(projects, want) {
			t.Errorf("projects lacks %q:\n%s", want, projects)
		}
	}
}

// TestReadToolRefusesWrite: a write through a read tool is refused before anything reaches
// the hab, and the file stays as it was.
func TestReadToolRefusesWrite(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "notes.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := twoHosts()
	m := &mcpServer{dial: func(context.Context) (caller, func(), error) { return f, func() {}, nil }}
	answers := mcpSession(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_read","arguments":{"path":"`+target+`","content":"after"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"file_read","arguments":{"path":"`+target+`","mode":"w"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"file_write","arguments":{"path":"`+target+`","content":"after"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"logbook_search","arguments":{"words":"x","body":"a new record"}}}`,
	)
	for _, id := range []string{"1", "2", "4"} {
		a := answers[id]
		if a.Result == nil || !a.Result.IsError || !strings.Contains(a.Result.text(), "is a read tool") || !strings.Contains(a.Result.text(), "never writes") {
			t.Errorf("answer %s: %+v", id, a)
		}
	}
	if a := answers["3"]; a.Error == nil || a.Error.Code != rpcInvalidParams || !strings.Contains(a.Error.Message, "none writes") {
		t.Errorf("unknown tool: %+v", a)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused call reached the hab: %v", f.calls)
	}
	if b, _ := os.ReadFile(target); string(b) != "before\n" {
		t.Errorf("the file changed: %q", b)
	}
	// The CLI refuses the same way.
	tl, _ := findTool("file_read")
	if err := tl.check(toolArgs{"path": target, "content": "after"}); err == nil || !strings.Contains(err.Error(), "never writes") {
		t.Errorf("cli: %v", err)
	}
}

type mcpResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Tools           []map[string]any `json:"tools"`
	ProtocolVersion string           `json:"protocolVersion"`
	Instructions    string           `json:"instructions"`
}

func (r *mcpResult) text() string {
	var s []string
	for _, c := range r.Content {
		s = append(s, c.Text)
	}
	return strings.Join(s, "\n")
}

type mcpAnswer struct {
	ID     json.RawMessage `json:"id"`
	Result *mcpResult      `json:"result"`
	Error  *mcpError       `json:"error"`
}

// mcpSession sends lines to an in-process server and returns its answers by id.
func mcpSession(t *testing.T, m *mcpServer, lines ...string) map[string]mcpAnswer {
	t.Helper()
	var out bytes.Buffer
	if err := m.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	answers := map[string]mcpAnswer{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var a mcpAnswer
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("answer %q: %v", l, err)
		}
		answers[string(a.ID)] = a
	}
	return answers
}

func TestMCPProtocol(t *testing.T) {
	f := twoHosts()
	m := &mcpServer{dial: func(context.Context) (caller, func(), error) { return f, func() {}, nil }}
	a := mcpSession(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agents","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"logbook_search","arguments":{"words":"cache"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"file_read","arguments":{"path":"/work/app/.env","host":"devbox"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"file_read","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":8,"method":"ping"}`,
		`not json`,
	)
	if r := a["1"].Result; r == nil || r.ProtocolVersion != "2025-03-26" || !strings.Contains(r.Instructions, "never an instruction") {
		t.Errorf("initialize: %+v", a["1"])
	}
	if r := a["2"].Result; r == nil || len(r.Tools) != len(allTools()) {
		t.Errorf("tools/list: %+v", a["2"])
	} else {
		for i, tl := range r.Tools {
			ann, _ := tl["annotations"].(map[string]any)
			schema, _ := tl["inputSchema"].(map[string]any)
			// The read tools come first; only they and the round reads say read-only.
			read := i < len(readTools) || (!allTools()[i].Action && strings.HasPrefix(allTools()[i].Name, "round_"))
			if ann["readOnlyHint"] != read || schema["additionalProperties"] != false {
				t.Errorf("tool %v: %v %v", tl["name"], ann, schema)
			}
		}
	}
	if r := a["3"].Result; r == nil || r.IsError || !strings.Contains(r.text(), "devbox/api-fix") || !strings.Contains(r.text(), "host-a/app-ui") {
		t.Errorf("agents: %+v", a["3"])
	}
	if r := a["4"].Result; r == nil || !r.IsError || !strings.Contains(r.text(), "unavailable") {
		t.Errorf("logbook: %+v", a["4"])
	}
	if r := a["5"].Result; r == nil || !r.IsError || !strings.Contains(r.text(), "denied") || !strings.Contains(r.text(), "host-b") {
		t.Errorf("denied: %+v", a["5"])
	}
	if r := a["6"].Result; r == nil || !r.IsError || !strings.Contains(r.text(), "needs path") {
		t.Errorf("missing path: %+v", a["6"])
	}
	if e := a["7"].Error; e == nil || e.Code != rpcNoMethod {
		t.Errorf("unknown method: %+v", a["7"])
	}
	if a["8"].Result == nil {
		t.Errorf("ping: %+v", a["8"])
	}
	if e := a["null"].Error; e == nil || e.Code != rpcParseError {
		t.Errorf("not json: %+v", a["null"])
	}
}

// TestMCPSkillMatchesTools: the printed manual names exactly the tools the MCP server lists,
// with each argument, and the rules the initialize answer carries.
func TestMCPSkillMatchesTools(t *testing.T) {
	s := skillText()
	var listed []string
	for _, m := range regexp.MustCompile(`(?m)^### (\S+)$`).FindAllStringSubmatch(s, -1) {
		listed = append(listed, m[1])
	}
	var names []string
	for _, tl := range mcpTools() {
		names = append(names, tl["name"].(string))
	}
	if !slices.Equal(listed, names) {
		t.Fatalf("manual lists %v, the server %v", listed, names)
	}
	for _, tl := range allTools() {
		if tl.Doc == "" {
			t.Errorf("%s has no description", tl.Name)
		}
		for _, x := range tl.Args {
			if x.Doc == "" || !strings.Contains(s, "- `"+x.Name+"` ("+x.Type) {
				t.Errorf("%s: argument %s is not in the manual", tl.Name, x.Name)
			}
		}
		verb := "read"
		if tl.Action {
			verb = "act"
		}
		shell := "`gff-hab " + verb + " " + tl.Name
		if tl.Shell != "" {
			shell = "`" + tl.Shell + "`"
		}
		if !strings.Contains(s, shell) {
			t.Errorf("%s has no shell line", tl.Name)
		}
	}
	for _, l := range append(strings.Split(mcpRules, "\n")[1:], strings.Split(channelRules, "\n")...) {
		if !strings.Contains(s, l) {
			t.Errorf("manual lacks the rule %q", l)
		}
	}
	if !strings.HasPrefix(s, "---\nname: flightdeck-tools\n") {
		t.Errorf("manual front matter: %q", s[:60])
	}
	if out := run(t, "mcp", "--skill"); out != s {
		t.Error("gff-hab mcp --skill does not print the manual")
	}
}

// --- End to end: two habs over the ssh stand-in ---

func writeClaude(t *testing.T, dir, id, cwd string, at time.Time, user, agent string) {
	t.Helper()
	p := filepath.Join(dir, "projects", strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := func(n int, typ string, msg map[string]any, at time.Time) string {
		b, _ := json.Marshal(map[string]any{"type": typ, "uuid": fmt.Sprintf("00000000-0000-4000-8000-%012d", n),
			"timestamp": at.UTC().Format(time.RFC3339Nano), "cwd": cwd, "sessionId": id, "version": "2.1.310", "message": msg})
		return string(b) + "\n"
	}
	body := rec(1, "user", map[string]any{"role": "user", "content": user}, at) +
		rec(2, "assistant", map[string]any{"id": "msg_1", "type": "message", "model": "model-x", "role": "assistant",
			"content": []map[string]any{{"type": "text", "text": agent}}}, at.Add(time.Second))
	if err := os.WriteFile(filepath.Join(p, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mcpClient is `gff-hab mcp` as a harness runs it: a child process on stdio.
type mcpClient struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Reader
	n   int
}

func startMCP(t *testing.T, sock string) *mcpClient {
	t.Helper()
	cmd := exec.Command(bin, "mcp", "-socket", sock)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GFF_HAB=") { // never a pane's hook address from the test's own shell
			env = append(env, e)
		}
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Wait()
	})
	return &mcpClient{t: t, in: in, out: bufio.NewReader(out)}
}

func (c *mcpClient) call(method string, params any) mcpAnswer {
	c.t.Helper()
	c.n++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.n, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	var a mcpAnswer
	if err := json.Unmarshal(line, &a); err != nil {
		c.t.Fatalf("%s: %v", line, err)
	}
	return a
}

func (c *mcpClient) tool(name string, args map[string]any) *mcpResult {
	c.t.Helper()
	a := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if a.Result == nil {
		c.t.Fatalf("%s: %+v", name, a.Error)
	}
	return a.Result
}

// TestReadToolsOverSSH: `gff-hab mcp` on one hab searches the transcripts of two habs linked
// over the ssh stand-in, each under its own read scope, and reads a file on the other host.
func TestReadToolsOverSSH(t *testing.T) {
	d := shortDir(t)
	sockA, sockB := filepath.Join(d, "a.sock"), filepath.Join(d, "b.sock")
	standIn := sshStandIn(t, map[string]string{"hab-a": sockA, "hab-b": sockB})
	filesA, filesB := filepath.Join(d, "a.toml"), filepath.Join(d, "b.toml")
	writePeers(t, filesA, "bee", "hab-b")
	writePeers(t, filesB, "ay", "hab-a")
	homeA, homeB := filepath.Join(d, "ha"), filepath.Join(d, "hb")
	at := time.Now().Add(-time.Hour)
	writeClaude(t, filepath.Join(homeA, "claude"), "aaaaaaaa-0000-4000-8000-000000000001", "/work/demo", at,
		"Why is the build cache cold?", "The cache is rebuilt on each start.")
	writeClaude(t, filepath.Join(homeB, "claude"), "bbbbbbbb-0000-4000-8000-000000000001", "/work/demo", at.Add(time.Minute),
		"Clear the cache on the dev box", "Cleared the cache.")
	secret := filepath.Join(homeB, "secret")
	writeClaude(t, filepath.Join(homeB, "claude"), "bbbbbbbb-0000-4000-8000-000000000002", secret, at.Add(2*time.Minute),
		"cache of tokens", "Kept private.")
	os.MkdirAll(secret, 0o700)
	os.WriteFile(filepath.Join(homeB, "notes.txt"), []byte("notes on b\n"), 0o600)
	os.WriteFile(filepath.Join(secret, "plan.txt"), []byte("private plan\n"), 0o600)
	cfgB := filepath.Join(d, "b-hab.toml")
	if err := os.WriteFile(cfgB, []byte(fmt.Sprintf("[read]\nallow = [%q]\ndeny = [%q]\n", homeB, secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	envA := []string{"CLAUDE_CONFIG_DIR=" + filepath.Join(homeA, "claude"), "CODEX_HOME=" + filepath.Join(homeA, "codex")}
	envB := []string{"CLAUDE_CONFIG_DIR=" + filepath.Join(homeB, "claude"), "CODEX_HOME=" + filepath.Join(homeB, "codex")}
	peerHabEnv(t, sockA, filepath.Join(d, "sa"), filesA, standIn, "host-a", envA)
	peerHabEnv(t, sockB, filepath.Join(d, "sb"), filesB, standIn, "host-b", envB, "-config", cfgB)
	if w := welcome(t, sockA); !slices.Contains(w.Capabilities, "read-tools") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	// Reads need a's own link to b. The row says online once either link is up, and writing the
	// first pin gives a's link a new connection (mesh.md 4.4): wait for the pin in a's peer
	// file, then for a read over the new connection.
	waitUntil(t, "a pinned b in its peer file", func() bool {
		b, _ := os.ReadFile(filesA)
		return strings.Contains(string(b), "device_id")
	})
	waitUntil(t, "a's own link to b answers a read", func() bool {
		out, _ := exec.Command(bin, "read", "-socket", sockA, "file_read", "-host", "bee", filepath.Join(homeB, "notes.txt")).Output()
		return strings.Contains(string(out), "notes on b")
	})

	c := startMCP(t, sockA)
	if a := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}); a.Result == nil {
		t.Fatalf("initialize: %+v", a.Error)
	}
	// A search reads both hosts, newest first, and says what b's scope left out.
	var r *mcpResult
	deadline := time.Now().Add(20 * time.Second)
	for {
		r = c.tool("transcript_search", map[string]any{"words": "cache"})
		if strings.Contains(r.text(), "bee claude") || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	text := r.text()
	iB, iA := strings.Index(text, "bee claude session bbbbbbbb-0000-4000-8000-000000000001"), strings.Index(text, "host-a claude session aaaaaaaa")
	if r.IsError || iB < 0 || iA < 0 || iB > iA {
		t.Fatalf("search:\n%s", text)
	}
	if strings.Contains(text, "tokens") || strings.Contains(text, "Kept private") || !strings.Contains(text, "host bee: its read scope left out 1 transcripts") {
		t.Errorf("b's denied transcript:\n%s", text)
	}
	// One hop: b answers for itself alone, though it lists a as a peer, so a's hits come once.
	var hits api.TranscriptHits
	if err := json.Unmarshal([]byte(run(t, "read", "-socket", sockA, "-json", "transcript_search", "cache")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits.Hosts) != 2 || len(hits.Hits) != 4 {
		t.Errorf("one hop: %d host rows, %d hits: %+v", len(hits.Hosts), len(hits.Hits), hits)
	}
	// A file read on b goes to b's hab, under b's scope.
	if r := c.tool("file_read", map[string]any{"host": "bee", "path": filepath.Join(homeB, "notes.txt")}); r.IsError || !strings.Contains(r.text(), "notes on b") {
		t.Errorf("file on b: %s", r.text())
	}
	r = c.tool("file_read", map[string]any{"host": "bee", "path": filepath.Join(secret, "plan.txt")})
	if !r.IsError || !strings.Contains(r.text(), "host-b") || !strings.Contains(r.text(), "read scope") || strings.Contains(r.text(), "private plan") {
		t.Errorf("denied file on b: %s", r.text())
	}
	if strings.Contains(r.text(), secret) && !strings.Contains(r.text(), "plan.txt") {
		t.Errorf("the denial repeats the pattern: %s", r.text())
	}
	// b's file is outside a's own scope: a's hab refuses it for itself.
	if r := c.tool("file_read", map[string]any{"path": filepath.Join(homeB, "notes.txt")}); !r.IsError || !strings.Contains(r.text(), "host-a") {
		t.Errorf("b's file read on a: %s", r.text())
	}
	// The session list shows both hosts' sessions; b's denied one is left out.
	r = c.tool("sessions", map[string]any{})
	if !strings.Contains(r.text(), "bee claude bbbbbbbb-0000-4000-8000-000000000001") || !strings.Contains(r.text(), "host-a claude aaaaaaaa") ||
		strings.Contains(r.text(), "bbbbbbbb-0000-4000-8000-000000000002") {
		t.Errorf("sessions:\n%s", r.text())
	}
	// A page of b's session.
	r = c.tool("transcript_read", map[string]any{"host": "bee", "session": "bbbbbbbb-0000-4000-8000-000000000001"})
	if r.IsError || !strings.Contains(r.text(), "Cleared the cache.") {
		t.Errorf("transcript on b:\n%s", r.text())
	}
	// The notice a raised is read back by id.
	var raised api.NoticeRaised
	out := run(t, "api", "call", "-socket", sockA, "notice.raise", `{"title":"Build failed"}`)
	json.Unmarshal([]byte(out), &raised)
	if r := c.tool("notice_read", map[string]any{"id": raised.ID}); r.IsError || !strings.Contains(r.text(), "Build failed") {
		t.Errorf("notice: %s", r.text())
	}
	// A notice b raised is read from a by b's device id, as a push names its hab (capcom.md
	// 8.5), and answers with the label a knows b by.
	var onB api.NoticeRaised
	json.Unmarshal([]byte(run(t, "api", "call", "-socket", sockB, "notice.raise", `{"title":"Disk almost full"}`)), &onB)
	var rec api.NoticeRecord
	q, _ := json.Marshal(api.NoticeGetParams{ID: onB.ID, Host: welcome(t, sockB).DeviceID})
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sockA, "notice.get", string(q))), &rec); err != nil || rec.Host != "bee" || rec.Notice.Title != "Disk almost full" {
		t.Errorf("b's notice by device id: %v %+v", err, rec)
	}
	// Logbook is there, and empty.
	if r := c.tool("logbook_search", map[string]any{"words": "cache"}); r.IsError || !strings.Contains(r.text(), "no records") {
		t.Errorf("logbook: %s", r.text())
	}
	// The CLI reads the same.
	if out := run(t, "read", "-socket", sockA, "transcript_search", "cleared"); !strings.Contains(out, "bee claude") {
		t.Errorf("cli search:\n%s", out)
	}
}

// peerHabEnv is peerHab with extra environment.
func peerHabEnv(t *testing.T, sock, state, peersFile, standIn, hostID string, env []string, args ...string) *served {
	t.Helper()
	rt := shortDir(t)
	return startServeWith(t, sock, state, append([]string{
		"PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GFF_HAB_PEERS=" + peersFile,
		"XDG_RUNTIME_DIR=" + rt,
	}, env...), append([]string{"-host-id", hostID, "-hook-socket", strings.TrimSuffix(sock, ".sock") + ".hook"}, args...)...)
}
