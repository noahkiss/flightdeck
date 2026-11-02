package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/push"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestRoundNoticePush: the scheduler's notices through gff-hab (rounds.md 10.2): each raises a
// kept notice, a notice.raised event naming the round, and a push of kind round to the device
// that registered it, collapsed by round id. The run's result goes into the kept notice only
// where the read scope allows the run's folder. The round's notify setting is applied again:
// a run failure of a notify = never round raises nothing, its self-pause still does.
func TestRoundNoticePush(t *testing.T) {
	state := t.TempDir()
	writePushConfig(t, state)
	st, err := structure.Open(structure.Config{Dir: filepath.Join(state, "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sent := &sentLog{}
	var mu sync.Mutex
	var events []api.Notice
	pushes := newPushService(pushSetup{
		state: state, hab: "laptop", self: "dev-self",
		getenv: func(string) string { return "" }, logf: t.Logf, sender: sent,
		connected: func(string) bool { return false }, label: func(string) string { return "" },
		publish: func(e structure.Event) {
			if e.Type == control.EventNoticeRaised {
				mu.Lock()
				events = append(events, e.Data.(api.Notice))
				mu.Unlock()
			}
		},
		notices: openNoticeLog(state, nil, t.Logf),
		denied:  func(folder string) bool { return folder == "/secret/app" },
	}, st, harness.NewStore(harness.Config{}))
	defer pushes.Close()
	for dev, kinds := range map[string][]api.PushKind{"dev-phone": {"round"}, "dev-mac": {"permission"}} {
		tok := strings.Repeat("ab", 32)
		if dev == "dev-mac" {
			tok = strings.Repeat("cd", 32)
		}
		if err := pushes.Register("c-"+dev, api.PushRegisterParams{DeviceID: dev, Token: tok, Environment: "development", Kinds: kinds}); err != nil {
			t.Fatal(err)
		}
	}
	folders := map[string]string{"s-open": "/work/app", "s-secret": "/secret/app"}
	svc := &roundsService{cfg: roundsConfig{logf: t.Logf, raise: func(ev push.RoundEvent, d, f, dir string) { pushes.raiseRound(ev, d, f, dir) },
		folder: func(_ context.Context, session string) string { return folders[session] }}}

	entry := func(id, name string, notify rounds.Notify) rounds.Entry {
		return rounds.Entry{Round: rounds.Round{ID: id, Name: name, Notify: notify, CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)},
			State: rounds.RoundState{Failures: 5}}
	}
	run := func(n int, state rounds.RunState, reason, session, result string) *rounds.Run {
		return &rounds.Run{ID: "run" + string(rune('0'+n)), Number: n, State: state, Reason: reason, SessionID: session, Result: result}
	}
	svc.Notice(rounds.Notice{Kind: rounds.NoticeRunFailed, Round: entry("r1", "nightly", rounds.NotifyFailures),
		Run: run(1, rounds.RunFailed, "exit status 2", "s-open", "the build broke\nat step 3")})
	svc.Notice(rounds.Notice{Kind: rounds.NoticeRunFailed, Round: entry("r1", "nightly", rounds.NotifyFailures),
		Run: run(2, rounds.RunTimedOut, "timed out", "s-secret", "private words")})
	svc.Notice(rounds.Notice{Kind: rounds.NoticeRunSucceeded, Round: entry("r1", "nightly", rounds.NotifyFailures),
		Run: run(3, rounds.RunSucceeded, "", "s-open", "fine")}) // failures: no success row
	svc.Notice(rounds.Notice{Kind: rounds.NoticeRunFailed, Round: entry("r2", "quiet", rounds.NotifyNever),
		Run: run(4, rounds.RunFailed, "exit status 1", "s-open", "")})
	svc.Notice(rounds.Notice{Kind: rounds.NoticePaused, Round: entry("r2", "quiet", rounds.NotifyNever),
		Run: run(5, rounds.RunFailed, "exit status 1", "s-open", "")})
	svc.Notice(rounds.Notice{Kind: rounds.NoticeTargetGone, Round: entry("r3", "check-in", rounds.NotifyFailures),
		Run: run(6, rounds.RunSkipped, "the target session is gone", "s-gone", "")})
	pushes.notifier.Sync()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("%d notice.raised events, want 4: %+v", len(events), events)
	}
	wantKinds := []api.RoundNoticeKind{"run_failed", "run_failed", "paused", "target_gone"}
	for i, e := range events {
		if e.Round == nil || e.Round.Kind != wantKinds[i] || e.Round.Run == "" {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if b := events[0].Body; !strings.Contains(b, "Run 1 failed: exit status 2") || !strings.Contains(b, "the build broke\nat step 3") {
		t.Errorf("kept body with the result: %q", b)
	}
	if b := events[1].Body; strings.Contains(b, "private") || !strings.Contains(b, "timed out") {
		t.Errorf("a denied folder kept the result: %q", b)
	}
	if n, ok := pushes.notices.get(events[0].ID); !ok || n.Round == nil || n.Round.Round != "r1" || n.Title != "Round failed: nightly" {
		t.Errorf("notice.get: %v %+v", ok, n)
	}
	al := sent.alerts()
	if len(al) != 4 {
		t.Fatalf("%d pushes, want 4 (the phone only): %v", len(al), al)
	}
	wantTitles := []string{"Round failed: nightly", "Round timed out: nightly", "Round paused: quiet", "Round completed: check-in"}
	wantCollapse := []string{"round-r1", "round-r1", "round-r2", "round-r3"}
	for i, a := range al {
		fd := a["flightdeck"].(map[string]any)
		if title(a) != wantTitles[i] || a["collapse"] != wantCollapse[i] || fd["kind"] != push.KindRound || fd["round"] == nil || fd["notice"] != events[i].ID {
			t.Errorf("push %d: %v", i, a)
		}
	}
	if cat := al[0]["aps"].(map[string]any)["category"]; cat != push.CategoryOpen {
		t.Errorf("a failed run's banner opens its session: %v", cat)
	}
}
