// Package rounds holds the round definition and its checks, the cron and zone rules that
// compute a round's slots, and the rounds store with its run log (docs/specs/rounds.md 2, 3,
// 4.1, 4.2, 7.6, 7.7). The scheduler, the API and run sessions build on it in later stages.
package rounds

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Mode says how a run reaches its session (rounds.md 5).
type Mode string

const (
	ModeFresh  Mode = "fresh"
	ModeResume Mode = "resume"
	ModeFork   Mode = "fork"
)

// CatchUp says what a round does with slots missed while the runner slept (rounds.md 8.2).
type CatchUp string

const (
	CatchUpOnce CatchUp = "once"
	CatchUpSkip CatchUp = "skip"
)

// OnFinish says what happens to a fresh or fork run's session after a successful run (7.6).
type OnFinish string

const (
	OnFinishClose OnFinish = "close"
	OnFinishKeep  OnFinish = "keep"
)

// Notify says which run ends raise a notification (rounds.md 10.2).
type Notify string

const (
	NotifyFailures Notify = "failures"
	NotifyAlways   Notify = "always"
	NotifyNever    Notify = "never"
)

// Limits and defaults of the definition.
const (
	MaxPromptBytes = 64 << 10 // the channel message limit (rounds.md 11)
	MaxNameLen     = 64
	DefaultTimeout = 60 * time.Minute
	DefaultWait    = 10 * time.Minute
)

// Duration is a time.Duration that is written to JSON as text such as "1h0m0s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Round is one round's definition (rounds.md 3). The scheduler's view of it (next slot,
// counts) is RoundState.
type Round struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Mode    Mode    `json:"mode"`
	Trigger Trigger `json:"trigger"`
	Prompt  string  `json:"prompt,omitempty"`
	// Harness, Model and HarnessArgs start the harness of a fresh run, as session.create does.
	// Model and HarnessArgs also apply when a resume or fork run starts its harness.
	Harness     string   `json:"harness,omitempty"`
	Model       string   `json:"model,omitempty"`
	HarnessArgs []string `json:"harness_args,omitempty"`
	// Command is a plain program (argv) a fresh run starts instead of a harness and a prompt.
	Command []string `json:"command,omitempty"`
	Project string   `json:"project,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Target  string   `json:"target,omitempty"`
	// MaxRuns completes the round after this many finished runs; 0 is no limit.
	MaxRuns   int        `json:"max_runs,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Timeout   Duration   `json:"timeout"`
	// Wait is how long a resume run waits for the lock; zero for the other modes.
	Wait     Duration `json:"wait,omitempty"`
	CatchUp  CatchUp  `json:"catch_up"`
	OnFinish OnFinish `json:"on_finish,omitempty"`
	Notify   Notify   `json:"notify"`
	Paused   bool     `json:"paused,omitempty"`
	// CreatedBy is the session that created the round, when an agent did. The hab sets it;
	// it never changes.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FieldError is a definition check that failed, naming the field.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

func fieldErr(field, format string, a ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, a...)}
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Normalize fills the defaults and checks the definition. zone is the zone a cron or once
// trigger with no zone takes (the host's zone). prev is the stored definition on an edit, nil
// on create: the checks against now (a past once instant, a past expiry) apply only to a
// value that is new or changed, so an edit to another field never fails on them. Name
// uniqueness is the store's check.
func (r *Round) Normalize(now time.Time, zone string, prev *Round) error {
	if r.Name == "" {
		return fieldErr("name", "required")
	}
	if len(r.Name) > MaxNameLen || !nameRE.MatchString(r.Name) {
		return fieldErr("name", "use up to %d letters, digits, - and _", MaxNameLen)
	}

	t := &r.Trigger
	if (t.Kind == TriggerCron || t.Kind == TriggerOnce) && t.Zone == "" {
		t.Zone = zone
	}
	if t.At != nil {
		at := t.At.UTC()
		t.At = &at
	}
	sched, err := t.Schedule()
	if err != nil {
		return fieldErr("trigger", "%v", err)
	}
	triggerChanged := prev == nil || !prev.Trigger.Equal(*t)
	if t.Kind == TriggerOnce && triggerChanged {
		if _, ok := sched.Next(now); !ok {
			return fieldErr("trigger", "once: %s is in the past", t.At.Format(time.RFC3339))
		}
	}
	if t.Kind != TriggerCron && t.Cron != "" {
		return fieldErr("trigger", "cron is for a cron trigger only")
	}
	if t.Kind != TriggerOnce && t.At != nil {
		return fieldErr("trigger", "at is for a once trigger only")
	}

	if len(r.Prompt) > MaxPromptBytes {
		return fieldErr("prompt", "%d bytes; the limit is %d", len(r.Prompt), MaxPromptBytes)
	}
	if !utf8.ValidString(r.Prompt) {
		return fieldErr("prompt", "not valid UTF-8")
	}
	harnessSet := r.Harness != "" || r.Model != "" || len(r.HarnessArgs) > 0

	switch r.Mode {
	case ModeFresh:
		if r.Project == "" {
			return fieldErr("project", "required for a fresh round")
		}
		if r.Target != "" {
			return fieldErr("target", "for resume and fork rounds only")
		}
		if len(r.Command) > 0 {
			if r.Command[0] == "" {
				return fieldErr("command", "the program name is empty")
			}
			if r.Prompt != "" || harnessSet {
				return fieldErr("command", "a command round takes no prompt, harness, model or harness_args")
			}
		} else {
			if r.Prompt == "" {
				return fieldErr("prompt", "required unless the round runs a command")
			}
			if r.Harness == "" {
				return fieldErr("harness", "required for a fresh round with a prompt")
			}
		}
	case ModeResume, ModeFork:
		if r.Target == "" {
			return fieldErr("target", "required for a %s round", r.Mode)
		}
		if r.Prompt == "" {
			return fieldErr("prompt", "required")
		}
		if len(r.Command) > 0 {
			return fieldErr("command", "for fresh rounds only")
		}
		if r.Harness != "" {
			return fieldErr("harness", "a %s round uses its target's harness", r.Mode)
		}
		if r.Project != "" {
			return fieldErr("project", "a %s round works in its target's folder", r.Mode)
		}
		if r.Cwd != "" {
			return fieldErr("cwd", "a %s round works in its target's folder", r.Mode)
		}
	case "":
		return fieldErr("mode", "required")
	default:
		return fieldErr("mode", "%q: want fresh, resume or fork", r.Mode)
	}

	if r.MaxRuns < 0 {
		return fieldErr("max_runs", "must not be negative")
	}
	if r.ExpiresAt != nil {
		e := r.ExpiresAt.UTC()
		r.ExpiresAt = &e
		changed := prev == nil || prev.ExpiresAt == nil || !prev.ExpiresAt.Equal(e)
		if changed && !e.After(now) {
			return fieldErr("expires_at", "%s is in the past", e.Format(time.RFC3339))
		}
	}
	switch {
	case r.Timeout == 0:
		r.Timeout = Duration(DefaultTimeout)
	case r.Timeout < 0:
		return fieldErr("timeout", "must be positive")
	}
	if r.Mode == ModeResume {
		switch {
		case r.Wait == 0:
			r.Wait = Duration(DefaultWait)
		case r.Wait < 0:
			return fieldErr("wait", "must be positive")
		}
	} else if r.Wait != 0 {
		return fieldErr("wait", "for resume rounds only")
	}
	switch r.CatchUp {
	case "":
		r.CatchUp = CatchUpOnce
	case CatchUpOnce, CatchUpSkip:
	default:
		return fieldErr("catch_up", "%q: want once or skip", r.CatchUp)
	}
	switch r.OnFinish {
	case "":
		if r.Mode != ModeResume {
			r.OnFinish = OnFinishClose
		}
	case OnFinishClose, OnFinishKeep:
		if r.Mode == ModeResume {
			return fieldErr("on_finish", "for fresh and fork rounds only")
		}
	default:
		return fieldErr("on_finish", "%q: want close or keep", r.OnFinish)
	}
	switch r.Notify {
	case "":
		r.Notify = NotifyFailures
	case NotifyFailures, NotifyAlways, NotifyNever:
	default:
		return fieldErr("notify", "%q: want failures, always or never", r.Notify)
	}
	return nil
}

// RunState is a run's state (rounds.md 7.1).
type RunState string

const (
	RunQueued    RunState = "queued"
	RunWaiting   RunState = "waiting"
	RunRunning   RunState = "running"
	RunBlocked   RunState = "blocked"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunTimedOut  RunState = "timed_out"
	RunSkipped   RunState = "skipped"
	RunTakenOver RunState = "taken_over"
)

// Final reports whether the run has ended. A run in a final state never changes state again.
func (s RunState) Final() bool {
	switch s {
	case RunSucceeded, RunFailed, RunTimedOut, RunSkipped, RunTakenOver:
		return true
	}
	return false
}

// MaxResultBytes bounds the final agent message a run keeps (rounds.md 7.3).
const MaxResultBytes = 4 << 10

// Run is one run's record (rounds.md 7.7). Logbook takes the same fields later.
type Run struct {
	ID      string `json:"id"`
	Number  int    `json:"number"`
	RoundID string `json:"round_id"`
	// Slot is the slot the run serves; nil for a manual or event run.
	Slot    *time.Time  `json:"slot,omitempty"`
	Trigger TriggerKind `json:"trigger"`
	// Missed is how many slots the run covered: 1 on time, more after a catch-up.
	Missed    int        `json:"missed,omitempty"`
	State     RunState   `json:"state"`
	Reason    string     `json:"reason,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Created   time.Time  `json:"created"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Result is the first MaxResultBytes of the final agent message.
	Result string `json:"result,omitempty"`
}

// TruncateResult cuts a final agent message to MaxResultBytes on a UTF-8 boundary.
func TruncateResult(s string) string {
	if len(s) <= MaxResultBytes {
		return s
	}
	i := MaxResultBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// NewID returns a random id with a kind prefix ("round", "run"). Random ids never repeat, even
// after the state file is lost.
func NewID(kind string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("rounds: crypto/rand failed: " + err.Error())
	}
	return kind + "-" + hex.EncodeToString(b[:])
}
