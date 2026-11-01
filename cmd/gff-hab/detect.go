package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// maxRegionText bounds the region text each rule carries in a detect.explain result.
const maxRegionText = 2048

// detectService serves detect.explain (harness-adapters.md, Detection): the pane's agent state
// and the source that won, the process part over the pane's foreground group, and every screen
// rule of the chosen manifest over the pane's screen now. It changes nothing.
type detectService struct {
	lk     paneReader
	store  *structure.Store
	agents *harness.Store
	// manifests loads the detection manifests for each call, so a refresh shows at once.
	manifests func() *detect.Set
	// group lists a process group's processes; detect.GroupProcesses, or a stand-in in tests.
	group func(pgid int) (detect.Foreground, error)
}

// paneReader is what explain reads from the holder link; *link.Link is one.
type paneReader interface {
	Screen(u hp.UUID) (link.Screen, error)
	Foreground(ctx context.Context, u hp.UUID) (int, error)
}

// linkPanes adapts *link.Link to paneReader.
type linkPanes struct{ lk *link.Link }

func (l linkPanes) Screen(u hp.UUID) (link.Screen, error) { return l.lk.Screen(u) }
func (l linkPanes) Foreground(ctx context.Context, u hp.UUID) (int, error) {
	return l.lk.Foreground(ctx, u)
}

var _ control.Detect = (*detectService)(nil)

func (d *detectService) Explain(ctx context.Context, p api.DetectExplainParams) (api.DetectExplain, error) {
	pane := p.Pane
	if p.Session != "" {
		ss, ok := d.store.Session(p.Session)
		if !ok {
			ss, ok = d.store.SessionByName(p.Session)
		}
		if !ok {
			return api.DetectExplain{}, control.Errorf(api.ErrorKindNotFound, "session %s does not exist", p.Session)
		}
		if ss.Pane == "" {
			return api.DetectExplain{}, control.Errorf(api.ErrorKindConflict, "session %s has no live pane", p.Session)
		}
		pane = ss.Pane
	}
	u, ok := parseUUID(pane)
	if !ok {
		return api.DetectExplain{}, control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane)
	}
	pane = u.String()
	scr, err := d.lk.Screen(u)
	if errors.Is(err, link.ErrNoPane) {
		return api.DetectExplain{}, control.Errorf(api.ErrorKindNotFound, "pane %s does not exist", pane)
	}
	res := api.DetectExplain{Pane: pane}
	harnessID := ""
	if tr, ok := d.agents.Trace(pane); ok {
		st := tr.State
		res.Agent, res.TurnSource, res.Open, res.OwnerStop = &st, tr.TurnSource, tr.Open, tr.OwnerStop
		harnessID = st.Harness
	}
	set := d.manifests()
	res.Process = d.process(ctx, u, set)

	var entry *detect.Entry
	chosen := ""
	if res.Process.Match != "" {
		entry, chosen = set.Lookup(res.Process.Match), "process"
	} else if harnessID != "" {
		entry, chosen = set.Lookup(harnessID), "agent"
	}
	if entry == nil {
		return res, nil
	}
	m := entry.Manifest
	res.Manifest = &api.DetectManifest{ID: m.ID, Version: m.Version, From: entry.From, Path: entry.Path, ChosenBy: chosen}
	if err != nil {
		return res, nil // a pane the link follows with no parser yet: no screen to read
	}
	text := detect.VisibleText(scr.Text, scr.Rows)
	tr := m.ExplainScreen(detect.Input{Screen: text})
	ds := &api.DetectScreen{Cols: scr.Cols, Rows: scr.Rows, State: string(tr.Result.State), Rule: tr.Result.Rule}
	if text != "" {
		ds.Lines = strings.Count(text, "\n") + 1
	}
	for _, r := range tr.Rules {
		t, cut := regionTail(r.Text, maxRegionText)
		ds.Rules = append(ds.Rules, api.DetectRule{ID: r.ID, State: string(r.State), Priority: r.Priority, Region: r.Region,
			Matched: r.Matched, Text: t, Truncated: cut})
	}
	res.Screen = ds
	return res, nil
}

// process runs the process part of detection over the pane's foreground group.
func (d *detectService) process(ctx context.Context, u hp.UUID, set *detect.Set) api.DetectProcess {
	var out api.DetectProcess
	pgid, err := d.lk.Foreground(ctx, u)
	if err != nil {
		out.Error = "foreground group: " + err.Error()
		return out
	}
	out.Pgid = pgid
	if pgid == 0 {
		out.Error = "the terminal has no foreground process group"
		return out
	}
	fg, err := d.group(pgid)
	if err != nil {
		out.Error = fmt.Sprintf("processes of group %d: %v", pgid, err)
		return out
	}
	trace, det, err := set.ExplainProcess(fg)
	for _, t := range trace {
		out.Processes = append(out.Processes, api.DetectProc{PID: t.PID, Name: t.Name, Args: t.Args, Leader: t.Leader, Names: t.Names, Match: t.Match})
	}
	if err == nil {
		out.Match, out.MatchPID, out.MatchName = det.ID, det.PID, det.Name
	}
	return out
}

// regionTail returns the last n bytes of s, starting on a line or rune boundary, and whether it cut.
func regionTail(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	t := s[len(s)-n:]
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < len(t)-1 {
		return t[i+1:], true
	}
	for len(t) > 0 && t[0]&0xc0 == 0x80 { // a UTF-8 continuation byte
		t = t[1:]
	}
	return t, true
}

func detectCmd(args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] != "explain" {
		return errors.New("detect needs a subcommand: explain")
	}
	fs := flag.NewFlagSet("detect explain", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	all := fs.Bool("all", false, "list every rule with the region text it read, not only the winner's")
	timeout := fs.Duration("timeout", 20*time.Second, "give up after this long")
	fs.Parse(args[1:])
	if fs.NArg() != 1 {
		return errors.New("detect explain needs one pane id, session id or session name")
	}
	p := api.DetectExplainParams{Session: fs.Arg(0)}
	if _, ok := parseUUID(fs.Arg(0)); ok {
		p = api.DetectExplainParams{Pane: fs.Arg(0)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "gff-hab detect", Version: build, Kind: api.ClientKindScript}, "", ""); err != nil {
		return err
	}
	res, err := api.Client{Caller: c}.DetectExplain(ctx, p)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	writeExplain(stdout, res, *all)
	return nil
}

// writeExplain prints a detect.explain result for a person.
func writeExplain(w io.Writer, r api.DetectExplain, all bool) {
	fmt.Fprintf(w, "pane      %s\n", r.Pane)

	// The state, and the source that won.
	if a := r.Agent; a != nil {
		st := string(a.State)
		if a.Kind != "" {
			st += ":" + string(a.Kind)
		}
		fmt.Fprintf(w, "state     %s %s, set by %s\n", a.Harness, st, a.Source)
		switch {
		case r.TurnSource == "":
			fmt.Fprintf(w, "          no source holds this turn: the next report of any source wins\n")
		case r.TurnSource == api.StateSourceScreen:
			fmt.Fprintf(w, "          strongest this turn: screen (a replayed OSC 7501 report); any live report wins\n")
		default:
			fmt.Fprintf(w, "          strongest this turn: %s; a weaker source cannot change the state until the turn ends\n", r.TurnSource)
		}
		if r.Open != "" {
			fmt.Fprintf(w, "          permission request %s is open; only a hook or server closes it\n", r.Open)
		}
		if r.OwnerStop {
			fmt.Fprintf(w, "          the owner's interrupt shows idle; sends wait until the harness reports\n")
		}
	} else {
		fmt.Fprintf(w, "state     none: no spawn, hook or OSC 7501 report reached gff-hab for this pane\n")
	}

	// The process part.
	pr := r.Process
	switch {
	case pr.Error != "":
		fmt.Fprintf(w, "process   %s\n", pr.Error)
	case pr.Match != "":
		fmt.Fprintf(w, "process   group %d: %s matched by %q (pid %d)\n", pr.Pgid, pr.Match, pr.MatchName, pr.MatchPID)
	default:
		fmt.Fprintf(w, "process   group %d: no process matches a manifest\n", pr.Pgid)
	}
	for _, p := range pr.Processes {
		mark := " "
		if p.Leader {
			mark = "*"
		}
		m := "-"
		if p.Match != "" {
			m = p.Match
		}
		fmt.Fprintf(w, "          %s %d %s  names %s  -> %s\n", mark, p.PID, p.Name, strings.Join(p.Names, ", "), m)
	}

	// The manifest and the screen rules.
	if r.Manifest == nil {
		fmt.Fprintf(w, "manifest  none: neither the foreground process nor the agent state names one\n")
	} else {
		mf := r.Manifest
		fmt.Fprintf(w, "manifest  %s %s (%s, %s), chosen by %s\n", mf.ID, mf.Version, mf.From, mf.Path, mf.ChosenBy)
	}
	if s := r.Screen; s != nil {
		matched := 0
		var win *api.DetectRule
		for i := range s.Rules {
			if s.Rules[i].Matched {
				matched++
			}
			if s.Rules[i].ID == s.Rule && s.Rules[i].Matched && win == nil {
				win = &s.Rules[i]
			}
		}
		if win != nil {
			fmt.Fprintf(w, "screen    %s by rule %s (priority %d, region %s)\n", s.State, win.ID, win.Priority, win.Region)
		} else {
			fmt.Fprintf(w, "screen    unknown: none of %d rules matched\n", len(s.Rules))
		}
		fmt.Fprintf(w, "          %dx%d, %d lines read\n", s.Cols, s.Rows, s.Lines)
		fmt.Fprintf(w, "rules     %d of %d matched\n", matched, len(s.Rules))
		for i := range s.Rules {
			ru := &s.Rules[i]
			if !all && !ru.Matched {
				continue
			}
			m := "no   "
			if ru.Matched {
				m = "match"
			}
			won := ""
			if ru == win {
				won = "  <- won"
			}
			fmt.Fprintf(w, "          %s %s  %s  priority %d  %s%s\n", m, ru.ID, ru.State, ru.Priority, ru.Region, won)
			if all || ru == win {
				writeRegion(w, ru)
			}
		}
		if !all && matched < len(s.Rules) {
			fmt.Fprintf(w, "          %d more did not match (-all lists them with their regions)\n", len(s.Rules)-matched)
		}
	}

	// The verdict.
	fmt.Fprintf(w, "verdict   %s\n", verdict(r))
}

func writeRegion(w io.Writer, ru *api.DetectRule) {
	if ru.Text == "" {
		fmt.Fprintf(w, "            (region empty)\n")
		return
	}
	if ru.Truncated {
		fmt.Fprintf(w, "            (last %d bytes)\n", len(ru.Text))
	}
	for _, l := range strings.Split(strings.TrimRight(ru.Text, "\n"), "\n") {
		fmt.Fprintf(w, "            | %s\n", l)
	}
}

// verdict says in one line why the pane shows what it shows.
func verdict(r api.DetectExplain) string {
	var b strings.Builder
	screen := ""
	if r.Screen != nil && r.Screen.State != string(detect.ScreenUnknown) {
		screen = r.Screen.State
	}
	if a := r.Agent; a != nil {
		fmt.Fprintf(&b, "%s from %s.", a.State, a.Source)
		if r.Process.Match != "" && r.Process.Match != a.Harness {
			fmt.Fprintf(&b, " The foreground runs %s, not %s.", r.Process.Match, a.Harness)
		} else if r.Process.Error == "" && r.Process.Match == "" && a.State != api.AgentStatusExited {
			fmt.Fprintf(&b, " No foreground process matches %s.", a.Harness)
		}
	} else {
		b.WriteString("no agent state.")
	}
	switch {
	case screen == "":
	case r.Agent != nil && screen == string(r.Agent.State):
		b.WriteString(" The screen agrees.")
	default:
		fmt.Fprintf(&b, " The screen says %s; screen rules are a fallback this build shows but does not apply.", screen)
	}
	return b.String()
}
