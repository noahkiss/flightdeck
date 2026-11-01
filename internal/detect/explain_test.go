package detect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// TestExplainScreenMatchesScreen: the explanation's result is always Screen's, for every vendored
// manifest over every hand-written screen, and it lists every rule in file order with its region.
func TestExplainScreenMatchesScreen(t *testing.T) {
	inputs := []Input{
		{Screen: claudePermission}, {Screen: claudeIdle}, {Screen: codexTrust}, {Screen: opencodePermission},
		{Screen: opencodeWorking}, {Screen: "hello\n"}, {}, {OSCTitle: "⠂ Fixing the tests"}, {OSCTitle: "Action Required"},
	}
	for _, e := range Vendored().Entries() {
		m := e.Manifest
		for i, in := range inputs {
			tr := m.ExplainScreen(in)
			if want := m.Screen(in); tr.Result != want {
				t.Errorf("%s input %d: explain %+v, screen %+v", m.ID, i, tr.Result, want)
			}
			if len(tr.Rules) != len(m.Rules) {
				t.Fatalf("%s: %d rules traced, manifest has %d", m.ID, len(tr.Rules), len(m.Rules))
			}
			for j, r := range tr.Rules {
				if r.ID != m.Rules[j].ID || r.Region != m.Rules[j].Region || r.Text != region(in, r.Region) {
					t.Errorf("%s rule %d: traced %+v", m.ID, j, r)
				}
			}
			if (tr.Winner >= 0) != tr.Result.Matched || (tr.Winner >= 0 && tr.Rules[tr.Winner].ID != tr.Result.Rule) {
				t.Errorf("%s input %d: winner %d, result %+v", m.ID, i, tr.Winner, tr.Result)
			}
		}
	}
}

// TestExplainScreenShowsLosers: a rule that matched but lost to a higher priority shows as
// matched, and the region text is what the rule read.
func TestExplainScreenShowsLosers(t *testing.T) {
	m, err := Parse([]byte(`
id = "t"
version = "1"
[[rules]]
id = "low"
state = "idle"
priority = 1
region = "bottom_lines(1)"
contains = ["ready"]
[[rules]]
id = "high"
state = "working"
priority = 5
contains = ["busy"]
[[rules]]
id = "miss"
state = "blocked"
priority = 9
contains = ["allow?"]
`))
	if err != nil {
		t.Fatal(err)
	}
	tr := m.ExplainScreen(Input{Screen: "busy\nready"})
	got := []bool{tr.Rules[0].Matched, tr.Rules[1].Matched, tr.Rules[2].Matched}
	if !reflect.DeepEqual(got, []bool{true, true, false}) || tr.Result.Rule != "high" || tr.Winner != 1 {
		t.Fatalf("matched %v, result %+v, winner %d", got, tr.Result, tr.Winner)
	}
	if tr.Rules[0].Text != "ready" || tr.Rules[1].Text != "busy\nready" {
		t.Errorf("region texts %q, %q", tr.Rules[0].Text, tr.Rules[1].Text)
	}
}

// TestExplainProcessMatchesDetect: the explanation's detection is DetectProcess's, and each
// process shows the names it offered and what its first matching name resolves to.
func TestExplainProcessMatchesDetect(t *testing.T) {
	s := Vendored()
	fgs := []Foreground{
		{Leader: Process{PID: 40, Name: "bash"}, Members: []Process{{PID: 41, Name: "less"}, {PID: 42, Name: "codex"}, {PID: 43, Name: "claude"}}},
		{Leader: Process{PID: 21, Name: "node", Args: []string{"node", "--no-warnings", "/x/codex.js"}}},
		{Leader: Process{PID: 60, Name: "zsh", Args: []string{"-zsh"}}},
		{Members: []Process{{PID: 70, Name: "opencode"}}}, // the leader has exited
	}
	for i, fg := range fgs {
		trace, det, err := s.ExplainProcess(fg)
		wd, werr := s.DetectProcess(fg)
		if det != wd || err != werr {
			t.Errorf("fg %d: explain %+v %v, detect %+v %v", i, det, err, wd, werr)
		}
		for _, p := range trace {
			if !reflect.DeepEqual(p.Names, processNames(p.Process)) {
				t.Errorf("fg %d pid %d: names %v", i, p.PID, p.Names)
			}
		}
	}
	trace, _, _ := s.ExplainProcess(fgs[0])
	var matches []string
	for _, p := range trace {
		matches = append(matches, p.Match)
	}
	if !reflect.DeepEqual(matches, []string{"", "", KindCodex, KindClaude}) || !trace[0].Leader || trace[1].Leader {
		t.Errorf("matches %v, trace %+v", matches, trace)
	}
	if trace, _, _ := s.ExplainProcess(fgs[1]); !reflect.DeepEqual(trace[0].Names, []string{"node", "codex"}) {
		t.Errorf("names offered twice or missing: %v", trace[0].Names)
	}
	if trace, _, _ := s.ExplainProcess(fgs[3]); len(trace) != 1 || trace[0].Leader || trace[0].Match != KindOpencode {
		t.Errorf("leaderless group: %+v", trace)
	}
}

func TestVisibleText(t *testing.T) {
	tests := []struct {
		plain string
		rows  int
		want  string
	}{
		{"old 1\nold 2\na\nb\nc\n\n\n", 5, "a\nb\nc"},
		{"a\r\nb\r\n", 24, "a\nb"},
		{"", 24, ""},
		{"\n\n\n", 3, ""},
		{"x\ny", 0, "x\ny"},
	}
	for _, tt := range tests {
		if got := VisibleText(tt.plain, tt.rows); got != tt.want {
			t.Errorf("VisibleText(%q, %d) = %q, want %q", tt.plain, tt.rows, got, tt.want)
		}
	}
}

// TestGroupProcesses lists a real process group: a leader running a copy of sleep named claude,
// and a member, and detects claude from it.
func TestGroupProcesses(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	leader := exec.Command(bin, "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Skipf("cannot run a copy of sleep: %v", err)
	}
	defer func() { leader.Process.Kill(); leader.Wait() }()
	pgid := leader.Process.Pid
	member := exec.Command(sleep, "30")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { member.Process.Kill(); member.Wait() }()

	fg, err := GroupProcesses(pgid)
	if err != nil {
		t.Fatal(err)
	}
	if fg.Leader.PID != pgid || fg.Leader.Name != "claude" || len(fg.Leader.Args) != 2 || fg.Leader.Args[1] != "30" {
		t.Errorf("leader %+v", fg.Leader)
	}
	if len(fg.Members) != 1 || fg.Members[0].PID != member.Process.Pid || !strings.HasSuffix(fg.Members[0].Name, "sleep") {
		t.Errorf("members %+v", fg.Members)
	}
	if d, err := Vendored().DetectProcess(fg); err != nil || d.ID != KindClaude || d.PID != pgid {
		t.Errorf("detect %+v %v", d, err)
	}

	member.Process.Kill()
	member.Wait()
	leader.Process.Kill()
	leader.Wait()
	if _, err := GroupProcesses(pgid); !errors.Is(err, ErrNoGroup) {
		t.Errorf("group after exit: %v, want ErrNoGroup", err)
	}
	if _, err := GroupProcesses(0); !errors.Is(err, ErrNoGroup) {
		t.Errorf("group 0: %v", err)
	}
}
