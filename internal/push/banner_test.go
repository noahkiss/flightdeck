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
		{"question", &api.PermissionRequest{ID: "q1", Tool: "AskUserQuestion", Input: in(`{"questions":[{"question":"Which auth library should we use?","header":"Auth","options":[{"label":"Clerk","description":"hosted"},{"label":"Auth.js"}],"multiSelect":false}]}`)},
			"Which auth library should we use?", "1 Clerk · 2 Auth.js", CategoryOpen, "Allowed: question"},
		{"question, no input", &api.PermissionRequest{ID: "q2", Tool: "AskUserQuestion"},
			"Question waiting", "", CategoryOpen, "Allowed: question"},
		{"mcp tool", &api.PermissionRequest{ID: "m1", Tool: "mcp__github__create_issue"},
			"Allow github create issue?", "", CategoryDeny, "Allowed:"},
		{"fetch", &api.PermissionRequest{ID: "f1", Tool: "WebFetch", Input: in(`{"url":"https://docs.example.com/guide/","prompt":"summarise"}`)},
			"Fetch docs.example.com?", "docs.example.com/guide", CategoryApproval, "Allowed:"},
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
	if b := PermissionBanner(w, cases[2].r, true); b.Title != "Approval needed" || strings.Contains(b.Body, "Clerk") {
		t.Errorf("hidden question leaked: %+v", b)
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
