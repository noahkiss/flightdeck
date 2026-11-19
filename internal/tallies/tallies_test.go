package tallies

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSurface() Surface {
	return Surface{
		Methods:  []string{"hab.hello", "session.send", "session.create", "diag.friction", "structure.get"},
		Events:   []string{"session.updated", "pane.updated"},
		Hooks:    []HookSet{{Harness: "claude", Events: []string{"Stop", "SessionStart"}}},
		Settings: []string{"heavy.slots", "group.max_leads"},
		Clients:  []string{"app", "cli"},
		Errors:   []string{"invalid", "not_found", ErrorProtocol},
		Procs:    []string{"hab", "holder"},
	}
}

func newTest(t *testing.T, now *time.Time) *Tallies {
	t.Helper()
	return New(Config{Dir: filepath.Join(t.TempDir(), DirName), Surface: testSurface(),
		Now: func() time.Time { return *now }, InConfig: func() []string { return []string{"group.max_leads", "no.such"} }})
}

// The counter path allocates nothing (diagnostics.md 14, stage 3).
func TestCountsAllocateNothing(t *testing.T) {
	now := time.Now()
	tl := newTest(t, &now)
	method, client, kind := strings.Clone("session.send"), strings.Clone("app"), strings.Clone("invalid")
	unknown := strings.Clone("a user's text, not a method")
	n := testing.AllocsPerRun(1000, func() {
		tl.Call(method, client, "", false)
		tl.Call(method, client, kind, true)
		tl.Call(unknown, unknown, unknown, true)
		tl.Event("pane.updated")
		tl.Event(unknown)
		tl.Hook("claude", "Stop", false)
		tl.Hook(unknown, unknown, true)
		tl.Feature(FeatureRoundRun)
		tl.SettingSet("heavy.slots")
		tl.SettingSet(unknown)
		tl.Health("hab", KindStart)
	})
	if n != 0 {
		t.Fatalf("counting allocates %v times per run", n)
	}
	var off *Tallies
	if n := testing.AllocsPerRun(100, func() { off.Call(method, client, "", false) }); n != 0 {
		t.Fatalf("a nil Tallies allocates %v", n)
	}
}

func BenchmarkCall(b *testing.B) {
	tl := New(Config{Dir: b.TempDir(), Surface: Current()})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tl.Call("session.send", "app", "", false)
		}
	})
}

func BenchmarkCallSerial(b *testing.B) {
	tl := New(Config{Dir: b.TempDir(), Surface: Current()})
	b.ReportAllocs()
	for b.Loop() {
		tl.Call("session.send", "app", "", false)
	}
}

// A key outside the surface is counted under other and never stored by name.
func TestClosedKeySet(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	tl := newTest(t, &now)
	secret := "a private project name"
	tl.Call(secret, "app", "", false)
	tl.Call("session.send", secret, secret, true)
	tl.Event(secret)
	tl.Hook("claude", secret, false)
	tl.Hook(secret, "Stop", true)
	tl.SettingSet(secret)
	tl.Health(secret, KindCrash)
	tl.Health("hab", secret) // an unknown kind is not counted at all
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	var all []byte
	es, _ := os.ReadDir(tl.dir)
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(tl.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if strings.Contains(string(all), "private") {
		t.Fatalf("a key outside the surface was stored by name:\n%s", all)
	}
	d, err := readDay(filepath.Join(tl.dir, "2026-11-19.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Calls[Other]["app"].N != 1 || d.Calls["session.send"][Other].Errors[Other] != 1 {
		t.Fatalf("calls: %+v", d.Calls)
	}
	if d.Events[Other] != 1 || d.Hooks[Other].N != 1 || d.Hooks[Other].Refused != 1 {
		t.Fatalf("events %v hooks %+v", d.Events, d.Hooks[Other])
	}
	if d.Settings[Other].Set != 1 || d.Health[Other].Crashes != 1 || d.Health["hab"] != nil {
		t.Fatalf("settings %+v health %+v", d.Settings, d.Health)
	}
	if d.Settings["no.such"] != nil || !d.Settings["group.max_leads"].InConfig {
		t.Fatalf("in_config: %+v", d.Settings)
	}
	seen, err := ReadSeen(tl.dir)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Methods[Other] != nil || seen.Methods["session.send"] == nil {
		t.Fatalf("seen methods: %+v", seen.Methods)
	}
}

func TestFlushDaysAndSeen(t *testing.T) {
	now := time.Date(2026, 11, 18, 23, 0, 0, 0, time.Local)
	tl := newTest(t, &now)
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tl.dir); err == nil {
		t.Fatal("a flush with no counts wrote files")
	}
	tl.Call("session.send", "app", "", false)
	tl.Call("session.send", "app", "", false)
	tl.Call("session.send", "cli", "not_found", true)
	tl.Hook("claude", "Stop", false)
	tl.Health("hab", KindStart)
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	tl.Call("session.send", "app", "", false)
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // the next day
	tl.Call("structure.get", "app", "", false)
	tl.Close()
	tl.Close()

	d, err := readDay(filepath.Join(tl.dir, "2026-11-18.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Calls["session.send"]["app"]; c.N != 3 || len(c.Errors) != 0 {
		t.Fatalf("app calls %+v", c)
	}
	if c := d.Calls["session.send"]["cli"]; c.N != 1 || c.Errors["not_found"] != 1 {
		t.Fatalf("cli calls %+v", c)
	}
	if d.Features["send"] != 3 {
		t.Fatalf("send feature %d, want 3: a failed call counts no feature", d.Features["send"])
	}
	if d.Health["hab"].Starts != 1 {
		t.Fatalf("health %+v", d.Health)
	}
	seen, _ := ReadSeen(tl.dir)
	if seen.Since != "2026-11-18" {
		t.Fatalf("since %q", seen.Since)
	}
	if s := seen.Methods["session.send"]; s.First != "2026-11-18" || s.Last != "2026-11-18" || s.Total != 4 {
		t.Fatalf("span %+v", s)
	}
	if s := seen.Methods["structure.get"]; s.First != "2026-11-19" {
		t.Fatalf("span %+v", s)
	}
	if s := seen.Settings["group.max_leads"]; s == nil || s.Last != "2026-11-19" {
		t.Fatalf("a setting in config is in use: %+v", seen.Settings)
	}

	w, err := ReadWindow(tl.dir, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if w.Files != 2 || w.Sum.Calls["session.send"]["app"].N != 3 || w.Sum.Calls["structure.get"]["app"].N != 1 {
		t.Fatalf("window %+v", w)
	}
	if w1, _ := ReadWindow(tl.dir, now, 1); w1.Files != 1 || w1.Sum.Calls["session.send"] != nil {
		t.Fatalf("one-day window %+v", w1)
	}

	us := FindUnused(tl.surface, seen, now, 1)
	got := map[string]string{}
	for _, u := range us {
		got[u.Kind+" "+u.Name] = u.Last
	}
	if last, ok := got["method session.send"]; !ok || last != "2026-11-18" {
		t.Fatalf("session.send unused today, last 2026-11-18: %v", got)
	}
	if _, ok := got["method structure.get"]; ok {
		t.Fatal("structure.get was used today")
	}
	if last, ok := got["method hab.hello"]; !ok || last != "" {
		t.Fatalf("hab.hello never used: %v", got)
	}
	if _, ok := got["setting group.max_leads"]; ok {
		t.Fatal("a setting in config is not unused")
	}
	if _, ok := got["hook claude/SessionStart"]; !ok {
		t.Fatal("hook events list too")
	}
	if _, ok := got["feature capcom.job"]; !ok {
		t.Fatal("features list too")
	}
	// A name that left the surface is never listed, though seen.json keeps it.
	smaller := tl.surface
	smaller.Methods = slices.DeleteFunc(slices.Clone(smaller.Methods), func(m string) bool { return m == "session.send" })
	for _, u := range FindUnused(smaller, seen, now, 1) {
		if u.Name == "session.send" {
			t.Fatal("a method gone from the surface is listed as unused")
		}
	}
}

// A failed write keeps its counts for the next flush.
func TestFlushFailureKeepsCounts(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	tl := newTest(t, &now)
	if err := os.WriteFile(tl.dir, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl.Call("session.send", "app", "", false)
	if err := tl.Flush(); err == nil {
		t.Fatal("flush into a file, not a folder, worked")
	}
	os.Remove(tl.dir)
	tl.Call("session.send", "app", "", false)
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	d, _ := readDay(filepath.Join(tl.dir, "2026-11-19.json"))
	if d.Calls["session.send"]["app"].N != 2 {
		t.Fatalf("calls %+v", d.Calls)
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	dir := t.TempDir()
	old := now.AddDate(0, 0, -KeepDays-1).Format(dayLayout) + ".json"
	kept := now.AddDate(0, 0, -KeepDays).Format(dayLayout) + ".json"
	for _, n := range []string{old, kept, SeenFile} {
		os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600)
	}
	Prune(dir, now)
	for n, want := range map[string]bool{old: false, kept: true, SeenFile: true} {
		_, err := os.Stat(filepath.Join(dir, n))
		if (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", n, err == nil, want)
		}
	}
}

// Adds that race a flush are never lost, and a reader never sees half a file.
func TestConcurrentCountsAndReads(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local)
	tl := newTest(t, &now)
	const workers, each = 8, 2000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(readErr)
				return
			default:
			}
			for _, n := range []string{"2026-11-19.json", SeenFile} {
				b, err := os.ReadFile(filepath.Join(tl.dir, n))
				if err == nil && !json.Valid(b) {
					readErr <- &os.PathError{Op: "read", Path: n, Err: os.ErrInvalid}
					close(readErr)
					return
				}
			}
		}
	}()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				tl.Call("session.send", "app", "", false)
				if i%500 == 0 {
					tl.Flush()
				}
			}
		}()
	}
	wg.Wait()
	tl.Close()
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	d, err := readDay(filepath.Join(tl.dir, "2026-11-19.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := d.Calls["session.send"]["app"].N; n != workers*each {
		t.Fatalf("calls %d, want %d", n, workers*each)
	}
}

// The surface comes from the method table, event types, hook config and settings registry.
func TestCurrentSurface(t *testing.T) {
	s := Current()
	for _, want := range []string{"hab.hello", "rpc.discover", "session.send", "diag.friction"} {
		if !slices.Contains(s.Methods, want) {
			t.Errorf("method %s missing", want)
		}
	}
	if !slices.Contains(s.Events, "session.updated") || !slices.Contains(s.Settings, "heavy.slots") {
		t.Error("events or settings missing")
	}
	if !slices.Contains(s.HookKeys(), "claude/Stop") || !slices.Contains(s.HookKeys(), "codex/Interrupt") {
		t.Errorf("hooks %v", s.HookKeys())
	}
	if !slices.Contains(s.Clients, "app") || !slices.Contains(s.Errors, ErrorProtocol) {
		t.Error("clients or errors missing")
	}
	for name := range methodFeatures {
		if !slices.Contains(s.Methods, name) {
			t.Errorf("feature method %s is not in the method table", name)
		}
	}
}
