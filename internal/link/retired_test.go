package link

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// TestRetiredFile: the retired file round-trips, drops entries past the TTL on load and on save,
// and an unusable file counts as empty and asks for a rewrite.
func TestRetiredFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ttl := time.Hour
	set, pruned, err := loadRetired(dir, ttl, now)
	if err != nil || pruned || len(set) != 0 {
		t.Fatalf("no file: %v, pruned %v, %d entries", err, pruned, len(set))
	}
	fresh, old := uuidN(80), uuidN(81)
	in := map[hp.UUID]time.Time{fresh: now.Add(-time.Minute), old: now.Add(-2 * time.Hour)}
	if err := saveRetired(dir, in, 3*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	set, pruned, err = loadRetired(dir, ttl, now)
	if err != nil || !pruned {
		t.Fatalf("load: %v, pruned %v; want the old entry pruned", err, pruned)
	}
	if _, ok := set[old]; ok || len(set) != 1 || !set[fresh].Equal(in[fresh]) {
		t.Fatalf("load kept %v", set)
	}
	if err := saveRetired(dir, in, ttl, now); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, retiredFile))
	if strings.Contains(string(b), old.String()) || !strings.Contains(string(b), fresh.String()) {
		t.Fatalf("save did not drop the expired entry:\n%s", b)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("%d files in the state directory, want only %s", len(ents), retiredFile)
	}
	for _, bad := range []string{"{", `{"v":9,"retired":{}}`} {
		os.WriteFile(filepath.Join(dir, retiredFile), []byte(bad), 0o600)
		set, pruned, err = loadRetired(dir, ttl, now)
		if err == nil || !pruned || len(set) != 0 {
			t.Errorf("file %q: %v, pruned %v, %d entries", bad, err, pruned, len(set))
		}
	}
}

// TestRetiredAcrossRestart: a pane closed by a client is retired at once, and a restarted link
// still refuses its uuid. Once the TTL has passed, a restarted link forgets it.
func TestRetiredAcrossRestart(t *testing.T) {
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "keep", 0)
	cfg := Config{HolderDir: hdir, StateDir: state, Rescan: time.Hour}
	l := openLink(t, cfg)
	waitFor(t, "a current holder", func() bool { return l.client.Current() != nil })
	u := uuidN(82)
	spawn(t, l, u, "echo closing; exec sleep 1000")
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()
	if err := l.ClosePane(ctx, u); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(state, retiredFile))
	if err != nil || !strings.Contains(string(b), u.String()) {
		t.Fatalf("retired file after the close: %v\n%s", err, b)
	}
	l.Close()

	l = openLink(t, cfg)
	waitFor(t, "a current holder after the restart", func() bool { return l.client.Current() != nil })
	if _, ok := paneStatus(l, u); ok {
		t.Fatal("the restarted link lists the closed pane")
	}
	_, err = l.Spawn(ctx, hp.Spawn{UUID: u, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", "echo again; exec sleep 1000"}, Env: testEnv})
	if !errors.Is(err, ErrUUIDUsed) {
		t.Fatalf("spawn of a retired uuid after a restart: %v, want ErrUUIDUsed", err)
	}
	spawn(t, l, uuidN(83), "exec sleep 1000")
	l.Close()

	time.Sleep(20 * time.Millisecond)
	cfg.RetiredTTL = 10 * time.Millisecond
	l = openLink(t, cfg)
	waitFor(t, "a current holder after the second restart", func() bool { return l.client.Current() != nil })
	if !l.spawnable(u, l.client.Current()) {
		t.Error("a uuid retired longer than the TTL ago is still refused")
	}
	if b, _ := os.ReadFile(filepath.Join(state, retiredFile)); strings.Contains(string(b), u.String()) {
		t.Errorf("the expired uuid is still in the file:\n%s", b)
	}
}
