package link

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestStatusReports: the link answers a program's OSC 7501 query once, and hands each status
// report in the pane's output to the watchers as a live PaneReport (harness-adapters.md, Claude
// Code).
func TestStatusReports(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	l := openLink(t, Config{HolderDir: hdir, Rescan: 50 * time.Millisecond})
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	var mu sync.Mutex
	var reports []Notice
	stop := l.Watch(func(n Notice) {
		if n.Kind == PaneReport {
			mu.Lock()
			reports = append(reports, n)
			mu.Unlock()
		}
	})
	defer stop()
	reply := "\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\"
	u := uuidN(82)
	spawn(t, l, u, `stty raw -echo
printf '\033]7501;?\033\\'
a=$(dd bs=1 count=`+strconv.Itoa(len(reply))+` 2>/dev/null | tr -d '\033')
case "$a" in *';?:states=idle,working,done,blocked,error:kinds=permission,question,auth\') printf 'reply:ok\r\n';; esac
printf '\033]7501;state=working:app=claude\033\\'
printf '\033]7501;state=blocked:kind=permission:app=claude\007'
sleep 30`)
	waitFor(t, "the reply and two reports", func() bool {
		mu.Lock()
		n := len(reports)
		mu.Unlock()
		return screenHas(t, l, u, "reply:ok") && n == 2
	})
	mu.Lock()
	defer mu.Unlock()
	r0, r1 := reports[0], reports[1]
	if r0.Pane.UUID != u || !r0.Live || r0.Report.State != "working" || r0.Report.App != "claude" ||
		r1.Report.State != "blocked" || r1.Report.Kind != "permission" {
		t.Fatalf("reports %+v", reports)
	}
}

// TestReplayedReports: after a restart, the link hands on only the last status report in the
// replayed bytes, marked not live, and never the earlier ones (harness-adapters.md, Edge cases:
// gff-hab restarts).
func TestReplayedReports(t *testing.T) {
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	cfg := Config{HolderDir: hdir, Rescan: 50 * time.Millisecond}
	l := openLink(t, cfg)
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(83)
	spawn(t, l, u, `printf '\033]7501;state=working:app=claude\033\\'
printf '\033]7501;state=blocked:kind=permission:app=claude\033\\'
printf '\033]7501;state=idle:app=claude\033\\'
printf 'reports:sent\r\n'
exec sleep 30`)
	waitFor(t, "the reports", func() bool { return screenHas(t, l, u, "reports:sent") })
	l.Close()

	// A new state directory holds no checkpoint, so the whole ring replays.
	var mu sync.Mutex
	var got []Notice
	l = openLink(t, cfg)
	stop := l.Watch(func(n Notice) {
		if n.Kind == PaneReport {
			mu.Lock()
			got = append(got, n)
			mu.Unlock()
		}
	})
	defer stop()
	waitFor(t, "the pane", func() bool { _, ok := paneStatus(l, u); return ok })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Ready(ctx); err != nil { // the replay ended
		t.Fatal(err)
	}
	rs := l.ReplayedReports()
	if len(rs) != 1 || rs[0].Pane.UUID != u || rs[0].Live || rs[0].Report.State != "idle" {
		t.Fatalf("replayed reports %+v", rs)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range got {
		if n.Live || n.Report.State != "idle" {
			t.Fatalf("a watcher got %+v", n)
		}
	}
}

// restartedReports runs script in a pane, waits for "script:done" on its screen, closes the
// link (a planned stop writes the checkpoint), opens a new one on the same state directory, and
// returns the replayed reports once the replay ended. The first link's notices go to first.
func restartedReports(t *testing.T, n int, script string, first func(Notice)) []Notice {
	t.Helper()
	hdir := shortDir(t)
	startHolder(t, hdir, "one", 0)
	cfg := Config{HolderDir: hdir, StateDir: shortDir(t), Rescan: 50 * time.Millisecond}
	l := openLink(t, cfg)
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	if first != nil {
		stop := l.Watch(first)
		defer stop()
	}
	u := uuidN(n)
	spawn(t, l, u, script)
	waitFor(t, "the script", func() bool { return screenHas(t, l, u, "script:done") })
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckpoint(cfg.StateDir, u); err != nil {
		t.Fatalf("no checkpoint after a planned stop: %v", err)
	}
	l = openLink(t, cfg)
	waitFor(t, "the pane", func() bool { _, ok := paneStatus(l, u); return ok })
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return l.ReplayedReports()
}

// TestCheckpointKeepsReport: a restart replays from the checkpoint, past the bytes that held the
// last status report. The checkpoint keeps that report, so the new link still hands it on. A
// report with an id (below the program) is not the pane's.
func TestCheckpointKeepsReport(t *testing.T) {
	rs := restartedReports(t, 84, `printf '\033]7501;state=working:app=claude\033\\'
printf '\033]7501;state=idle:app=claude\033\\'
printf '\033]7501;state=working:id=sub:app=claude\033\\'
printf 'script:done\r\n'
exec sleep 30`, nil)
	if len(rs) != 1 || rs[0].Live || rs[0].Report.State != "idle" || rs[0].Report.ID != "" {
		t.Fatalf("replayed reports after a checkpoint restart: %+v", rs)
	}
}

// TestPromptAfterReport: a shell prompt (OSC 133 A) after the last status report reaches the
// watchers as a live PanePrompt, and the report is not handed on after a restart.
func TestPromptAfterReport(t *testing.T) {
	var mu sync.Mutex
	var kinds []NoticeKind
	rs := restartedReports(t, 85, `printf '\033]7501;state=working:app=claude\033\\'
sleep 0.2
printf '\033]133;A\007$ \033]133;B\007'
printf 'script:done\r\n'
exec sleep 30`, func(n Notice) {
		if n.Kind == PaneReport || n.Kind == PanePrompt {
			mu.Lock()
			kinds = append(kinds, n.Kind)
			mu.Unlock()
		}
	})
	mu.Lock()
	got := append([]NoticeKind(nil), kinds...)
	mu.Unlock()
	if len(got) != 2 || got[0] != PaneReport || got[1] != PanePrompt {
		t.Fatalf("live notices %v, want a report then a prompt", got)
	}
	if len(rs) != 0 {
		t.Fatalf("a report from before the prompt was handed on: %+v", rs)
	}
}

// TestPromptThenCommand: a prompt that a command followed in the same output started a program:
// no PanePrompt, and the program's report stays.
func TestPromptThenCommand(t *testing.T) {
	var mu sync.Mutex
	prompts := 0
	rs := restartedReports(t, 86, `printf '\033]133;A\007$ \033]133;B\007claude\033]133;C\007'
printf '\033]7501;state=idle:app=claude\033\\'
printf 'script:done\r\n'
exec sleep 30`, func(n Notice) {
		if n.Kind == PanePrompt {
			mu.Lock()
			prompts++
			mu.Unlock()
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if prompts != 0 || len(rs) != 1 || rs[0].Report.State != "idle" {
		t.Fatalf("prompts %d, replayed %+v", prompts, rs)
	}
}
