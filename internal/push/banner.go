package push

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Level is a banner's interruption level.
type Level string

const (
	LevelPassive       Level = "passive"
	LevelActive        Level = "active"
	LevelTimeSensitive Level = "time-sensitive"
)

// Categories name the action sets the app registers (app.md 7.1, 16.2).
const (
	CategoryApproval = "approval"      // Approve, Deny
	CategoryDeny     = "approval-deny" // Deny, Open on iPhone: the command does not fit, or is hidden
	CategoryOpen     = "open"          // Open on iPhone
	// The same sets with "Ask" last, sent while some host runs Capcom (capcom.md 8.5), and ask
	// alone for a banner that had no actions.
	CategoryApprovalAsk = "approval-ask"      // Approve, Deny, Ask
	CategoryDenyAsk     = "approval-deny-ask" // Deny, Open on iPhone, Ask
	CategoryOpenAsk     = "open-ask"          // Open on iPhone, Ask
	CategoryAsk         = "ask"               // Ask
)

// AskCategory is category with the Ask action added.
func AskCategory(category string) string {
	switch category {
	case CategoryApproval:
		return CategoryApprovalAsk
	case CategoryDeny:
		return CategoryDenyAsk
	case CategoryOpen:
		return CategoryOpenAsk
	case "":
		return CategoryAsk
	}
	return category
}

// Ask is what a banner's Ask action hands to capcom.open as its one context item (capcom.md
// 8.5): a notice id, with the device id of the hab that raised it when that hab is not the
// Capcom home host.
type Ask struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Host string `json:"host,omitempty"`
}

// Kinds of push, as push.register names them, plus notice for notice.raise.
const (
	KindPermission = "permission"
	KindQuestion   = "question"
	KindAuth       = "auth"
	KindError      = "error"
	KindDone       = "done"
	KindAnswered   = "answered"
	KindNotice     = "notice"
)

// Banner is one alert's words and actions.
type Banner struct {
	Kind     string
	Title    string
	Subtitle string
	Body     string
	Level    Level
	Category string // empty: no actions
}

// Where names what a banner is about, in the words a person reads.
type Where struct {
	Hab          string // the hab's label
	Session      string // session id
	SessionName  string
	SessionTitle string
	Project      string // project name; empty when unfiled
	Harness      string // claude, codex, opencode, ...
}

// Command budget inside a permission body: 16 words and 100 characters (app.md 16.2), less
// room for the " · Expires 14:05" the phone adds from expires_at (16.1).
const (
	commandWords = 13
	commandChars = 84
)

var harnessNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "opencode": "opencode", "pi": "pi"}

// harnessName is the harness's product name, not its id.
func harnessName(id string) string {
	if n, ok := harnessNames[id]; ok {
		return n
	}
	if id == "" {
		return "The agent"
	}
	return id
}

// place is the subtitle "project · hab" in 4 words (16.2). Unfiled sessions use their name.
func (w Where) place() string {
	what := w.Project
	if what == "" {
		what = w.SessionName
	}
	hab := Fit(w.Hab, 2, 16)
	if what == "" {
		return hab
	}
	return Fit(what, max(1, 4-Words(hab)), 24) + " · " + hab
}

// requestText is what a permission request asks to run or touch, shortened, and whether it is
// whole after the safe drops.
func requestText(r *api.PermissionRequest) (text string, whole bool) {
	switch {
	case r == nil:
		return "", false
	case r.Tool == toolPlan:
		s := planText(r)
		return Fit(s, commandWords, commandChars), s != "" && Fits(s, commandWords, commandChars)
	case r.Tool == toolQuestion:
		// The options are shown, never answered from the banner: whole stays false.
		_, opts, _ := questionText(r)
		return Fit(opts, commandWords, commandChars), false
	case r.Command != "":
		s := ShortCommand(r.Command, r.Cwd)
		return Fit(s, commandWords, commandChars), Fits(s, commandWords, commandChars)
	case len(r.Files) > 0:
		names := make([]string, len(r.Files))
		for i, f := range r.Files {
			names[i] = path.Base(f)
		}
		s := strings.Join(names, ", ")
		return Fit(s, commandWords, commandChars), Fits(s, commandWords, commandChars)
	case inputURL(r) != "":
		s := inputURL(r)
		return Fit(s, commandWords, commandChars), Fits(s, commandWords, commandChars)
	}
	return "", false
}

// Claude Code tools whose banners read as a plan or a question, not as a tool call
// (landscape.md 4.4).
const (
	toolPlan     = "ExitPlanMode"
	toolQuestion = "AskUserQuestion"
)

// inputField is a string field of the request's tool input, or "".
func inputField(r *api.PermissionRequest, key string) string {
	if r == nil || len(r.Input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r.Input, &m) != nil {
		return ""
	}
	var v string
	if json.Unmarshal(m[key], &v) != nil {
		return ""
	}
	return v
}

// toolFetch is Claude Code's fetch tool. Only its url is the whole request; another tool with
// a url (a browser or webhook MCP tool) has other arguments the banner would hide.
const toolFetch = "WebFetch"

// inputURL is a WebFetch request's url without its scheme or a trailing slash, else "".
func inputURL(r *api.PermissionRequest) string {
	if r == nil || r.Tool != toolFetch {
		return ""
	}
	u := inputField(r, "url")
	for _, p := range []string{"https://", "http://"} {
		u = strings.TrimPrefix(u, p)
	}
	return strings.TrimRight(u, "/")
}

// lineMark is one Markdown mark at the start of a line: a heading, a quote, a bullet or a list
// number, each followed by a space. "--force" keeps its dashes.
var lineMark = regexp.MustCompile(`^(?:#{1,6}|>|[-*+]|\d+[.)])\s+`)

// planText is the plan of an ExitPlanMode request in plain words: Markdown heading, quote,
// list and emphasis marks dropped, a heading that only says "Plan" left out, lines joined
// with " · ".
func planText(r *api.PermissionRequest) string {
	var out []string
	for _, line := range strings.Split(inputField(r, "plan"), "\n") {
		line = strings.TrimSpace(line)
		line = lineMark.ReplaceAllString(line, "")
		line = strings.NewReplacer("**", "", "__", "", "`", "").Replace(line)
		if line != "" && !strings.EqualFold(strings.TrimRight(line, ":"), "plan") {
			out = append(out, line)
		}
	}
	return strings.Join(out, " · ")
}

// questionText is the first question of an AskUserQuestion request and its options, numbered
// by their place: "1 Clerk · 2 Auth.js" (app.md 16.2). An option with no label is left out.
// more counts the questions after the first.
func questionText(r *api.PermissionRequest) (question, options string, more int) {
	if r == nil || len(r.Input) == 0 {
		return "", "", 0
	}
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(r.Input, &in) != nil || len(in.Questions) == 0 {
		return "", "", 0
	}
	q := in.Questions[0]
	opts := make([]string, 0, len(q.Options))
	for i, o := range q.Options {
		if label := strings.Join(strings.Fields(o.Label), " "); label != "" {
			opts = append(opts, fmt.Sprintf("%d %s", i+1, label))
		}
	}
	return strings.Join(strings.Fields(q.Question), " "), strings.Join(opts, " · "), len(in.Questions) - 1
}

// questionTitle is the first question in the question budget, 8 words and 50 characters, with
// "(+N more)" when more questions follow.
func questionTitle(r *api.PermissionRequest) string {
	q, _, more := questionText(r)
	if q == "" {
		return "Question waiting"
	}
	if more == 0 {
		return Fit(q, 8, 50)
	}
	tail := fmt.Sprintf(" (+%d more)", more)
	return Fit(q, 6, 50-len(tail)) + tail
}

// toolName is a tool's name in plain words: the MCP tool "mcp__github__create_issue" reads
// "github create issue".
func toolName(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		tool = strings.NewReplacer("__", " ", "_", " ").Replace(rest)
	}
	return tool
}

// permissionTitle is a verb and object in 4 words and 28 characters: the harness's own title
// when it fits whole, else one built from the tool.
func permissionTitle(r *api.PermissionRequest) string {
	if r == nil {
		return "Approval needed"
	}
	switch r.Tool {
	case toolPlan:
		return "Approve plan?"
	case toolQuestion:
		return questionTitle(r)
	}
	if r.Title != "" && Fits(r.Title, 4, 28) {
		return r.Title
	}
	switch {
	case r.Command != "":
		if obj := verbObject(ShortCommand(r.Command, r.Cwd)); obj != "" {
			return Fit("Run "+obj, 3, 27) + "?"
		}
	case len(r.Files) == 1:
		return Fit("Edit "+path.Base(r.Files[0]), 3, 27) + "?"
	case len(r.Files) > 1:
		return fmt.Sprintf("Edit %d files?", len(r.Files))
	}
	if u := inputURL(r); u != "" {
		return Fit("Fetch "+strings.SplitN(u, "/", 2)[0], 2, 27) + "?"
	}
	if r.Tool != "" {
		return Fit("Allow "+toolName(r.Tool), 4, 27) + "?"
	}
	return "Approval needed"
}

// PermissionBanner is the banner for blocked:permission (16.2). Approve is offered only when
// the whole command fits after the safe drops, and never when commands are hidden. A request
// with no id cannot be answered through approval.answer, so its banner only opens the app.
func PermissionBanner(w Where, r *api.PermissionRequest, hide bool) Banner {
	b := Banner{Kind: KindPermission, Subtitle: w.place(), Level: LevelTimeSensitive, Category: CategoryDeny}
	// A question is answered with its options in the app: Deny would read as a refusal to
	// answer, and the hab takes no option from a banner yet.
	if r == nil || r.ID == "" || r.Tool == toolQuestion {
		b.Category = CategoryOpen
	}
	if hide {
		b.Title = "Approval needed"
		if r != nil && r.Tool == toolQuestion {
			b.Title = "Question waiting"
		}
		b.Body = w.place()
		return b
	}
	b.Title = permissionTitle(r)
	text, whole := requestText(r)
	b.Body = text
	if whole && b.Category == CategoryDeny {
		b.Category = CategoryApproval
	}
	return b
}

// QuestionBanner is the banner for blocked:question. The agent state carries no question text
// yet, so the banner asks the owner to open it.
func QuestionBanner(w Where) Banner {
	return Banner{Kind: KindQuestion, Title: Fit(harnessName(w.Harness)+" asks a question", 8, 50), Subtitle: w.place(),
		Body: "Open to answer.", Level: LevelTimeSensitive, Category: CategoryOpen}
}

// AuthBanner is the banner for blocked:auth.
func AuthBanner(w Where) Banner {
	return Banner{Kind: KindAuth, Title: "Sign-in needed", Subtitle: w.place(),
		Body: Fit(harnessName(w.Harness)+" needs you to sign in on "+w.Hab+".", 10, 0), Level: LevelActive, Category: CategoryOpen}
}

// ErrorBanner is the banner for error: "Turn stopped: <project>", from hab and harness.
func ErrorBanner(w Where) Banner {
	what := w.Project
	if what == "" {
		what = w.SessionName
	}
	return Banner{Kind: KindError, Title: Fit("Turn stopped: "+what, 4, 32), Subtitle: Fit(w.Hab, 2, 16) + " · " + Fit(harnessName(w.Harness), 2, 16),
		Body: "The agent stopped on an error.", Level: LevelActive, Category: CategoryOpen}
}

// DoneBanner is the passive banner for done, sent only to devices that asked for it.
func DoneBanner(w Where) Banner {
	name := w.SessionTitle
	if name == "" {
		name = w.SessionName
	}
	return Banner{Kind: KindDone, Title: Fit("Done: "+name, 5, 40), Subtitle: w.place(), Body: "The agent finished its turn.", Level: LevelPassive}
}

// AnsweredBanner replaces an approval banner once the request resolved (7.5): where it was
// answered, and what. by is the answering client's label; empty means the terminal. allow is
// nil when the hab does not know the answer.
func AnsweredBanner(w Where, by string, allow *bool, r *api.PermissionRequest, hide bool) Banner {
	title := "Answered in the terminal"
	if by != "" {
		title = Fit("Answered on "+by, 4, 32)
	}
	b := Banner{Kind: KindAnswered, Title: title, Subtitle: w.place(), Level: LevelPassive}
	if r != nil && r.Tool == toolQuestion {
		// A question is answered, not allowed: the body is the question.
		if !hide {
			q, _, _ := questionText(r)
			b.Body = Fit(q, 8, 60)
		}
		return b
	}
	obj := ""
	if r != nil && !hide {
		switch {
		case r.Command != "":
			obj = verbObject(ShortCommand(r.Command, r.Cwd))
		case len(r.Files) == 1:
			obj = path.Base(r.Files[0])
		case len(r.Files) > 1:
			obj = fmt.Sprintf("%d files", len(r.Files))
		case r.Tool == toolPlan:
			obj = "plan"
		case inputURL(r) != "":
			obj = "fetch " + strings.SplitN(inputURL(r), "/", 2)[0]
		case r.Tool != "":
			obj = toolName(r.Tool)
		}
	}
	switch {
	case allow != nil && *allow:
		b.Body = Fit(strings.TrimSpace("Allowed: "+obj), 8, 60)
	case allow != nil:
		b.Body = Fit(strings.TrimSpace("Denied: "+obj), 8, 60)
	default:
		b.Body = Fit(obj, 8, 60)
	}
	return b
}

// ClosedBanner replaces an open approval banner whose pane closed: the request can no longer
// be answered.
func ClosedBanner(w Where, r *api.PermissionRequest, hide bool) Banner {
	b := AnsweredBanner(w, "", nil, r, hide)
	b.Title = "Pane closed"
	return b
}

// EndedBanner replaces an open approval banner whose pane's program exited: the session
// ended, so nobody answered the request.
func EndedBanner(w Where, r *api.PermissionRequest, hide bool) Banner {
	b := AnsweredBanner(w, "", nil, r, hide)
	b.Title = "Session ended"
	return b
}

// NoticeBanner is a notice from notice.raise, fitted to the question budgets (16.2).
func NoticeBanner(w Where, title, body string, level Level) Banner {
	if level == "" {
		level = LevelActive
	}
	b := Banner{Kind: KindNotice, Title: Fit(title, 8, 50), Body: Fit(body, 16, 100), Level: level}
	if w.Session != "" {
		b.Subtitle = w.place()
	} else {
		b.Subtitle = Fit(w.Hab, 2, 16)
	}
	return b
}

// Meta is the small part of a push the app reads before it loads the rest (app.md 8.3).
type Meta struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	Hab       string `json:"hab"`
	Session   string `json:"session,omitempty"`
	Pane      string `json:"pane,omitempty"`
	State     string `json:"state,omitempty"`
	Request   string `json:"request,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Notice    string `json:"notice,omitempty"`
	// Context is the Ask action's context item, and CapcomHome the device id of the hab that
	// runs Capcom, where capcom.open goes. Both absent: no host runs Capcom, and no Ask.
	Context    *Ask   `json:"capcom_context,omitempty"`
	CapcomHome string `json:"capcom_home,omitempty"`
}

// AlertPayload is the JSON of an alert push. The phone's notification service extension adds
// the expiry as a local clock time (mutable-content).
func AlertPayload(b Banner, m Meta, thread string) []byte {
	m.V = 1
	m.Kind = b.Kind
	alert := map[string]string{"title": b.Title, "body": b.Body}
	if b.Subtitle != "" {
		alert["subtitle"] = b.Subtitle
	}
	aps := map[string]any{
		"alert":              alert,
		"interruption-level": string(b.Level),
		"mutable-content":    1,
	}
	if b.Level != LevelPassive {
		aps["sound"] = "default"
	}
	if b.Category != "" {
		aps["category"] = b.Category
	}
	if thread != "" {
		aps["thread-id"] = thread
	}
	out, _ := json.Marshal(map[string]any{"aps": aps, "flightdeck": m})
	return out
}

// ActivityState is the content state of a Live Activity (app.md 9.1). The app's widget decodes
// it; the keys are the contract.
type ActivityState struct {
	State   string `json:"state"`
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Request string `json:"request,omitempty"`
	Pane    string `json:"pane,omitempty"`
	Buttons bool   `json:"buttons"`
	Since   string `json:"since"`
	Others  int    `json:"others"`
	Hab     string `json:"hab"`
	Harness string `json:"harness,omitempty"`
}

// ActivityPayload is the JSON of a Live Activity update, or of its end when dismiss is set.
func ActivityPayload(st ActivityState, now int64, end bool, dismiss int64) []byte {
	aps := map[string]any{"timestamp": now, "event": "update", "content-state": st}
	if end {
		aps["event"] = "end"
		aps["dismissal-date"] = dismiss
	}
	out, _ := json.Marshal(map[string]any{"aps": aps})
	return out
}
