package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// claudeLines writes a hand-made Claude Code transcript: one user and one agent message per
// pair, a second apart from start.
func claudeLines(id, cwd string, start time.Time, pairs ...[2]string) string {
	var b strings.Builder
	n := 0
	rec := func(typ, role string, content any, at time.Time) {
		n++
		msg := map[string]any{"role": role, "content": content}
		if role == "assistant" {
			msg["id"] = fmt.Sprintf("msg_%d", n)
			msg["type"] = "message"
			msg["model"] = "model-x"
		}
		line, _ := json.Marshal(map[string]any{
			"type": typ, "uuid": fmt.Sprintf("00000000-0000-4000-8000-%012d", n), "timestamp": at.UTC().Format(time.RFC3339Nano),
			"cwd": cwd, "sessionId": id, "version": "2.1.310", "message": msg,
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	at := start
	for _, p := range pairs {
		rec("user", "user", p[0], at)
		at = at.Add(time.Second)
		rec("assistant", "assistant", []map[string]any{{"type": "text", "text": p[1]}}, at)
		at = at.Add(time.Second)
	}
	return b.String()
}

// fixture is one hab's folders: a Claude config folder, a Codex home and a home folder.
type fixture struct {
	root, claude, codex, home string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{root: root, claude: filepath.Join(root, "claude"), codex: filepath.Join(root, "codex"), home: filepath.Join(root, "home")}
	for _, d := range []string{f.claude, f.codex, f.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// claudeSession writes a Claude transcript and sets its modification time.
func (f fixture) claudeSession(t *testing.T, id, cwd string, mod time.Time, pairs ...[2]string) string {
	t.Helper()
	dir := filepath.Join(f.claude, "projects", strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(claudeLines(id, cwd, mod.Add(-time.Hour), pairs...)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// codexSession copies the hand-written Codex rollout fixture into the Codex home.
func (f fixture) codexSession(t *testing.T, mod time.Time) string {
	t.Helper()
	b, err := os.ReadFile("../transcript/testdata/codex-0.166.0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.codex, "sessions", "2026", "01", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-01-01T10-00-00-019a0000-0000-7000-8000-000000000001.jsonl")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f fixture) service(host string, deny []string, peers Peers) *Service {
	tr := &Transcripts{ClaudeDir: f.claude, CodexHome: f.codex}
	return New(Config{
		Self: "dev-" + host, Host: host, Transcripts: tr,
		Scope: func() Scope {
			return Scope{Host: host, Home: f.home, Allow: append(tr.Roots(), f.home), Deny: append(append([]string{}, DefaultDeny...), deny...)}
		},
		ProjectName: func(folder string) string {
			if folder == "/work/demo" {
				return "demo"
			}
			return ""
		},
		Peers: peers,
	})
}

// peerCaller answers a peer link's calls with another hab's Service, as a peer link would:
// the params and result go through JSON, and the call is local to that hab.
type peerCaller struct {
	s *Service
}

func (p peerCaller) Call(ctx context.Context, method string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var res any
	switch method {
	case "transcript.list":
		var q api.TranscriptListParams
		json.Unmarshal(b, &q)
		res, err = p.s.Sessions(ctx, true, q)
	case "transcript.search":
		var q api.TranscriptSearchParams
		json.Unmarshal(b, &q)
		res, err = p.s.Search(ctx, true, q)
	case "transcript.read":
		var q api.TranscriptReadParams
		json.Unmarshal(b, &q)
		res, err = p.s.Transcript(ctx, true, q)
	case "file.read":
		var q api.FileReadParams
		json.Unmarshal(b, &q)
		res, err = p.s.File(ctx, true, q)
	default:
		return fmt.Errorf("method %q does not exist", method)
	}
	if err != nil {
		return err
	}
	out, _ := json.Marshal(res)
	return json.Unmarshal(out, result)
}

// slowCaller never answers before its context ends.
type slowCaller struct{}

func (slowCaller) Call(ctx context.Context, method string, params, result any) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakePeers struct {
	list  []api.Peer
	links []Link
}

func (f fakePeers) List() api.PeerList { return api.PeerList{Peers: f.list} }
func (f fakePeers) Links() []Link      { return f.links }

func kindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

func TestScopeAllowDeny(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(home, "proj")
	secret := filepath.Join(home, "private")
	for _, d := range []string{filepath.Join(proj, "src"), filepath.Join(home, ".ssh"), secret, filepath.Join(root, "outside")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proj, "src", "main.go"))
	write(filepath.Join(proj, ".env"))
	write(filepath.Join(proj, "server.pem"))
	write(filepath.Join(home, ".ssh", "id_ed25519"))
	write(filepath.Join(secret, "notes.txt"))
	write(filepath.Join(root, "outside", "x.txt"))
	// Links: one inside the root pointing at a denied file, one pointing out of every root.
	if err := os.Symlink(filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(proj, "key-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(proj, "out-link")); err != nil {
		t.Fatal(err)
	}
	sc := Scope{Host: "boxa", Home: home, Allow: []string{home}, Deny: append(append([]string{}, DefaultDeny...), "~/private")}

	cases := []struct {
		path string
		ok   bool
		why  string
	}{
		{filepath.Join(proj, "src", "main.go"), true, ""},
		{"~/proj/src/main.go", true, ""},
		{filepath.Join(proj, ".env"), false, "read.deny"},
		{filepath.Join(proj, "server.pem"), false, "read.deny"},
		{filepath.Join(home, ".ssh", "id_ed25519"), false, "read.deny"},
		{filepath.Join(secret, "notes.txt"), false, "read.deny"},        // the user's own deny entry
		{filepath.Join(proj, "key-link"), false, "read.deny"},           // a link to a denied file
		{filepath.Join(proj, "out-link", "x.txt"), false, "read.allow"}, // a link out of the root
		{filepath.Join(root, "outside", "x.txt"), false, "read.allow"},  // never listed
		{filepath.Join(proj, "src", "..", "..", ".ssh", "x"), false, "read.deny"},
		{filepath.Join(root, "outside", "missing.txt"), false, "read.allow"}, // no file: still refused, not "not found"
	}
	for _, c := range cases {
		_, err := sc.Check(c.path)
		if c.ok {
			if err != nil {
				t.Errorf("%s: %v", c.path, err)
			}
			continue
		}
		if kindOf(err) != api.ErrorKindDenied {
			t.Errorf("%s: got %v, want a denial", c.path, err)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "boxa") || !strings.Contains(msg, c.why) {
			t.Errorf("%s: %q should name the host and %s", c.path, msg, c.why)
		}
		// The denial never repeats the pattern that matched (capcom.md 5.2, D9).
		for _, pat := range []string{"~/private", "private", ".ssh", "*.pem", ".env", "id_ed25519"} {
			if strings.Contains(msg, pat) {
				t.Errorf("%s: denial %q repeats the pattern %q", c.path, msg, pat)
			}
		}
	}
	if _, err := sc.Check("relative/path"); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("relative path: got %v, want invalid", err)
	}
}

func TestScopeSourceConfig(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "hab.toml")
	src := &ScopeSource{Host: "boxa", Home: home, Config: cfg, Roots: func() []string { return []string{filepath.Join(home, "proj")} }}
	// No file: the defaults.
	sc := src.Scope()
	if sc.Broken || len(sc.Allow) != 1 || len(sc.Deny) != len(DefaultDeny) {
		t.Fatalf("no file: %+v", sc)
	}
	// A file that does not parse at start: every read refused.
	os.WriteFile(cfg, []byte("[read\nallow = 3"), 0o644)
	src = &ScopeSource{Host: "boxa", Home: home, Config: cfg, Roots: src.Roots}
	if sc := src.Scope(); !sc.Broken {
		t.Fatal("a broken first config should refuse every read")
	} else if _, err := sc.Check(filepath.Join(home, "proj", "a")); kindOf(err) != api.ErrorKindDenied {
		t.Fatalf("broken config: %v", err)
	}
	// A good file adds roots and patterns.
	os.WriteFile(cfg, []byte("[read]\nallow = [\"~/extra\", \"relative\"]\ndeny = [\"*.sqlite\"]\n"), 0o644)
	sc = src.Scope()
	if sc.Broken || len(sc.Allow) != 2 || sc.Allow[1] != filepath.Join(home, "extra") || sc.Deny[len(sc.Deny)-1] != "*.sqlite" {
		t.Fatalf("good config: %+v", sc)
	}
	// Then a broken one keeps the last good section.
	os.WriteFile(cfg, []byte("[read\n"), 0o644)
	if sc := src.Scope(); sc.Broken || len(sc.Allow) != 2 {
		t.Fatalf("broken after good: %+v", sc)
	}
}

func TestSessionsList(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.claudeSession(t, "aaaaaaaa-0000-4000-8000-000000000001", "/work/demo", now.Add(-2*time.Hour), [2]string{"Fix the cache bug", "Done."})
	f.claudeSession(t, "aaaaaaaa-0000-4000-8000-000000000002", "/work/other", now.Add(-time.Hour), [2]string{"Write the docs", "Written."})
	f.codexSession(t, now.Add(-3*time.Hour))
	s := f.service("boxa", nil, nil)
	s.cfg.Live = func() []Live {
		return []Live{{Harness: Claude, Native: "aaaaaaaa-0000-4000-8000-000000000002", Session: "s1", Name: "docs", State: api.AgentStatusWorking}}
	}
	res, err := s.Sessions(context.Background(), false, api.TranscriptListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 3 {
		t.Fatalf("got %d sessions: %+v", len(res.Sessions), res.Sessions)
	}
	first, second, third := res.Sessions[0], res.Sessions[1], res.Sessions[2]
	if first.ID != "aaaaaaaa-0000-4000-8000-000000000002" || first.Title != "Write the docs" || first.Live == nil || first.Live.Name != "docs" {
		t.Errorf("newest: %+v", first)
	}
	if second.Project != "demo" || second.Folder != "/work/demo" || second.Harness != Claude || second.Started == "" {
		t.Errorf("second: %+v", second)
	}
	if third.Harness != Codex || third.ID != "019a0000-0000-7000-8000-000000000001" || third.Folder != "/work/demo" ||
		!strings.HasPrefix(third.Title, "Rename greet to hello") {
		t.Errorf("codex: %+v", third)
	}
	if len(res.Hosts) != 1 || !res.Hosts[0].Own || res.Hosts[0].State != api.ReadHostStateOk {
		t.Errorf("hosts: %+v", res.Hosts)
	}
	// Filters: project by name, harness, since.
	res, _ = s.Sessions(context.Background(), false, api.TranscriptListParams{Project: "demo"})
	if len(res.Sessions) != 2 {
		t.Errorf("project demo: %+v", res.Sessions)
	}
	res, _ = s.Sessions(context.Background(), false, api.TranscriptListParams{Harness: "codex"})
	if len(res.Sessions) != 1 || res.Sessions[0].Harness != Codex {
		t.Errorf("harness codex: %+v", res.Sessions)
	}
	res, _ = s.Sessions(context.Background(), false, api.TranscriptListParams{Since: now.Add(-90 * time.Minute).Format(time.RFC3339), Limit: 5})
	if len(res.Sessions) != 1 {
		t.Errorf("since: %+v", res.Sessions)
	}
	res, _ = s.Sessions(context.Background(), false, api.TranscriptListParams{Limit: 1})
	if len(res.Sessions) != 1 || !res.More {
		t.Errorf("limit 1: %+v", res)
	}
	if _, err := s.Sessions(context.Background(), false, api.TranscriptListParams{Harness: "pi"}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("unknown harness: %v", err)
	}
}

func TestSearchLocal(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.claudeSession(t, "bbbbbbbb-0000-4000-8000-000000000001", "/work/demo", now.Add(-2*time.Hour),
		[2]string{"Why is the Redis cache cold?", "The cache warms on start."},
		[2]string{"Decide about the cache eviction", "We keep LRU eviction for the cache."})
	f.claudeSession(t, "bbbbbbbb-0000-4000-8000-000000000002", "/work/secret", now.Add(-time.Hour),
		[2]string{"cache keys for the vault", "Stored."})
	f.codexSession(t, now.Add(-3*time.Hour))
	s := f.service("boxa", []string{"/work/secret"}, nil)

	res, err := s.Search(context.Background(), false, api.TranscriptSearchParams{Words: "CACHE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 4 {
		t.Fatalf("got %d hits: %+v", len(res.Hits), res.Hits)
	}
	if res.Hits[0].Snippet != "We keep LRU eviction for the cache." || res.Hits[0].Project != "demo" || res.Hits[0].Message == "" {
		t.Errorf("newest hit: %+v", res.Hits[0])
	}
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i-1].At < res.Hits[i].At {
			t.Errorf("hits not newest first: %s before %s", res.Hits[i-1].At, res.Hits[i].At)
		}
	}
	for _, h := range res.Hits {
		if h.Folder == "/work/secret" || strings.Contains(h.Snippet, "vault") {
			t.Errorf("a denied transcript was searched: %+v", h)
		}
	}
	if res.Hosts[0].LeftOut != 1 {
		t.Errorf("left out: %+v", res.Hosts[0])
	}
	// Every term must match; a phrase stays together.
	res, _ = s.Search(context.Background(), false, api.TranscriptSearchParams{Words: `"lru eviction" cache`})
	if len(res.Hits) != 1 {
		t.Errorf("phrase: %+v", res.Hits)
	}
	res, _ = s.Search(context.Background(), false, api.TranscriptSearchParams{Words: `"eviction lru"`})
	if len(res.Hits) != 0 {
		t.Errorf("phrase in the wrong order: %+v", res.Hits)
	}
	// Codex rollouts are searched too.
	res, _ = s.Search(context.Background(), false, api.TranscriptSearchParams{Words: "app.py", Harness: "codex"})
	if len(res.Hits) == 0 || res.Hits[0].Harness != Codex {
		t.Errorf("codex: %+v", res.Hits)
	}
	if _, err := s.Search(context.Background(), false, api.TranscriptSearchParams{Words: "  "}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("no words: %v", err)
	}
}

func TestSearchCapsHits(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var pairs [][2]string
	for i := range 40 {
		pairs = append(pairs, [2]string{fmt.Sprintf("needle question %d", i), fmt.Sprintf("needle answer %d", i)})
	}
	f.claudeSession(t, "cccccccc-0000-4000-8000-000000000001", "/work/demo", now, pairs...)
	s := f.service("boxa", nil, nil)
	res, err := s.Search(context.Background(), false, api.TranscriptSearchParams{Words: "needle", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != MaxHits || !res.More {
		t.Fatalf("got %d hits, more %v; want %d and more", len(res.Hits), res.More, MaxHits)
	}
	if res.Hits[0].Snippet != "needle answer 39" {
		t.Errorf("newest first: %q", res.Hits[0].Snippet)
	}
}

// TestSearchOfflineAndSlowPeers: a search with one offline and one slow peer returns the
// hits it has, with each host's state (capcom.md 12).
func TestSearchOfflineAndSlowPeers(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	fb := newFixture(t)
	fb.claudeSession(t, "dddddddd-0000-4000-8000-000000000002", "/work/demo", now.Add(-time.Minute), [2]string{"the deploy failed on boxb", "Fixed the deploy."})
	fb.claudeSession(t, "dddddddd-0000-4000-8000-000000000003", "/work/private", now, [2]string{"deploy keys", "Hidden."})
	peerB := fb.service("boxb", []string{"/work/private"}, nil)

	fa := newFixture(t)
	fa.claudeSession(t, "dddddddd-0000-4000-8000-000000000001", "/work/demo", now.Add(-time.Hour), [2]string{"deploy plan", "Deploy on Friday."})
	peers := fakePeers{
		list: []api.Peer{
			{Label: "b", State: api.PeerStateOnline, DeviceID: "dev-b"},
			{Label: "laptop", State: api.PeerStateOffline, DeviceID: "dev-l", Reason: "asleep since 10:00"},
			{Label: "slow", State: api.PeerStateOnline, DeviceID: "dev-s"},
			{Label: "old", State: api.PeerStateOnline, DeviceID: "dev-o"},
		},
		links: []Link{
			{Label: "b", DeviceID: "dev-b", Online: true, Caps: []string{Capability}, Caller: peerCaller{peerB}},
			{Label: "laptop", DeviceID: "dev-l"},
			{Label: "slow", DeviceID: "dev-s", Online: true, Caps: []string{Capability}, Caller: slowCaller{}},
			{Label: "old", DeviceID: "dev-o", Online: true, Caps: []string{"catalog"}, Caller: slowCaller{}},
		},
	}
	a := fa.service("boxa", nil, peers)
	a.cfg.Budget = 200 * time.Millisecond

	start := time.Now()
	res, err := a.Search(context.Background(), false, api.TranscriptSearchParams{Words: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > a.cfg.Budget+forwardGrace+2*time.Second {
		t.Errorf("the search took %v", took)
	}
	byHost := map[string]api.ReadHost{}
	for _, h := range res.Hosts {
		byHost[h.Label] = h
	}
	if h := byHost["boxa"]; !h.Own || h.State != api.ReadHostStateOk {
		t.Errorf("own: %+v", h)
	}
	if h := byHost["b"]; h.State != api.ReadHostStateOk || h.LeftOut != 1 || h.DeviceID != "dev-b" {
		t.Errorf("b: %+v", h)
	}
	if h := byHost["laptop"]; h.State != api.ReadHostStateOffline || h.Reason != "asleep since 10:00" {
		t.Errorf("laptop: %+v", h)
	}
	if h := byHost["slow"]; h.State != api.ReadHostStatePartial || h.Reason == "" {
		t.Errorf("slow: %+v", h)
	}
	if h := byHost["old"]; h.State != api.ReadHostStateUpdateNeeded {
		t.Errorf("old: %+v", h)
	}
	if len(res.Hosts) != 5 {
		t.Errorf("hosts: %+v", res.Hosts)
	}
	// Hits from both reachable hosts, newest first, each labelled with this hab's name for it.
	var got []string
	for _, h := range res.Hits {
		got = append(got, h.Host+":"+h.Snippet)
	}
	want := []string{"b:Fixed the deploy.", "b:the deploy failed on boxb", "boxa:Deploy on Friday.", "boxa:deploy plan"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("hits:\n got %v\nwant %v", got, want)
	}
	// One host by label.
	res, err = a.Search(context.Background(), false, api.TranscriptSearchParams{Words: "deploy", Host: "b"})
	if err != nil || len(res.Hosts) != 1 || len(res.Hits) != 2 {
		t.Errorf("host b: %v %+v", err, res)
	}
	if _, err := a.Search(context.Background(), false, api.TranscriptSearchParams{Words: "deploy", Host: "nope"}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("unknown host: %v", err)
	}
	// A call over a peer link reads that hab alone, never its peers (one hop).
	res, err = a.Search(context.Background(), true, api.TranscriptSearchParams{Words: "deploy"})
	if err != nil || len(res.Hosts) != 1 || !res.Hosts[0].Own {
		t.Errorf("local: %v %+v", err, res.Hosts)
	}
}

func TestTranscriptRead(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.claudeSession(t, "eeeeeeee-0000-4000-8000-000000000001", "/work/demo", now,
		[2]string{"one", "first answer"}, [2]string{"two", "second answer"})
	f.claudeSession(t, "eeeeeeee-0000-4000-8000-000000000002", "/work/secret", now, [2]string{"hidden", "hidden answer"})
	f.codexSession(t, now)
	s := f.service("boxa", []string{"/work/secret"}, nil)
	s.cfg.Live = func() []Live {
		return []Live{{Harness: Claude, Native: "eeeeeeee-0000-4000-8000-000000000001", Session: "s9", Name: "demo-agent"}}
	}
	page, err := s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "eeeeeeee-0000-4000-8000-000000000001", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || !page.More || page.Messages[1].Text != "second answer" || page.Folder != "/work/demo" || page.Harness != Claude {
		t.Fatalf("page: %+v", page)
	}
	older, err := s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "eeeeeeee-0000-4000-8000-000000000001", Before: page.Messages[0].ID})
	if err != nil || len(older.Messages) != 2 || older.Messages[0].Text != "one" {
		t.Fatalf("older: %v %+v", err, older)
	}
	// By the Flightdeck session's name.
	if p, err := s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "demo-agent"}); err != nil || len(p.Messages) != 4 {
		t.Errorf("by name: %v %+v", err, p)
	}
	// A Codex rollout, found with no harness given.
	if p, err := s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "019a0000-0000-7000-8000-000000000001"}); err != nil || p.Harness != Codex || len(p.Messages) == 0 {
		t.Errorf("codex: %v %+v", err, p)
	}
	// A session whose folder is denied is refused, without the pattern.
	_, err = s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "eeeeeeee-0000-4000-8000-000000000002"})
	if kindOf(err) != api.ErrorKindDenied || strings.Contains(err.Error(), "secret") {
		t.Errorf("denied folder: %v", err)
	}
	if _, err := s.Transcript(context.Background(), false, api.TranscriptReadParams{Session: "ffffffff-0000-4000-8000-000000000009"}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestFileRead(t *testing.T) {
	f := newFixture(t)
	proj := filepath.Join(f.home, "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "README.md"), []byte("héllo world\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".env"), []byte("TOKEN=x\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "blob.bin"), []byte{0, 1, 2, 0xff}, 0o644)
	os.WriteFile(filepath.Join(f.root, "elsewhere.txt"), []byte("outside\n"), 0o644)
	s := f.service("boxa", nil, nil)
	ctx := context.Background()

	d, err := s.File(ctx, false, api.FileReadParams{Path: "~/proj/README.md"})
	if err != nil || d.Text != "héllo world\n" || d.More || d.Size != 13 || d.Path != filepath.Join(proj, "README.md") {
		t.Fatalf("readme: %v %+v", err, d)
	}
	// A run that ends inside a character drops it; the next run starts with it.
	d, err = s.File(ctx, false, api.FileReadParams{Path: filepath.Join(proj, "README.md"), Limit: 2})
	if err != nil || d.Text != "h" || !d.More {
		t.Fatalf("cut run: %v %+v", err, d)
	}
	d, _ = s.File(ctx, false, api.FileReadParams{Path: filepath.Join(proj, "README.md"), Offset: 1, Limit: 2})
	if d.Text != "é" {
		t.Errorf("next run: %+v", d)
	}
	if d, _ := s.File(ctx, false, api.FileReadParams{Path: filepath.Join(proj, "blob.bin")}); !d.Binary || d.Text != "" {
		t.Errorf("binary: %+v", d)
	}
	for _, p := range []string{filepath.Join(proj, ".env"), filepath.Join(f.root, "elsewhere.txt")} {
		_, err := s.File(ctx, false, api.FileReadParams{Path: p})
		if kindOf(err) != api.ErrorKindDenied || strings.Contains(err.Error(), ".env\"") {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := s.File(ctx, false, api.FileReadParams{Path: filepath.Join(proj, "missing.txt")}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := s.File(ctx, false, api.FileReadParams{Path: proj}); kindOf(err) != api.ErrorKindInvalid {
		t.Errorf("folder: %v", err)
	}
}

// TestFileReadOnPeer: a read on another host goes to its hab, under its scope, not this one's.
func TestFileReadOnPeer(t *testing.T) {
	fb := newFixture(t)
	os.WriteFile(filepath.Join(fb.home, "notes.txt"), []byte("b notes\n"), 0o644)
	os.WriteFile(filepath.Join(fb.home, "private.txt"), []byte("b private\n"), 0o644)
	b := fb.service("boxb", []string{"private.txt"}, nil)
	fa := newFixture(t)
	a := fa.service("boxa", nil, fakePeers{
		list:  []api.Peer{{Label: "b", State: api.PeerStateOnline}, {Label: "gone", State: api.PeerStateOffline, Reason: "no route"}},
		links: []Link{{Label: "b", Online: true, Caps: []string{Capability}, Caller: peerCaller{b}}, {Label: "gone"}},
	})
	d, err := a.File(context.Background(), false, api.FileReadParams{Host: "b", Path: "~/notes.txt"})
	if err != nil || d.Text != "b notes\n" || d.Host != "b" {
		t.Fatalf("peer read: %v %+v", err, d)
	}
	_, err = a.File(context.Background(), false, api.FileReadParams{Host: "b", Path: "~/private.txt"})
	if kindOf(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "boxb") || strings.Contains(err.Error(), "private.txt\"") {
		t.Errorf("peer denial: %v", err)
	}
	if _, err := a.File(context.Background(), false, api.FileReadParams{Host: "gone", Path: "~/notes.txt"}); kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "no route") {
		t.Errorf("offline peer: %v", err)
	}
}

func TestNoticeGet(t *testing.T) {
	s := New(Config{Host: "boxa",
		Notice: func(id string) (api.Notice, bool) {
			return api.Notice{ID: id, Title: "Build failed", Session: "s1"}, id == "n1"
		},
		Session: func(id string) (api.Session, bool) { return api.Session{ID: id, Name: "api"}, id == "s1" },
	})
	r, err := s.Notice(context.Background(), false, api.NoticeGetParams{ID: "n1"})
	if err != nil || r.Notice.Title != "Build failed" || r.Session == nil || r.Session.Name != "api" || r.Host != "boxa" {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := s.Notice(context.Background(), false, api.NoticeGetParams{ID: "n2"}); kindOf(err) != api.ErrorKindNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestTermsAndSnippet(t *testing.T) {
	if got := strings.Join(Terms(`Cache  "LRU   eviction" x`), "|"); got != "cache|lru eviction|x" {
		t.Errorf("terms: %q", got)
	}
	long := strings.Repeat("a ", 200) + "needle " + strings.Repeat("b ", 200)
	s, ok := Snippet(long, []string{"needle"})
	if !ok || !strings.HasPrefix(s, "…") || !strings.Contains(s, "needle") || len([]rune(s)) > snippetChars+1 {
		t.Errorf("snippet %q", s)
	}
	if _, ok := Snippet("only one", []string{"one", "two"}); ok {
		t.Error("a missing term matched")
	}
}
