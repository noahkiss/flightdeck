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
