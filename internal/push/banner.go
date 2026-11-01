package push

import (
	"encoding/json"
	"fmt"
	"path"
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
)

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
	}
	return "", false
}

// permissionTitle is a verb and object in 4 words and 28 characters: the harness's own title
// when it fits whole, else one built from the tool.
func permissionTitle(r *api.PermissionRequest) string {
	if r == nil {
		return "Approval needed"
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
	if r.Tool != "" {
		return Fit("Allow "+r.Tool, 3, 27) + "?"
	}
	return "Approval needed"
}

// PermissionBanner is the banner for blocked:permission (16.2). Approve is offered only when
// the whole command fits after the safe drops, and never when commands are hidden.
func PermissionBanner(w Where, r *api.PermissionRequest, hide bool) Banner {
	b := Banner{Kind: KindPermission, Subtitle: w.place(), Level: LevelTimeSensitive, Category: CategoryDeny}
	if hide {
		b.Title = "Approval needed"
		b.Body = w.place()
		return b
	}
	b.Title = permissionTitle(r)
	text, whole := requestText(r)
	b.Body = text
	if whole {
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
	obj := ""
	if r != nil && !hide {
		if r.Command != "" {
			obj = verbObject(ShortCommand(r.Command, r.Cwd))
		} else if len(r.Files) > 0 {
			obj = path.Base(r.Files[0])
			if len(r.Files) > 1 {
				obj = fmt.Sprintf("%d files", len(r.Files))
			}
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
