package rounds

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = utc("2026-06-01T12:00:00Z")

func cronTrigger(expr string) Trigger { return Trigger{Kind: TriggerCron, Cron: expr} }

func freshRound() Round {
	return Round{Name: "triage", Mode: ModeFresh, Trigger: cronTrigger("0 9 * * *"),
		Prompt: "triage new issues", Harness: "claude", Project: "proj-1"}
}

func TestNormalizeDefaults(t *testing.T) {
	r := freshRound()
	if err := r.Normalize(testNow, "Europe/Paris", nil); err != nil {
		t.Fatal(err)
	}
	if r.Trigger.Zone != "Europe/Paris" || r.Timeout != Duration(time.Hour) || r.Wait != 0 ||
		r.CatchUp != CatchUpOnce || r.OnFinish != OnFinishClose || r.Notify != NotifyFailures {
		t.Errorf("fresh defaults: %+v", r)
	}

	res := Round{Name: "check", Mode: ModeResume, Trigger: Trigger{Kind: TriggerManual},
		Prompt: "check the deploy", Target: "sess-1"}
	if err := res.Normalize(testNow, "UTC", nil); err != nil {
		t.Fatal(err)
	}
	if res.Wait != Duration(10*time.Minute) || res.OnFinish != "" || res.Trigger.Zone != "" {
		t.Errorf("resume defaults: %+v", res)
	}

	r = freshRound()
	r.Trigger.Zone = "Asia/Tokyo"
	r.Normalize(testNow, "Europe/Paris", nil)
	if r.Trigger.Zone != "Asia/Tokyo" {
		t.Errorf("a set zone was replaced: %q", r.Trigger.Zone)
	}
}

func TestNormalizeRefused(t *testing.T) {
	past := testNow.Add(-time.Minute)
	future := testNow.Add(time.Hour)
	tests := []struct {
		name  string
		edit  func(*Round)
		field string
		want  string
	}{
		{"no name", func(r *Round) { r.Name = "" }, "name", "required"},
		{"name with a space", func(r *Round) { r.Name = "my round" }, "name", "letters, digits"},
		{"name with a dot", func(r *Round) { r.Name = "a.b" }, "name", "letters, digits"},
		{"long name", func(r *Round) { r.Name = strings.Repeat("a", 65) }, "name", "letters, digits"},
		{"no mode", func(r *Round) { r.Mode = "" }, "mode", "required"},
		{"bad mode", func(r *Round) { r.Mode = "detached" }, "mode", "want fresh"},
		{"never runs", func(r *Round) { r.Trigger = cronTrigger("0 0 31 2 *") }, "trigger", "never runs"},
		{"bad cron", func(r *Round) { r.Trigger = cronTrigger("@reboot") }, "trigger", "@ forms"},
		{"past once", func(r *Round) { r.Trigger = Trigger{Kind: TriggerOnce, At: &past} }, "trigger", "in the past"},
		{"once now", func(r *Round) { now := testNow; r.Trigger = Trigger{Kind: TriggerOnce, At: &now} }, "trigger", "in the past"},
		{"at on cron", func(r *Round) { r.Trigger.At = &future }, "trigger", "once trigger only"},
		{"cron on manual", func(r *Round) { r.Trigger = Trigger{Kind: TriggerManual, Cron: "* * * * *"} }, "trigger", "cron trigger only"},
		{"event", func(r *Round) { r.Trigger = Trigger{Kind: TriggerEvent} }, "trigger", "not supported"},
		{"prompt over 64 KiB", func(r *Round) { r.Prompt = strings.Repeat("x", 64<<10+1) }, "prompt", "65537 bytes"},
		{"prompt not UTF-8", func(r *Round) { r.Prompt = "\xff" }, "prompt", "UTF-8"},
		{"fresh without project", func(r *Round) { r.Project = "" }, "project", "required"},
		{"fresh with target", func(r *Round) { r.Target = "s" }, "target", "resume and fork"},
		{"fresh without prompt", func(r *Round) { r.Prompt = "" }, "prompt", "required unless"},
		{"fresh without harness", func(r *Round) { r.Harness = "" }, "harness", "required"},
		{"command with prompt", func(r *Round) { r.Command = []string{"make"} }, "command", "no prompt"},
		{"command with model", func(r *Round) { r.Command = []string{"make"}; r.Prompt, r.Harness, r.Model = "", "", "m" }, "command", "no prompt"},
		{"command empty name", func(r *Round) { r.Command = []string{""}; r.Prompt, r.Harness = "", "" }, "command", "empty"},
		{"resume without target", func(r *Round) { r.Mode, r.Harness, r.Project = ModeResume, "", "" }, "target", "required"},
		{"fork with harness", func(r *Round) { r.Mode, r.Target, r.Project = ModeFork, "s", "" }, "harness", "target's harness"},
		{"fork with project", func(r *Round) { r.Mode, r.Target, r.Harness = ModeFork, "s", "" }, "project", "target's folder"},
		{"resume with command", func(r *Round) {
			r.Mode, r.Target, r.Harness, r.Project, r.Command = ModeResume, "s", "", "", []string{"x"}
		}, "command", "fresh rounds only"},
		{"resume with on_finish", func(r *Round) {
			r.Mode, r.Target, r.Harness, r.Project, r.OnFinish = ModeResume, "s", "", "", OnFinishKeep
		}, "on_finish", "fresh and fork"},
		{"wait on fresh", func(r *Round) { r.Wait = Duration(time.Minute) }, "wait", "resume rounds only"},
		{"negative timeout", func(r *Round) { r.Timeout = -1 }, "timeout", "positive"},
		{"negative max_runs", func(r *Round) { r.MaxRuns = -1 }, "max_runs", "negative"},
		{"past expiry", func(r *Round) { r.ExpiresAt = &past }, "expires_at", "in the past"},
		{"bad catch_up", func(r *Round) { r.CatchUp = "all" }, "catch_up", "once or skip"},
		{"bad on_finish", func(r *Round) { r.OnFinish = "archive" }, "on_finish", "close or keep"},
		{"bad notify", func(r *Round) { r.Notify = "success" }, "notify", "failures, always or never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := freshRound()
			tt.edit(&r)
			err := r.Normalize(testNow, "UTC", nil)
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != tt.field || !strings.Contains(fe.Reason, tt.want) {
				t.Errorf("err = %v, want %s: ...%s", err, tt.field, tt.want)
			}
		})
	}
}

func TestNormalizeAccepted(t *testing.T) {
	future := testNow.Add(time.Hour)
	tests := []struct {
		name string
		edit func(*Round)
	}{
		{"prompt of exactly 64 KiB", func(r *Round) { r.Prompt = strings.Repeat("x", 64<<10) }},
		{"command round", func(r *Round) { r.Command = []string{"make", "test"}; r.Prompt, r.Harness = "", "" }},
		{"fork with model", func(r *Round) { r.Mode, r.Target, r.Harness, r.Project, r.Model = ModeFork, "s", "", "", "m" }},
		{"once in the future", func(r *Round) { r.Trigger = Trigger{Kind: TriggerOnce, At: &future} }},
		{"names", func(r *Round) { r.Name = "Nightly_sweep-2" }},
		{"limits", func(r *Round) { r.MaxRuns, r.ExpiresAt, r.Timeout = 3, &future, Duration(5*time.Minute) }},
	}
	for _, tt := range tests {
		r := freshRound()
		tt.edit(&r)
		if err := r.Normalize(testNow, "UTC", nil); err != nil {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

// On an edit, a once instant or an expiry that has passed since creation does not block a
// change to another field.
func TestNormalizeEditKeepsPastValues(t *testing.T) {
	at := testNow.Add(time.Hour)
	r := freshRound()
	r.Trigger = Trigger{Kind: TriggerOnce, At: &at}
	r.ExpiresAt = &at
	if err := r.Normalize(testNow, "UTC", nil); err != nil {
		t.Fatal(err)
	}
	later := testNow.Add(2 * time.Hour)
	edit := r
	edit.Prompt = "new text"
	if err := edit.Normalize(later, "UTC", &r); err != nil {
		t.Errorf("edit after the instant passed: %v", err)
	}
	edit.Trigger.At = &later
	if err := edit.Normalize(later, "UTC", &r); err == nil {
		t.Error("a changed once instant in the past was accepted")
	}
}

func TestRunStateFinal(t *testing.T) {
	final := map[RunState]bool{RunQueued: false, RunWaiting: false, RunRunning: false, RunBlocked: false,
		RunSucceeded: true, RunFailed: true, RunTimedOut: true, RunSkipped: true, RunTakenOver: true}
	for s, want := range final {
		if s.Final() != want {
			t.Errorf("%s.Final() = %v", s, !want)
		}
	}
}

func TestTruncateResult(t *testing.T) {
	if s := TruncateResult("short"); s != "short" {
		t.Error(s)
	}
	long := strings.Repeat("a", MaxResultBytes-1) + "é" + "tail" // é straddles the limit
	got := TruncateResult(long)
	if len(got) != MaxResultBytes-1 || !strings.HasSuffix(got, "a") {
		t.Errorf("len %d", len(got))
	}
}
