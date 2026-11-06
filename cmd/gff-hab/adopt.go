package main

// session.adopt and session.outside (gff-client.md 6.5): bring a Claude Code or Codex
// conversation started in a plain terminal into a new pane by its resume id. The transcript
// stays where the harness wrote it; gff-hab only reads it, for the folder.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// Bounds of session.outside.
const (
	outsideDefault = 20
	outsideMax     = 100
	// psTimeout bounds the process listing.
	psTimeout = 5 * time.Second
)

// outsideProc is one process on the host: its pid and its arguments as one line.
type outsideProc struct {
	pid  int
	args string
}

// adopter serves session.adopt and session.outside.
type adopter struct {
	acts *actions
	book *resumeBook
	// agents lists the agent states the hab knows.
	agents func() []api.AgentState
	getenv func(string) string
	// procs lists the host's processes; alive says whether a pid runs.
	procs func(ctx context.Context) ([]outsideProc, error)
	alive func(pid int) bool
	self  int
	logf  func(string, ...any)

	op sync.Mutex // one adopt at a time: two adopts of one id never both pass the checks
}

func newAdopter(acts *actions, book *resumeBook, agents func() []api.AgentState, logf func(string, ...any)) *adopter {
	return &adopter{acts: acts, book: book, agents: agents, getenv: os.Getenv, procs: listProcs,
		alive: pidAlive, self: os.Getpid(), logf: logf}
}

var _ control.Adopter = (*actions)(nil)

// AdoptSession serves session.adopt.
func (a *actions) AdoptSession(ctx context.Context, p api.SessionAdoptParams) (api.SessionAdopted, error) {
	if a.adopt == nil {
		return api.SessionAdopted{}, control.Errorf(api.ErrorKindUnavailable, "this hab cannot adopt a session")
	}
	return a.adopt.Adopt(ctx, p)
}

// OutsideConversations serves session.outside.
func (a *actions) OutsideConversations(ctx context.Context, p api.SessionOutsideParams) ([]api.OutsideConversation, error) {
	if a.adopt == nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "this hab cannot adopt a session")
	}
	return a.adopt.Outside(ctx, p)
}

// harnesses returns the harnesses a request names: one, or both when empty.
func adoptHarnesses(h string) ([]string, error) {
	switch h {
	case "":
		return []string{transcript.HarnessClaude, transcript.HarnessCodex}, nil
	case transcript.HarnessClaude, transcript.HarnessCodex:
		return []string{h}, nil
	case "opencode":
		return nil, control.Errorf(api.ErrorKindUnavailable, "adopt of an opencode conversation is not built: its conversations live in its server's database")
	}
	return nil, control.Errorf(api.ErrorKindInvalid, "harness %q: want claude or codex", h)
}

// find reads the conversation id names, under the harness given or each one.
func (d *adopter) find(harness, id string) (transcript.Conversation, error) {
	if !transcript.ValidID(id) {
		return transcript.Conversation{}, control.Errorf(api.ErrorKindInvalid, "id %q is not a session id: letters, digits, - and _", id)
	}
	hs, err := adoptHarnesses(harness)
	if err != nil {
		return transcript.Conversation{}, err
	}
	var found []transcript.Conversation
	for _, h := range hs {
		c, err := d.conversation(h, id)
		switch {
		case errors.Is(err, transcript.ErrNoTranscript):
		case err != nil:
			return transcript.Conversation{}, control.Errorf(api.ErrorKindUnavailable, "%s: %v", h, err)
		default:
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return transcript.Conversation{}, control.Errorf(api.ErrorKindNotFound, "no %s transcript on this host holds %s", strings.Join(hs, " or "), id)
	case 1:
		return found[0], nil
	}
	return transcript.Conversation{}, control.Errorf(api.ErrorKindInvalid, "both claude and codex hold %s: give the harness", id)
}

func (d *adopter) conversation(h, id string) (transcript.Conversation, error) {
	if h == transcript.HarnessClaude {
		dir, err := transcript.ClaudeConfigDir(d.getenv)
		if err != nil {
			return transcript.Conversation{}, err
		}
		return transcript.ClaudeConversation(dir, id)
	}
	home, err := transcript.CodexHome(d.getenv)
	if err != nil {
		return transcript.Conversation{}, err
	}
	return transcript.CodexConversation(home, id)
}

// holder names the session of this hab that holds a conversation: its book names the id, or an
// agent in one of its panes reports it. "" for none.
func (d *adopter) holder(harness, id string) string {
	if s, ok := d.book.holder(harness, id); ok {
		return d.sessionName(s)
	}
	if d.agents == nil {
		return ""
	}
	for _, st := range d.agents() {
		if st.Harness == harness && st.Session == id && st.State != api.AgentStatusExited {
			if ss, ok := d.acts.store.SessionByPane(st.Pane); ok {
				return ss.Name
			}
			return "pane " + st.Pane
		}
	}
	return ""
}

func (d *adopter) sessionName(id string) string {
	if ss, ok := d.acts.store.Session(id); ok {
		return ss.Name
	}
	return id
}

// outsideView is what the hab can see of the processes outside it, read once per call.
type outsideView struct {
	registry []transcript.ClaudeRunning
	procs    []outsideProc
}

func (d *adopter) view(ctx context.Context) outsideView {
	var v outsideView
	if dir, err := transcript.ClaudeConfigDir(d.getenv); err == nil {
		v.registry = transcript.ClaudeRegistry(dir)
	}
	pctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	procs, err := d.procs(pctx)
	if err != nil && d.logf != nil {
		d.logf("adopt: the process list: %v", err)
	}
	v.procs = procs
	return v
}

// runner returns the pid of a process outside the hab that runs the conversation, or 0.
func (d *adopter) runner(v outsideView, harness, id string) int {
	if harness == transcript.HarnessClaude {
		for _, r := range v.registry {
			if r.SessionID == id && r.PID != d.self && d.alive(r.PID) {
				return r.PID
			}
		}
	}
	for _, p := range v.procs {
		if p.pid != d.self && runsConversation(p.args, harness, id) {
			return p.pid
		}
	}
	return 0
}

// runsConversation says whether a process's arguments hold the id as a word beside a word
// naming the harness. gff and gff-hab themselves never count: `gff-hab adopt <id>` holds both.
func runsConversation(args, harness, id string) bool {
	words := strings.Fields(args)
	if len(words) == 0 || !slices.Contains(words, id) {
		return false
	}
	if b := filepath.Base(words[0]); b == "gff-hab" || b == "gff" {
		return false
	}
	for _, w := range words {
		if w != id && strings.Contains(w, harness) {
			return true
		}
	}
	return false
}

// Adopt starts the harness's resume of a conversation in a new pane and files it as a session.
func (d *adopter) Adopt(ctx context.Context, p api.SessionAdoptParams) (api.SessionAdopted, error) {
	d.op.Lock()
	defer d.op.Unlock()
	c, err := d.find(p.Harness, p.ID)
	if err != nil {
		return api.SessionAdopted{}, err
	}
	if name := d.holder(c.Harness, c.ID); name != "" {
		return api.SessionAdopted{}, control.Errorf(api.ErrorKindConflict, "session %s holds %s conversation %s: resume that session", name, c.Harness, c.ID)
	}
	if pid := d.runner(d.view(ctx), c.Harness, c.ID); pid != 0 {
		return api.SessionAdopted{}, control.Errorf(api.ErrorKindConflict, "a %s process outside the hab runs conversation %s (pid %d): quit it there first", c.Harness, c.ID, pid)
	}
	cwd := p.Cwd
	switch {
	case cwd != "":
	case c.Cwd == "":
		return api.SessionAdopted{}, control.Errorf(api.ErrorKindInvalid, "the transcript of %s names no folder: give cwd", c.ID)
	default:
		cwd = c.Cwd
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return api.SessionAdopted{}, control.Errorf(api.ErrorKindInvalid, "the folder %s of conversation %s no longer exists: give cwd", cwd, c.ID)
		}
	}
	if err := checkCwd(cwd); err != nil {
		return api.SessionAdopted{}, err
	}
	st, err := capcom.ResumeFor(c.Harness, c.ID)
	if err != nil {
		return api.SessionAdopted{}, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	cp := api.SessionCreateParams{Name: p.Name, Harness: c.Harness, Argv: st.Argv, Cwd: cwd}
	if _, _, _, err := gitIdentity(ctx, cwd); err == nil {
		cp.ProjectDir = cwd
	}
	session, pane, err := d.acts.CreateSession(ctx, cp)
	if err != nil {
		return api.SessionAdopted{}, err
	}
	d.book.adopted(session, c.Harness, c.ID)
	return api.SessionAdopted{Session: session, Pane: pane, Harness: c.Harness, Cwd: cwd}, nil
}

// Outside lists recent conversations no session of this hab holds, newest first.
func (d *adopter) Outside(ctx context.Context, p api.SessionOutsideParams) ([]api.OutsideConversation, error) {
	hs, err := adoptHarnesses(p.Harness)
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = outsideDefault
	}
	limit = min(limit, outsideMax)
	var all []transcript.Conversation
	for _, h := range hs {
		var list []transcript.Conversation
		if h == transcript.HarnessClaude {
			dir, err := transcript.ClaudeConfigDir(d.getenv)
			if err != nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "claude: %v", err)
			}
			list, err = transcript.RecentClaude(dir, limit)
			if err != nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "claude: %v", err)
			}
		} else {
			home, err := transcript.CodexHome(d.getenv)
			if err != nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "codex: %v", err)
			}
			list, err = transcript.RecentCodex(home, limit)
			if err != nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "codex: %v", err)
			}
		}
		all = append(all, list...)
	}
	slices.SortStableFunc(all, func(a, b transcript.Conversation) int { return b.Updated.Compare(a.Updated) })
	v := d.view(ctx)
	out := make([]api.OutsideConversation, 0, len(all))
	for _, c := range all {
		if d.holder(c.Harness, c.ID) != "" {
			continue
		}
		out = append(out, api.OutsideConversation{Harness: c.Harness, ID: c.ID, Cwd: c.Cwd, Title: c.Title,
			Updated: c.Updated.UTC().Format(time.RFC3339), Live: d.runner(v, c.Harness, c.ID) != 0})
	}
	return out, nil
}

// listProcs lists every process on the host with its arguments, through ps.
func listProcs(ctx context.Context) ([]outsideProc, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcs(out), nil
}

// parseProcs reads ps's "pid args" lines.
func parseProcs(out []byte) []outsideProc {
	var procs []outsideProc
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		pidText, args, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		procs = append(procs, outsideProc{pid: pid, args: strings.TrimSpace(args)})
	}
	return procs
}

// pidAlive says whether a process with pid runs.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
