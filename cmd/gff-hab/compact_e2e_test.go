package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeCompactClaude stands in for Claude Code in a pane (harness-adapters.md, Self-compact and
// the context nudge). It runs the real hook and status-line commands with hand-written payloads,
// as Claude's settings would, and calls `gff-hab agent compact` mid-turn as an agent would.
const fakeCompactClaude = `
stty raw -echo
"$HAB" hook claude SessionStart < "$DIR/start.json"
"$HAB" hook claude UserPromptSubmit < "$DIR/ups.json"
"$HAB" statusline claude < "$DIR/status.json" > /dev/null
"$HAB" hook claude PostToolUse < "$DIR/post.json" > "$DIR/nudge"
"$HAB" hook claude PostToolUse < "$DIR/post.json" > "$DIR/nudge-again"
"$HAB" agent compact "$FOLLOW" > "$DIR/compact.out" 2>&1
"$HAB" agent compact again > "$DIR/compact2.out" 2>&1; echo $? > "$DIR/compact2.code"
touch "$DIR/called"
"$HAB" hook claude Stop < "$DIR/stop.json"
head -c "$LEN1" > "$DIR/got1.tmp" && mv "$DIR/got1.tmp" "$DIR/got1"
"$HAB" hook claude PreCompact < "$DIR/pre.json"
while [ ! -e "$DIR/go-end" ]; do sleep 0.02; done
printf '%s\n' '{"type":"system","subtype":"compact_boundary","uuid":"b1"}' >> "$DIR/t.jsonl"
"$HAB" hook claude SessionStart < "$DIR/start-compact.json"
head -c "$LEN2" > "$DIR/got2.tmp" && mv "$DIR/got2.tmp" "$DIR/got2"
"$HAB" hook claude UserPromptSubmit < "$DIR/ups.json"
"$HAB" hook claude PostToolUse < "$DIR/post.json" > "$DIR/nudge-after"
touch "$DIR/finished"
while [ ! -e "$DIR/end" ]; do sleep 0.02; done
`

// TestCompactClaude: self-compact and the context nudge end to end with the real holder and
// gff-hab and a fake Claude Code. The nudge comes once per level through the PostToolUse hook; the
// agent's `gff-hab agent compact` answers at once, a second call is refused; /compact goes in
// only after the Stop hook, as one paste and Enter; PreCompact moves it to compacting; the
// follow-up goes in only after SessionStart compact; each step is an agent.compact event; after
// the compaction the nudge has no reading and stays quiet.
func TestCompactClaude(t *testing.T) {
	e := newHabEnv(t, 0)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()

	// The nudge: first at 50%, then every 10 points.
	var got api.SettingsResult
	ids := []string{"agent.context_nudge", "agent.context_nudge_text"}
	if err := c.Call(ctx, "settings.get", api.SettingsGetParams{IDs: ids}, &got); err != nil || len(got.Settings) != 2 {
		t.Fatalf("settings.get: %v %+v", err, got)
	}
	if err := c.Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: ids[0], Value: json.RawMessage(`50`), Revision: got.Settings[0].Revision},
		{ID: ids[1], Value: json.RawMessage(`"Context at {percent}%: hand off and compact."`), Revision: got.Settings[1].Revision},
	}}, nil); err != nil {
		t.Fatalf("settings.set: %v", err)
	}

	// Events from a second client, subscribed to the host.
	ev := e.control()
	if _, err := (api.Client{Caller: ev}).EventsSubscribe(ctx, api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var steps []api.Compaction
	go func() {
		for n := range ev.Notifications() {
			if n.Method != "event" {
				continue
			}
			x, err := n.Event()
			if err != nil || x.Type != api.EventAgentCompact {
				continue
			}
			var cp api.Compaction
			json.Unmarshal(x.Data, &cp)
			mu.Lock()
			steps = append(steps, cp)
			mu.Unlock()
		}
	}()
	states := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, s := range steps {
			out = append(out, s.State)
		}
		return out
	}

	dir := e.work
	tpath := filepath.Join(dir, "t.jsonl")
	writeFile(t, tpath, `{"type":"user","message":{"content":"hi"}}`+"\n")
	base := `"session_id":"sess-c","cwd":"/work/app","transcript_path":"` + tpath + `"`
	writeFile(t, filepath.Join(dir, "start.json"), `{`+base+`,"hook_event_name":"SessionStart","source":"startup"}`)
	writeFile(t, filepath.Join(dir, "start-compact.json"), `{`+base+`,"hook_event_name":"SessionStart","source":"compact"}`)
	writeFile(t, filepath.Join(dir, "ups.json"), `{`+base+`,"hook_event_name":"UserPromptSubmit","prompt":"work"}`)
	writeFile(t, filepath.Join(dir, "post.json"), `{`+base+`,"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"toolu_1"}`)
	writeFile(t, filepath.Join(dir, "stop.json"), `{`+base+`,"hook_event_name":"Stop"}`)
	writeFile(t, filepath.Join(dir, "pre.json"), `{`+base+`,"hook_event_name":"PreCompact","trigger":"manual","custom_instructions":""}`)
	writeFile(t, filepath.Join(dir, "status.json"), `{`+base+`,"hook_event_name":"Status","version":"2.1.316","context_window":{"context_window_size":200000,"used_percentage":63}}`)

	follow := "read handoff.md and continue"
	want1 := "\x1b[200~/compact\x1b[201~\r"
	want2 := "\x1b[200~" + follow + "\x1b[201~\r"
	var ss api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Name: "agent", Argv: []string{"/bin/sh", "-c", fakeCompactClaude}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + dir, "HAB=" + bin, "FOLLOW=" + follow,
			"LEN1=" + strconv.Itoa(len(want1)), "LEN2=" + strconv.Itoa(len(want2)), noRaceExitSleep}}, &ss); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, "the compact call", func() bool { _, err := os.Stat(filepath.Join(dir, "called")); return err == nil })
	// The nudge: once at the 60 level, then nothing for the same level.
	var nudge struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "nudge"))
	if err := json.Unmarshal(b, &nudge); err != nil || nudge.HookSpecificOutput.HookEventName != "PostToolUse" ||
		nudge.HookSpecificOutput.AdditionalContext != "Context at 63%: hand off and compact." {
		t.Fatalf("nudge %q: %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "nudge-again")); len(b) != 0 {
		t.Fatalf("a second nudge for the same level: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compact.out")); !strings.Contains(string(b), "queued") || !strings.Contains(string(b), "End your turn") {
		t.Fatalf("compact output %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compact2.code")); strings.TrimSpace(string(b)) != "3" {
		out, _ := os.ReadFile(filepath.Join(dir, "compact2.out"))
		t.Fatalf("a second compact: exit %q, %q", b, out)
	}

	waitUntil(t, "/compact in the pane", fileIs(filepath.Join(dir, "got1"), want1))
	waitUntil(t, "compacting", func() bool { s := states(); return len(s) > 0 && s[len(s)-1] == compactCompacting })
	if _, err := os.Stat(filepath.Join(dir, "got2")); err == nil {
		t.Fatal("the follow-up went in before the compaction ended")
	}
	writeFile(t, filepath.Join(dir, "go-end"), "")
	waitUntil(t, "the follow-up in the pane", fileIs(filepath.Join(dir, "got2"), want2))
	waitUntil(t, "done", func() bool { s := states(); return len(s) > 0 && s[len(s)-1] == compactDone })
	if s := strings.Join(states(), ","); s != "queued,sent,compacting,done" {
		t.Fatalf("steps %s", s)
	}
	mu.Lock()
	last := steps[len(steps)-1]
	mu.Unlock()
	if last.Session != ss.Session || last.Pane != ss.Pane || last.Harness != "claude" || !last.FollowUp {
		t.Fatalf("event %+v", last)
	}
	waitUntil(t, "the last hook", func() bool { _, err := os.Stat(filepath.Join(dir, "finished")); return err == nil })
	if b, _ := os.ReadFile(filepath.Join(dir, "nudge-after")); len(b) != 0 {
		t.Fatalf("a nudge right after the compaction: %q", b)
	}
	writeFile(t, filepath.Join(dir, "end"), "")
}
