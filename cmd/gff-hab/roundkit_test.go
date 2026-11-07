package main

// Rounds in the agent kit (rounds.md 12 Q1, 13, stage 10), end to end with the real holder and
// gff-hab: an agent makes a round on its own hab through the MCP tools with no card, and removes
// it by $GFF_ROUND through `gff round`; a round an agent makes on another host goes through
// confirm.start_elsewhere: off, it is made at once with a record; on, it waits for the card, a
// declined card makes nothing and a confirmed one makes it.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

var (
	gffOnce sync.Once
	gffBin  string
	gffErr  error
)

// buildGff builds the gff client beside gff-hab, once, so `gff round` finds it there.
func buildGff(t *testing.T) string {
	t.Helper()
	gffOnce.Do(func() {
		p := filepath.Join(testDir, "gff")
		out, err := exec.Command("go", "build", "-o", p, "github.com/noahkiss/flightdeck/cmd/gff").CombinedOutput()
		if err != nil {
			gffErr = err
			gffBin = string(out)
			return
		}
		gffBin = p
	})
	if gffErr != nil {
		t.Fatalf("build gff: %v\n%s", gffErr, gffBin)
	}
	return gffBin
}

// mcpReq is one tools/call line.
func mcpReq(t *testing.T, id int, tool string, args map[string]any) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	return string(line) + "\n"
}

type toolAnswer struct {
	text    string
	isError bool
}

// toolAnswers reads the server's answers by id.
func toolAnswers(t *testing.T, out string) map[int]toolAnswer {
	t.Helper()
	got := map[int]toolAnswer{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m struct {
			ID     int `json:"id"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("mcp line %q: %v", l, err)
		}
		a := toolAnswer{isError: m.Result.IsError}
		for _, c := range m.Result.Content {
			a.text += c.Text
		}
		got[m.ID] = a
	}
	return got
}

// agentPane starts a session named name in dir that runs script with HAB, GFF and the extra
// variables set, and returns its pane.
func agentPane(t *testing.T, sock, name, dir, script string, env ...string) api.SessionCreated {
	t.Helper()
	return createSession(t, sock, api.SessionCreateParams{Name: name, Argv: []string{"/bin/sh", "-c", script}, Cwd: dir,
		Env: append([]string{"HAB=" + bin, "PATH=/usr/bin:/bin"}, env...)})
}

// readWhenThere waits for a file the pane writes by rename, and returns it.
func readWhenThere(t *testing.T, path string) string {
	t.Helper()
	waitUntil(t, filepath.Base(path), func() bool { _, err := os.Stat(path); return err == nil })
	b, _ := os.ReadFile(path)
	return string(b)
}

func confirmations(t *testing.T, c *control.Client) []api.Confirmation {
	t.Helper()
	var l api.ConfirmList
	call(t, c, "confirm.list", api.ConfirmListParams{All: true}, &l)
	return l.Confirmations
}

// TestRoundAgentKit: from a session's pane, the MCP round tools make a resume round on the
// session itself and a fresh command round, at once and with no card; the session is their
// creator; another's round is refused; `gff round rm "$GFF_ROUND"` (the round's id) removes it.
func TestRoundAgentKit(t *testing.T) {
	e, repo := newRoundEnv(t)
	gff := buildGff(t)
	c := e.control()
	mustRound(t, e, "add", "users", "-manual", "-project", repo, "-command", "--", "true")

	req1 := filepath.Join(e.work, "req1")
	req2 := filepath.Join(e.work, "req2")
	writeFile(t, req1, mcpReq(t, 1, "round_add", map[string]any{"name": "selfcheck", "mode": "resume", "target": "self", "manual": true, "prompt": "look again"})+
		mcpReq(t, 2, "round_add", map[string]any{"name": "hourly", "every": "1h", "tz": "UTC", "command": []string{"true"}, "project": repo})+
		mcpReq(t, 3, "round_pause", map[string]any{"name": "users"}))
	writeFile(t, req2, mcpReq(t, 1, "round_ls", map[string]any{})+mcpReq(t, 2, "round_show", map[string]any{"name": "selfcheck"}))
	out1, out2, out3, idf := filepath.Join(e.work, "out1"), filepath.Join(e.work, "out2"), filepath.Join(e.work, "out3"), filepath.Join(e.work, "idf")
	script := `"$HAB" mcp < "$REQ1" > "$OUT1.tmp" 2>&1; mv "$OUT1.tmp" "$OUT1"
"$HAB" mcp < "$REQ2" > "$OUT2.tmp" 2>&1; mv "$OUT2.tmp" "$OUT2"
while [ ! -e "$IDF" ]; do sleep 0.1; done
GFF_ROUND=$(cat "$IDF"); export GFF_ROUND
"$GFF" round rm -yes "$GFF_ROUND" > "$OUT3.tmp" 2>&1; echo "exit $?" >> "$OUT3.tmp"; mv "$OUT3.tmp" "$OUT3"
exec sleep 60`
	made := agentPane(t, e.sock, "agent", repo, script, "GFF="+gff, "REQ1="+req1, "REQ2="+req2,
		"OUT1="+out1, "OUT2="+out2, "OUT3="+out3, "IDF="+idf)
	defer call(t, c, "pane.close", api.PaneRef{Pane: made.Pane}, nil)

	a := toolAnswers(t, readWhenThere(t, out1))
	if a[1].isError || !strings.Contains(a[1].text, "Made round selfcheck") || !strings.Contains(a[1].text, "no confirmation asked") {
		t.Errorf("round_add self: %+v", a[1])
	}
	if a[2].isError || !strings.Contains(a[2].text, "Made round hourly") {
		t.Errorf("round_add fresh: %+v", a[2])
	}
	if !a[3].isError || !strings.Contains(a[3].text, "only rounds this session created") {
		t.Errorf("round_pause of another's round: %+v", a[3])
	}
	self := getRound(t, c, "selfcheck")
	if self.Target != made.Session || self.CreatedBy != made.Session || self.Mode != api.RoundModeResume {
		t.Fatalf("selfcheck: %+v", self)
	}
	if h := getRound(t, c, "hourly"); h.CreatedBy != made.Session || len(h.Command) != 1 || h.Trigger.Zone != "UTC" {
		t.Errorf("hourly: %+v", h)
	}
	if l := confirmations(t, c); len(l) != 0 {
		t.Errorf("a round on the agent's own hab made a card: %+v", l)
	}
	if r := getRound(t, c, "users"); r.Paused {
		t.Error("another's round was paused")
	}
	a = toolAnswers(t, readWhenThere(t, out2))
	if !strings.Contains(a[1].text, "selfcheck") || !strings.Contains(a[1].text, "hourly") || !strings.Contains(a[1].text, "users") {
		t.Errorf("round_ls: %+v", a[1])
	}
	if !strings.Contains(a[2].text, "created by") || !strings.Contains(a[2].text, made.Session) {
		t.Errorf("round_show: %+v", a[2])
	}

	writeFile(t, idf, self.ID)
	if out := readWhenThere(t, out3); !strings.Contains(out, "deleted "+self.ID) || !strings.Contains(out, "exit 0") {
		t.Fatalf("gff round rm $GFF_ROUND: %s", out)
	}
	if hasRound(c, "selfcheck") {
		t.Fatal("selfcheck is still there")
	}
}

// TestRoundElsewhere: a round an agent makes on another host goes through
// confirm.start_elsewhere. Off: made at once on the runner, the card a record. On: the card
// waits, nothing is made, a cancel makes nothing; a confirm makes it; a round on the agent's own
// hab still makes no card. A pane's -host reaches no other verb.
func TestRoundElsewhere(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	a.start(t, standIn, "host-a")
	b.start(t, standIn, "host-b")
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	ca, cb := a.control(), b.control()
	waitUntil(t, "a's and b's catalogs have their projects", func() bool {
		ea, _ := hostEntry(catalogOf(t, ca), "host-a")
		eb, _ := hostEntry(catalogOf(t, ca), "host-b")
		return len(ea.Catalog.Projects) > 0 && len(eb.Catalog.Projects) > 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(90*time.Second))
	defer cancel()
	var panes []string
	defer func() {
		for _, p := range panes {
			ca.Call(context.Background(), "pane.close", api.PaneRef{Pane: p}, nil)
		}
	}()
	// step runs one command in a new session's pane on a, in a's checkout, and returns the
	// path its output lands at; the output ends with the exit status.
	n := 0
	step := func(cmd string, env ...string) string {
		n++
		out := filepath.Join(a.work, "step"+string(rune('0'+n)))
		s := agentPane(t, a.sock, "agent"+string(rune('0'+n)), a.repo,
			cmd+` > "$OUT.tmp" 2>&1; echo "exit $?" >> "$OUT.tmp"; mv "$OUT.tmp" "$OUT"; exec sleep 60`, append(env, "OUT="+out)...)
		panes = append(panes, s.Pane)
		return out
	}
	openRound := func(name string) api.Confirmation {
		var got api.Confirmation
		waitUntil(t, "the card for "+name, func() bool {
			for _, c := range confirmations(t, ca) {
				if c.Kind == "round" && c.State == "open" && c.Round != nil && c.Round.Round.Name == name {
					got = c
					return true
				}
			}
			return false
		})
		return got
	}

	// Off (the default): made at once on b, by this folder's remote; the card is a record.
	out := readWhenThere(t, step(`"$HAB" round add far -host bee -every 1h -tz UTC -command -- true`))
	if !strings.Contains(out, "Made round far") || !strings.Contains(out, "at once") || !strings.Contains(out, "exit 0") {
		t.Fatalf("add -host with the setting off: %s", out)
	}
	if !hasRound(cb, "far") || hasRound(ca, "far") {
		t.Fatal("far is not on b alone")
	}
	if r := getRound(t, cb, "far"); r.CreatedBy != "" || r.Project == "" {
		t.Errorf("far on b: %+v", r)
	}
	l := confirmations(t, ca)
	if len(l) != 1 || l[0].Kind != "round" || !l[0].Record || l[0].State != "done" || l[0].From != "agent1" ||
		l[0].Round.Trigger != "every 1h in UTC" || l[0].Result == nil || l[0].Result.Round == "" {
		t.Fatalf("the record: %+v", l)
	}
	// From a pane, -host reaches no verb but add.
	if out := readWhenThere(t, step(`"$HAB" round pause far -host bee`)); !strings.Contains(out, "not available from a pane") || strings.Contains(out, "exit 0") {
		t.Errorf("pause -host from a pane: %s", out)
	}
	if getRound(t, cb, "far").Paused {
		t.Error("far was paused from a pane")
	}

	// On: the card waits, and a cancel makes nothing.
	var set api.SettingsResult
	if err := ca.Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: "confirm.start_elsewhere", Value: json.RawMessage(`true`), Revision: 1}}}, &set); err != nil {
		t.Fatal(err)
	}
	req := filepath.Join(a.work, "req")
	writeFile(t, req, mcpReq(t, 1, "round_add", map[string]any{"name": "far2", "host": "bee", "every": "1h", "tz": "UTC", "command": []string{"true"}}))
	mcpOut := step(`"$HAB" mcp < "$REQ"`, "REQ="+req)
	cf := openRound("far2")
	if cf.Record || cf.From == "" || cf.Round.Host != "bee" || cf.Round.Project == "" || cf.ExpiresAt == "" {
		t.Errorf("the card: %+v", cf)
	}
	time.Sleep(300 * time.Millisecond)
	if hasRound(cb, "far2") {
		t.Fatal("far2 was made before the answer")
	}
	var res api.ConfirmationResult
	if err := ca.Call(ctx, "confirm.answer", api.ConfirmAnswerParams{ID: cf.ID, Choice: "cancel"}, &res); err != nil || res.Confirmation.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	ans := toolAnswers(t, strings.TrimSuffix(strings.TrimSpace(readWhenThere(t, mcpOut)), "exit 0"))
	if !ans[1].isError || !strings.Contains(ans[1].text, "cancelled") {
		t.Errorf("round_add after the cancel: %+v", ans[1])
	}
	if hasRound(cb, "far2") {
		t.Fatal("a cancelled card made far2")
	}

	// On: a confirm makes it.
	cliOut := step(`"$HAB" round add far3 -host bee -every 1h -tz UTC -command -- true`)
	cf = openRound("far3")
	if hasRound(cb, "far3") {
		t.Fatal("far3 was made before the answer")
	}
	if err := ca.Call(ctx, "confirm.answer", api.ConfirmAnswerParams{ID: cf.ID, Choice: "confirm"}, &res); err != nil || res.Confirmation.State != "done" {
		t.Fatalf("confirm: %+v %v", res, err)
	}
	if out := readWhenThere(t, cliOut); !strings.Contains(out, "Made round far3") || !strings.Contains(out, "after the owner confirmed") || !strings.Contains(out, "exit 0") {
		t.Errorf("add -host after the confirm: %s", out)
	}
	if !hasRound(cb, "far3") {
		t.Fatal("far3 is not on b")
	}

	// On: a round on the agent's own hab makes no card.
	before := len(confirmations(t, ca))
	if out := readWhenThere(t, step(`"$HAB" round add here -every 1h -tz UTC -command -- true`)); !strings.Contains(out, "exit 0") {
		t.Fatalf("add here: %s", out)
	}
	if !hasRound(ca, "here") || len(confirmations(t, ca)) != before {
		t.Errorf("a round here made a card: %+v", confirmations(t, ca))
	}
}
