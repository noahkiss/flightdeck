package push

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
)

func TestFit(t *testing.T) {
	for _, c := range []struct {
		in           string
		words, chars int
		want         string
	}{
		{"Run wrangler deploy?", 4, 28, "Run wrangler deploy?"},
		{"one two three four five", 3, 0, "one two three…"},
		{"alpha beta gamma", 0, 12, "alpha beta…"},
		{"  spaced\n\tout  ", 0, 0, "spaced out"},
		{"supercalifragilistic", 0, 8, "superca…"},
		{"web-app · laptop", 4, 0, "web-app · laptop"},
	} {
		got := Fit(c.in, c.words, c.chars)
		if got != c.want {
			t.Errorf("Fit(%q, %d, %d) = %q, want %q", c.in, c.words, c.chars, got, c.want)
		}
		if c.chars > 0 && utf8.RuneCountInString(got) > c.chars {
			t.Errorf("Fit(%q) is %d characters, over %d", c.in, utf8.RuneCountInString(got), c.chars)
		}
	}
}

func TestShortCommand(t *testing.T) {
	for _, c := range []struct{ cmd, cwd, want string }{
		{"cd /srv/app && NODE_ENV=production wrangler deploy --env prod", "/srv/app", "wrangler deploy --env prod"},
		{"cd a; cd b && make test", "", "make test"},
		{"go test /work/proj/internal/push/...", "/work/proj", "go test internal/push/..."},
		{`git commit -m "a very long commit message that keeps going and going"`, "", `git commit -m "a very long… going"`},
		{"FOO=1", "", "FOO=1"},
	} {
		if got := ShortCommand(c.cmd, c.cwd); got != c.want {
			t.Errorf("ShortCommand(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBannerBudgets: every banner kind keeps to the word budgets of app.md 16.2, and Approve
// is offered only when the whole command fits and commands are shown.
func TestBannerBudgets(t *testing.T) {
	w := Where{Hab: "laptop", Session: "s1", SessionName: "fix-login", SessionTitle: "Fix the login form validation and its tests", Project: "web-app", Harness: "claude"}
	short := &api.PermissionRequest{ID: "r1", Tool: "Bash", Command: "cd /srv/app && wrangler deploy --env prod", Cwd: "/srv/app"}
	long := &api.PermissionRequest{ID: "r2", Tool: "Bash", Command: "rsync -avz --delete --exclude node_modules --exclude .git --exclude dist ./ deploy@example.invalid:/var/www/app/releases/current/ && ssh deploy@example.invalid systemctl restart app"}
	files := &api.PermissionRequest{ID: "r3", Tool: "Edit", Files: []string{"/w/src/sign-in.ts", "/w/src/auth.ts", "/w/src/db.ts"}}
	cases := []struct {
		b                  Banner
		title, sub, body   int // word budgets
		titleChars         int
		wantTitle, wantCat string
	}{
		{PermissionBanner(w, short, false), 4, 4, 16, 28, "Run wrangler deploy?", CategoryApproval},
		{PermissionBanner(w, long, false), 4, 4, 16, 28, "Run rsync?", CategoryDeny},
		{PermissionBanner(w, files, false), 4, 4, 16, 28, "Edit 3 files?", CategoryApproval},
		{PermissionBanner(w, short, true), 4, 4, 16, 28, "Approval needed", CategoryDeny},
		{QuestionBanner(w), 8, 4, 16, 50, "", CategoryOpen},
		{AuthBanner(w), 3, 4, 10, 0, "Sign-in needed", CategoryOpen},
		{ErrorBanner(w), 4, 4, 15, 0, "Turn stopped: web-app", CategoryOpen},
		{DoneBanner(w), 5, 4, 15, 0, "", ""},
		{AnsweredBanner(w, "phone", ptr(true), short, false), 4, 4, 8, 0, "Answered on phone", ""},
		{AnsweredBanner(w, "", nil, short, false), 4, 4, 8, 0, "Answered in the terminal", ""},
		{NoticeBanner(Where{Hab: "laptop"}, "Backup finished on the big disk after a long run today", "All 3 snapshots verified.", LevelPassive), 8, 4, 16, 50, "", ""},
	}
	for _, c := range cases {
		b := c.b
		if Words(b.Title) > c.title || Words(b.Subtitle) > c.sub || Words(b.Body) > c.body {
			t.Errorf("%s over budget: %q (%d) / %q (%d) / %q (%d)", b.Kind, b.Title, Words(b.Title), b.Subtitle, Words(b.Subtitle), b.Body, Words(b.Body))
		}
		if c.titleChars > 0 && utf8.RuneCountInString(b.Title) > c.titleChars {
			t.Errorf("%s title %q over %d characters", b.Kind, b.Title, c.titleChars)
		}
		if c.wantTitle != "" && b.Title != c.wantTitle {
			t.Errorf("%s title %q, want %q", b.Kind, b.Title, c.wantTitle)
		}
		if b.Category != c.wantCat {
			t.Errorf("%s %q category %q, want %q", b.Kind, b.Title, b.Category, c.wantCat)
		}
	}
	if b := PermissionBanner(w, short, false); b.Body != "wrangler deploy --env prod" || b.Subtitle != "web-app · laptop" {
		t.Errorf("permission body %q subtitle %q", b.Body, b.Subtitle)
	}
	if b := PermissionBanner(w, short, true); strings.Contains(b.Body, "wrangler") || strings.Contains(b.Title, "wrangler") {
		t.Errorf("hidden commands leaked: %+v", b)
	}
	if b := AnsweredBanner(w, "phone", ptr(false), short, false); b.Body != "Denied: wrangler deploy" {
		t.Errorf("denied body %q", b.Body)
	}
}

// TestCompoundCommandBanner: a command that opens with a shell keyword or a group names no
// program, so its title and its answered body say "shell script", never "Run if?". Approve
// still follows the whole-command rule.
func TestCompoundCommandBanner(t *testing.T) {
	w := Where{Hab: "laptop", Session: "s1", Project: "web-app", Harness: "claude"}
	cases := []struct {
		cmd     string
		wantCat string
	}{
		{"if [ -e notes.txt ]; then echo yes; fi", CategoryApproval},
		{"if [ -e watch-test.txt ]; then echo 'the file is there, so the watch test can go on'; else echo 'no file yet, try again later'; fi", CategoryDeny},
		{"for f in *.go; do gofmt -l $f; done", CategoryApproval},
		{"[[ -f go.mod ]] && go test ./...", CategoryApproval},
		{"(cd sub && make)", CategoryApproval},
		{"{ echo a; echo b; } > out.txt", CategoryApproval},
		{"cd /srv/app && while true; do sleep 1; done", CategoryApproval},
	}
	for _, c := range cases {
		r := &api.PermissionRequest{ID: "r1", Tool: "Bash", Command: c.cmd}
		b := PermissionBanner(w, r, false)
		if b.Title != "Run shell script?" || b.Category != c.wantCat {
			t.Errorf("%q: title %q category %q, want %q %q", c.cmd, b.Title, b.Category, "Run shell script?", c.wantCat)
		}
		if a := AnsweredBanner(w, "phone", ptr(true), r, false); a.Body != "Allowed: shell script" {
			t.Errorf("%q: answered body %q", c.cmd, a.Body)
		}
	}
	// A program whose name only starts like a keyword keeps its name.
	r := &api.PermissionRequest{ID: "r2", Tool: "Bash", Command: "iftop -i en0"}
	if b := PermissionBanner(w, r, false); b.Title != "Run iftop?" {
		t.Errorf("iftop title %q", b.Title)
	}
}

// TestCommandDescription: the harness's summary of a command titles the banner when it fits
// the title budget whole, and names what was answered; the body keeps the command, so Approve
// still follows the whole-command rule. The summary is model text: control and bidi
// characters go, it is cut, and hidden commands hide it too.
func TestCommandDescription(t *testing.T) {
	w := Where{Hab: "laptop", Session: "s1", Project: "web-app", Harness: "claude"}
	req := func(cmd, input string) *api.PermissionRequest {
		return &api.PermissionRequest{ID: "r1", Tool: "Bash", Command: cmd, Input: api.RawJSON(input)}
	}
	cases := []struct {
		name                  string
		r                     *api.PermissionRequest
		title, body, answered string
		cat                   string
	}{
		{"short summary", req("rm -rf build", `{"command":"rm -rf build","description":"Remove build output."}`),
			"Remove build output?", "rm -rf build", "Allowed: Remove build output", CategoryApproval},
		{"summary over the title budget", req("if [ -e notes.txt ]; then echo yes; fi", `{"command":"x","description":"Check whether watch-test.txt exists"}`),
			"Run shell script?", "if [ -e notes.txt ]; then echo yes; fi", "Allowed: Check whether watch-test.txt exists", CategoryApproval},
		{"long summary cut in the answered line", req("make test", `{"command":"make test","description":"Run the whole test suite of the project again after the fix to the login form"}`),
			"Run make test?", "make test", "Allowed: Run the whole test suite of the…", CategoryApproval},
		{"control and bidi characters", req("ls", "{\"command\":\"ls\",\"description\":\"List\\u001b[31m\\u202efiles\\nnow\"}"),
			"List [31m files now?", "ls", "Allowed: List [31m files now", CategoryApproval},
		{"codex justification", req("git push", `{"command":"git push","justification":"Push the fix"}`),
			"Push the fix?", "git push", "Allowed: Push the fix", CategoryApproval},
		{"blank summary", req("make test", `{"command":"make test","description":"  \n "}`),
			"Run make test?", "make test", "Allowed: make test", CategoryApproval},
		{"no summary", req("make test", `{"command":"make test"}`),
			"Run make test?", "make test", "Allowed: make test", CategoryApproval},
		{"summary on a tool with no command", &api.PermissionRequest{ID: "r1", Tool: "Task", Input: api.RawJSON(`{"description":"Explore the repo"}`)},
			"Allow Task?", "", "Allowed: Task", CategoryDeny},
	}
	for _, c := range cases {
		b := PermissionBanner(w, c.r, false)
		if b.Title != c.title || b.Body != c.body || b.Category != c.cat {
			t.Errorf("%s: %q / %q / %q, want %q / %q / %q", c.name, b.Title, b.Body, b.Category, c.title, c.body, c.cat)
		}
		if Words(b.Title) > 4 || utf8.RuneCountInString(b.Title) > 28 {
			t.Errorf("%s: title %q over budget", c.name, b.Title)
		}
		a := AnsweredBanner(w, "phone", ptr(true), c.r, false)
		if a.Body != c.answered {
			t.Errorf("%s: answered %q, want %q", c.name, a.Body, c.answered)
		}
		if Words(a.Body) > 8 || utf8.RuneCountInString(a.Body) > 60 {
			t.Errorf("%s: answered %q over budget", c.name, a.Body)
		}
		for _, s := range []string{b.Title, a.Body} {
			if strings.ContainsAny(s, "\x1b‮\n") {
				t.Errorf("%s: control character kept in %q", c.name, s)
			}
		}
	}
	// Hidden commands hide the summary too: it says what the command does.
	r := cases[0].r
	if b := PermissionBanner(w, r, true); strings.Contains(b.Title+b.Body, "build") {
		t.Errorf("hidden banner shows the summary: %+v", b)
	}
	if a := AnsweredBanner(w, "phone", ptr(true), r, true); strings.Contains(a.Body, "build") {
		t.Errorf("hidden answered banner shows the summary: %q", a.Body)
	}
	// A very long summary is cut before it is fitted.
	long := req("ls", `{"command":"ls","description":"`+strings.Repeat("a", 5000)+`"}`)
	if d := commandDescription(long); utf8.RuneCountInString(d) != maxDescription {
		t.Errorf("long summary kept %d runes", utf8.RuneCountInString(d))
	}
}

func ptr[T any](v T) *T { return &v }

// TestToolBanners: plans, questions, MCP tools and fetches read in plain words on a Watch
// (app.md 16.1): no tool ids, the question and its numbered options, Approve only for a plan
// shown whole, and never for a question.
func TestToolBanners(t *testing.T) {
	w := Where{Hab: "laptop", Session: "s1", SessionName: "fix-login", Project: "web-app", Harness: "claude"}
	in := func(s string) api.RawJSON { return api.RawJSON(s) }
	cases := []struct {
		name            string
		r               *api.PermissionRequest
		title, body     string
		cat, answeredTo string
	}{
		{"short plan", &api.PermissionRequest{ID: "p1", Tool: "ExitPlanMode", Input: in(`{"plan":"## Plan\n\n- Add **rate limit** to ` + "`login`" + `\n"}`)},
			"Approve plan?", "Add rate limit to login", CategoryApproval, "Allowed: plan"},
		{"long plan", &api.PermissionRequest{ID: "p2", Tool: "ExitPlanMode", Input: in(`{"plan":"# Plan\n1. Move the session store to Redis so restarts keep users signed in\n2. Add a migration\n3. Update the tests"}`)},
			"Approve plan?", "Move the session store to Redis so restarts keep users signed in…", CategoryDeny, "Allowed: plan"},
		{"plan with flags", &api.PermissionRequest{ID: "p3", Tool: "ExitPlanMode", Input: in(`{"plan":"--force push the branch"}`)},
			"Approve plan?", "--force push the branch", CategoryApproval, "Allowed: plan"},
		{"question", &api.PermissionRequest{ID: "q1", Tool: "AskUserQuestion", Input: in(`{"questions":[{"question":"Which auth library should we use?","header":"Auth","options":[{"label":"Clerk","description":"hosted"},{"label":"Auth.js"}],"multiSelect":false}]}`)},
			"Which auth library should we use?", "1 Clerk · 2 Auth.js", CategoryOpen, "Which auth library should we use?"},
		{"question, no input", &api.PermissionRequest{ID: "q2", Tool: "AskUserQuestion"},
			"Question waiting", "", CategoryOpen, ""},
		{"two questions, an empty label", &api.PermissionRequest{ID: "q3", Tool: "AskUserQuestion", Input: in(`{"questions":[{"question":"Which database should the new service use for its jobs?","options":[{"label":""},{"label":"Postgres"},{"label":"SQLite"}]},{"question":"Deploy now?","options":[{"label":"Yes"}]}]}`)},
			"Which database should the new service… (+1 more)", "2 Postgres · 3 SQLite", CategoryOpen, "Which database should the new service use for…"},
		{"mcp tool", &api.PermissionRequest{ID: "m1", Tool: "mcp__github__create_issue"},
			"Allow github create issue?", "", CategoryDeny, "Allowed: github create issue"},
		// Only WebFetch reads as a fetch: another tool's url hides its other arguments.
		{"mcp tool with a url", &api.PermissionRequest{ID: "m2", Tool: "mcp__browser__navigate", Input: in(`{"url":"https://example.com/","post":"x"}`)},
			"Allow browser navigate?", "", CategoryDeny, "Allowed: browser navigate"},
		{"fetch", &api.PermissionRequest{ID: "f1", Tool: "WebFetch", Input: in(`{"url":"https://docs.example.com/guide/","prompt":"summarise"}`)},
			"Fetch docs.example.com?", "docs.example.com/guide", CategoryApproval, "Allowed: fetch docs.example.com"},
	}
	for _, c := range cases {
		b := PermissionBanner(w, c.r, false)
		if b.Title != c.title || b.Body != c.body || b.Category != c.cat {
			t.Errorf("%s: %q / %q / %q, want %q / %q / %q", c.name, b.Title, b.Body, b.Category, c.title, c.body, c.cat)
		}
		if strings.Contains(b.Title, "ExitPlanMode") || strings.Contains(b.Title, "AskUserQuestion") || strings.Contains(b.Title, "mcp__") {
			t.Errorf("%s: tool id in title %q", c.name, b.Title)
		}
		if Words(b.Body) > 16 || utf8.RuneCountInString(b.Body) > commandChars || utf8.RuneCountInString(b.Title) > 50 {
			t.Errorf("%s: over budget %q / %q", c.name, b.Title, b.Body)
		}
		if a := AnsweredBanner(w, "phone", ptr(true), c.r, false); a.Body != c.answeredTo {
			t.Errorf("%s: answered body %q, want %q", c.name, a.Body, c.answeredTo)
		}
	}
	// Hidden commands hide the plan and the question too.
	q := cases[3].r
	if b := PermissionBanner(w, q, true); b.Title != "Question waiting" || strings.Contains(b.Body, "Clerk") {
		t.Errorf("hidden question leaked: %+v", b)
	}
	if a := AnsweredBanner(w, "phone", nil, q, true); a.Body != "" {
		t.Errorf("hidden answered question leaked: %+v", a)
	}
	if b := PermissionBanner(w, cases[0].r, true); b.Title != "Approval needed" || strings.Contains(b.Body, "rate") {
		t.Errorf("hidden plan leaked: %+v", b)
	}
}

// TestPayloads: the alert and Live Activity payloads carry the fields the app reads, under
// the APNs size limit.
func TestPayloads(t *testing.T) {
	w := Where{Hab: "laptop", Session: "s1", SessionName: "fix-login", Project: "web-app", Harness: "claude"}
	req := &api.PermissionRequest{ID: "r1", Tool: "Bash", Command: "make test", ExpiresAt: "2026-11-01T12:05:00Z"}
	p := AlertPayload(PermissionBanner(w, req, false), Meta{Hab: "laptop", Session: "s1", Pane: "p1", State: "blocked", Request: "r1", ExpiresAt: req.ExpiresAt}, "s1")
	if len(p) > MaxPayload {
		t.Fatalf("payload %d bytes", len(p))
	}
	m := decode(t, p)
	aps := m["aps"].(map[string]any)
	alert := aps["alert"].(map[string]any)
	if alert["title"] != "Run make test?" || alert["body"] != "make test" || alert["subtitle"] != "web-app · laptop" {
		t.Errorf("alert %v", alert)
	}
	if aps["category"] != CategoryApproval || aps["interruption-level"] != "time-sensitive" || aps["thread-id"] != "s1" || aps["mutable-content"] != float64(1) || aps["sound"] != "default" {
		t.Errorf("aps %v", aps)
	}
	fd := m["flightdeck"].(map[string]any)
	if fd["v"] != float64(1) || fd["kind"] != "permission" || fd["request"] != "r1" || fd["expires_at"] != req.ExpiresAt || fd["pane"] != "p1" || fd["hab"] != "laptop" {
		t.Errorf("flightdeck %v", fd)
	}
	q := decode(t, AlertPayload(AnsweredBanner(w, "", nil, req, false), Meta{Hab: "laptop"}, ""))
	qa := q["aps"].(map[string]any)
	if qa["interruption-level"] != "passive" || qa["sound"] != nil || qa["category"] != nil {
		t.Errorf("answered aps %v", qa)
	}
	la := decode(t, ActivityPayload(ActivityState{State: "blocked", Kind: "permission", Title: "Run make test?", Request: "r1", Buttons: true, Hab: "laptop"}, 100, false, 0))
	laps := la["aps"].(map[string]any)
	cs := laps["content-state"].(map[string]any)
	if laps["event"] != "update" || laps["timestamp"] != float64(100) || cs["buttons"] != true || cs["request"] != "r1" {
		t.Errorf("activity %v", laps)
	}
	end := decode(t, ActivityPayload(ActivityState{State: "done"}, 100, true, 700))["aps"].(map[string]any)
	if end["event"] != "end" || end["dismissal-date"] != float64(700) {
		t.Errorf("activity end %v", end)
	}
}
