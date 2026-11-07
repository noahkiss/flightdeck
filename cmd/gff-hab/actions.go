package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/link"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Defaults for session.create (hab-logic-api.md 3.9).
const (
	defaultCols = 80
	defaultRows = 24
	// freeNameRetries bounds how often an unnamed create picks its name again when another
	// create took the picked one first.
	freeNameRetries = 2
)

// actions serves session.create and pane.close over the holder link and the structure store.
type actions struct {
	sp      *spawner
	store   *structure.Store
	getenv  func(string) string
	environ func() []string
	home    string // the default working directory
	// refresh asks for a read of a project's repository state (internal/gitstatus); nil for none.
	refresh func(project string)
	// opencode follows the private server of each opencode pane; nil: opencode cannot start.
	opencode *opencodeServers
	// exe is gff-hab's own executable, which an opencode pane runs as its wrapper.
	exe string
	// peerName returns where a name is in use on a peer, from the catalog copies, or ""
	// (mesh.md 6.5: names unique across peers). nil checks nothing.
	peerName func(name string) string
	// startedIn hears the folder a new session started in (content and attachments, app.md
	// 4.4 and 4.7). nil: none.
	startedIn func(session, dir string)
	// started hears each pane started with a harness, and the folder it runs in: the trust
	// card watches it for a folder trust prompt (app.md 7.10). nil: none.
	started func(pane, harness, dir string)
	// seed queues a new session's first message in the channels inbox (capcom.md 3.3). nil: a
	// create with a seed answers unavailable.
	seed func(session, from, text string) (string, error)
	// resume serves session.resume for the sessions it knows (Capcom's); ok false for others.
	// nil: none.
	resume func(ctx context.Context, session string) (pane string, ok bool, err error)
	// general serves session.resume for every other session (resume.go). nil: unavailable.
	general func(ctx context.Context, session string) (string, error)
	// folderOf is a session's working folder (the content service's), "" when not known. nil:
	// none.
	folderOf func(ctx context.Context, session string) string
	// created hears how a new session was asked to start, for its resume. nil: none.
	created func(session string, p api.SessionCreateParams)
	// adopt serves session.adopt and session.outside (adopt.go). nil: unavailable.
	adopt *adopter
	// mates keeps the sessions that report to their spawner (teammates.go). nil: a create
	// with report_to answers unavailable.
	mates *teammates
	// self is this hab's own channel end (device id and host id), for a report_to from a
	// caller on this hab.
	self api.ChannelEnd
	// listedPeer reports whether a device is a labelled peer in this hab's peer list, so a
	// report to it can go. nil: none is.
	listedPeer func(device string) bool
	// reserved reports a name only the Capcom service may hold: capcom and capcom-<anything>
	// while the service runs (capcom-service.md 2.4). nil: none.
	reserved func(name string) bool
}

// isReserved reports whether name is kept for the Capcom service.
func (a *actions) isReserved(name string) bool { return a.reserved != nil && a.reserved(name) }

var _ control.Resumer = (*actions)(nil)

// ResumeSession serves session.resume: Capcom's sessions through Capcom (capcom.md 7.4), the
// rest through the resume book (gff-client.md 6.2).
func (a *actions) ResumeSession(ctx context.Context, session string) (string, error) {
	if a.resume != nil {
		pane, ok, err := a.resume(ctx, session)
		if ok {
			return pane, err
		}
	}
	if a.general != nil {
		return a.general(ctx, session)
	}
	if _, ok := a.store.Session(session); !ok {
		return "", control.Errorf(api.ErrorKindNotFound, "session %s does not exist", session)
	}
	return "", control.Errorf(api.ErrorKindUnavailable, "this hab cannot resume session %s", session)
}

// onPeer returns where name is in use on a peer, or "".
func (a *actions) onPeer(name string) string {
	if a.peerName == nil {
		return ""
	}
	return a.peerName(name)
}

var _ control.Actions = (*actions)(nil)

// CreateSession files a session for a new pane, then starts the program in that pane through
// the spawner. It checks every input before it files. The record comes first so the pane's
// GFF_HAB token names the session from the program's first byte: a program that calls back at
// once (gff-hab show, send, wait) finds its session. If the spawn fails, it closes the session
// again, so no session is left that never ran.
func (a *actions) CreateSession(ctx context.Context, p api.SessionCreateParams) (string, string, error) {
	return a.createSession(ctx, p, "", "")
}

// createSession is CreateSession with a kind and a folder for the record (a Capcom session).
func (a *actions) createSession(ctx context.Context, p api.SessionCreateParams, kind, dir string) (string, string, error) {
	switch {
	case p.Model != "" && p.Harness == "":
		return "", "", control.Errorf(api.ErrorKindInvalid, "model needs harness")
	case p.Seed != "" && p.Harness == "":
		return "", "", control.Errorf(api.ErrorKindInvalid, "seed needs harness: it is the agent's first message")
	case p.Seed != "" && a.seed == nil:
		return "", "", control.Errorf(api.ErrorKindUnavailable, "this hab cannot queue a seed: it runs without channels")
	case strings.HasPrefix(p.Model, "-"):
		return "", "", control.Errorf(api.ErrorKindInvalid, "model %q is not a model name", p.Model)
	case p.SeedFrom != "" && !validSeedFrom(p.SeedFrom):
		return "", "", control.Errorf(api.ErrorKindInvalid, "seed_from %q is not host/session: a host id of printable characters with no space, quote or bracket, then a session name of %s", p.SeedFrom, channels.NameRule)
	case p.ReportTo != "" && p.Harness == "":
		return "", "", control.Errorf(api.ErrorKindInvalid, "report_to needs harness: it reports the agent's turns")
	case p.ReportTo != "" && !channels.ValidName(p.ReportTo):
		return "", "", control.Errorf(api.ErrorKindInvalid, "report_to %q: use %s", p.ReportTo, channels.NameRule)
	case p.ReportTo != "" && (a.mates == nil || a.self.DeviceID == ""):
		return "", "", control.Errorf(api.ErrorKindUnavailable, "this hab cannot report a session's turns: it runs without channels")
	}
	var reportTo api.ChannelEnd
	if p.ReportTo != "" {
		reportTo = api.ChannelEnd{DeviceID: a.self.DeviceID, Host: a.self.Host, Session: p.ReportTo}
		if peer, ok := control.CallerPeer(ctx); ok {
			// Over a peer link: the session is on the dialing hab, named by the link's hello.
			reportTo.DeviceID, reportTo.Host = peer.DeviceID, peer.Host
			if a.listedPeer == nil || !a.listedPeer(peer.DeviceID) {
				// A report goes only to a hab in the peer list: refuse now, not lose each one.
				return "", "", control.Errorf(api.ErrorKindUnavailable, "this hab's peer list does not name %s, so a teammate here cannot report to it: add it as a peer, or spawn with mode detached", peer.Host)
			}
		}
	}
	if p.Seed != "" {
		if err := confirm.CheckSeed(p.Seed); err != nil {
			return "", "", err
		}
	}
	asked := p // before prepare fills in the harness's start
	p, l, err := a.prepare(p)
	if err != nil {
		return "", "", err
	}

	if p.CwdOf != "" && p.Cwd == "" && p.ProjectDir == "" {
		// A new pane beside another (gff-client.md 5.4): its folder, and its project unless the
		// create names one.
		p.Cwd = a.paneCwd(ctx, p.CwdOf)
		if ss, ok := a.store.SessionByPane(p.CwdOf); ok && p.Project == "" {
			p.Project = ss.Project
		}
	}
	project, projectTop := p.Project, ""
	switch {
	case p.Project != "" && p.ProjectDir != "":
		return "", "", control.Errorf(api.ErrorKindInvalid, "give project or project_dir, not both")
	case p.Project != "":
		if _, ok := a.store.Project(p.Project); !ok {
			return "", "", control.Errorf(api.ErrorKindNotFound, "project %s does not exist", p.Project)
		}
	case p.ProjectDir != "":
		if !filepath.IsAbs(p.ProjectDir) {
			return "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %q is not an absolute path", p.ProjectDir)
		}
		if project, projectTop, err = a.projectFor(ctx, p.ProjectDir); err != nil {
			return "", "", err
		}
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = p.ProjectDir
	}
	if cwd == "" && p.Project != "" {
		cwd = a.recentDir(p.Project)
	}
	if cwd == "" {
		cwd = a.home
	}
	if err := checkCwd(cwd); err != nil {
		return "", "", err
	}

	argv := l.spawn.Argv
	// A round's run session is named after the round and marked with it (rounds.md 7.2).
	base := filepath.Base(argv[0])
	rs, isRun := roundStartFrom(ctx)
	if isRun {
		base = rs.name
	}
	name := p.Name
	if name == "" {
		name = a.freeName(base)
	} else if !channels.ValidName(name) {
		return "", "", control.Errorf(api.ErrorKindInvalid, "session name %q: use %s", name, channels.NameRule)
	} else if a.isReserved(name) {
		return "", "", control.Errorf(api.ErrorKindInvalid, "session name %q is kept for the Capcom service; capcom and names that start with capcom- are not free", name)
	} else if _, taken := a.store.SessionByName(name); taken {
		return "", "", control.Errorf(api.ErrorKindConflict, "a session named %q exists", name)
	} else if label := a.onPeer(name); label != "" {
		return "", "", control.Errorf(api.ErrorKindConflict, "name %q in use on %s", name, label)
	}

	pane := l.spawn.UUID.String()
	spec := structure.SessionSpec{Name: name, Project: project, Pane: pane, Index: -1, NewPane: true, Kind: kind, Dir: dir}
	if isRun {
		spec.Round = &rs.ref
	}
	ss, err := a.store.CreateSession(spec)
	// A picked name another create took between freeName and the record: pick again.
	for try := 0; try < freeNameRetries && p.Name == "" && errors.Is(err, structure.ErrConflict); try++ {
		spec.Name = a.freeName(base)
		ss, err = a.store.CreateSession(spec)
	}
	if err != nil {
		return "", "", storeErr(err)
	}
	l.spawn.Cwd = cwd
	// The program never ran: session.deleted follows the record's session.updated. Its only
	// error is not_found, when a client closed the session meanwhile.
	if err := a.launch(ctx, l, func() { _ = a.store.CloseSession(ss.ID) }); err != nil {
		return "", "", err
	}
	a.noteDir(ctx, project, projectTop, p.Project != "" && p.Cwd != "", cwd)
	if a.created != nil && kind == "" {
		if asked.Harness == "" {
			asked.Argv = argv // the shell when none was asked for
		}
		a.created(ss.ID, asked)
	}
	if a.startedIn != nil {
		a.startedIn(ss.ID, cwd)
	}
	if a.started != nil && p.Harness != "" {
		a.started(pane, p.Harness, cwd)
	}
	if p.ReportTo != "" {
		// Filed before the seed is queued, so the seed's delivery arms the first report.
		a.mates.add(ss.ID, pane, reportTo)
	}
	if p.Seed != "" {
		// The session runs; a seed that cannot be queued is logged, not an error, so a retry
		// with the same idempotency key never starts a second session.
		if _, err := a.seed(ss.Name, p.SeedFrom, p.Seed); err != nil && a.sp.logf != nil {
			a.sp.logf("session %s: seed: %v", ss.Name, err)
		}
	}
	return ss.ID, pane, nil
}

// resumeSession starts an exited session's program again in a new pane, in the folder cwd
// (session.resume). p gives the harness and argv; its name, project and cwd fields are not
// read. The session keeps its id. If the spawn fails, the session is exited again.
func (a *actions) resumeSession(ctx context.Context, session string, p api.SessionCreateParams, cwd string) (string, error) {
	p, l, err := a.prepare(p)
	if err != nil {
		return "", err
	}
	if err := checkCwd(cwd); err != nil {
		return "", err
	}
	pane := l.spawn.UUID.String()
	if _, err := a.store.ResumeSession(session, pane); err != nil {
		return "", storeErr(err)
	}
	l.spawn.Cwd = cwd
	if err := a.launch(ctx, l, func() { a.store.ResumeFailed(session, pane) }); err != nil {
		return "", err
	}
	if a.mates != nil {
		a.mates.repane(session, pane) // a teammate reports from its new pane
	}
	if a.startedIn != nil {
		a.startedIn(session, cwd)
	}
	if a.started != nil && p.Harness != "" {
		a.started(pane, p.Harness, cwd)
	}
	return pane, nil
}

// checkCwd refuses a working folder that is not an absolute path to a directory.
func checkCwd(cwd string) error {
	if !filepath.IsAbs(cwd) {
		return control.Errorf(api.ErrorKindInvalid, "cwd %q is not an absolute path", cwd)
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return control.Errorf(api.ErrorKindInvalid, "cwd %q is not a directory", cwd)
	}
	return nil
}

// launchPlan is a checked spawn, before the record: the pane's uuid, argv, environment and size.
type launchPlan struct {
	spawn hp.Spawn
	oc    bool // an opencode pane: register its server after the spawn
}

// prepare fills in the harness's start, the default argv and size, and the environment, and
// checks them. The spawn's Cwd is left for the caller.
func (a *actions) prepare(p api.SessionCreateParams) (api.SessionCreateParams, launchPlan, error) {
	if p.Harness != "" {
		var err error
		if p, err = harnessStart(p); err != nil {
			return p, launchPlan{}, err
		}
	}
	var paneID hp.UUID
	ocStarted := false
	if p.Harness == detect.KindOpencode {
		var err error
		if paneID, p, err = a.opencodeStart(p); err != nil {
			return p, launchPlan{}, err
		}
		ocStarted = true
	}
	cols, rows := p.Cols, p.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	if cols < 1 || rows < 1 || cols > 0xffff || rows > 0xffff {
		return p, launchPlan{}, control.Errorf(api.ErrorKindInvalid, "size %dx%d: cols and rows must be 1 to 65535", cols, rows)
	}
	argv := p.Argv
	if len(argv) == 0 {
		sh := a.getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		argv = []string{sh}
	}
	if argv[0] == "" {
		return p, launchPlan{}, control.Errorf(api.ErrorKindInvalid, "argv[0] is empty")
	}
	env, err := mergeEnv(a.environ(), p.Env)
	if err != nil {
		return p, launchPlan{}, err
	}
	if paneID == (hp.UUID{}) {
		paneID = newUUID()
	}
	return p, launchPlan{spawn: hp.Spawn{UUID: paneID, Cols: uint16(cols), Rows: uint16(rows), Argv: argv, Env: env}, oc: ocStarted}, nil
}

// launch spawns a filed session's pane. When the program never ran, undo runs and launch
// returns the error.
func (a *actions) launch(ctx context.Context, l launchPlan, undo func()) error {
	if _, err := a.sp.spawn(ctx, l.spawn); err != nil {
		if spawnRefused(err) || !a.sp.exists(l.spawn.UUID) {
			undo()
			if errors.Is(err, holderclient.ErrNoHolder) {
				return control.Errorf(api.ErrorKindUnavailable, "no holder is running on this host")
			}
			return control.Errorf(api.ErrorKindUnavailable, "spawn: %v", err)
		}
		// The answer was lost (a timeout, a closed link), but the holder started the pane:
		// the program runs, so its session stays and the call succeeds. The same spawn
		// again attaches the pane, so the link follows it: its output, exit and token work.
		a.sp.follow(ctx, l.spawn)
	}
	if l.oc {
		pane := l.spawn.UUID.String()
		if err := a.opencode.register(pane, l.spawn.UUID); err != nil {
			// The pane runs; only the server path is missing: state, sends and answers wait.
			a.opencode.logf("opencode: pane %s: %v", pane, err)
		}
	}
	return nil
}

// validSeedFrom reports whether seed_from is host/session, so it cannot forge the seed
// envelope's header. The session is the part after the last slash, in the channel charset. The
// host is a host id as -host-id or the hostname gives it, which may hold ':', '/' or '+': at
// most 255 bytes of printable characters with no space, quote, backslash or bracket.
func validSeedFrom(s string) bool {
	i := strings.LastIndexByte(s, '/')
	if i <= 0 || len(s[:i]) > 255 || !channels.ValidName(s[i+1:]) {
		return false
	}
	for _, r := range s[:i] {
		if r <= ' ' || r == 0x7f || !unicode.IsPrint(r) || strings.ContainsRune("'\"\\[]`", r) {
			return false
		}
	}
	return true
}

// recentDir returns the project's most recently used folder that is still a directory, or "".
func (a *actions) recentDir(project string) string {
	for _, pd := range a.store.ProjectDirs() {
		if pd.ID != project {
			continue
		}
		for _, d := range pd.Dirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				return d
			}
		}
	}
	return ""
}

// harnessStart fills in how session.create starts a known harness (harness-adapters.md,
// Environment contract and Open decisions). Codex runs with --no-daemon, so its agent loop and
// tool commands stay children of the holder and see the pane's environment; argv defaults to
// `codex` and gets the flag when it lacks it. opencode's argv defaults to `opencode`;
// opencodeStart then wraps it. Claude Code's argv defaults to `claude`. A model goes after the
// program's other arguments as --model. The pane gets AGENT_KIND.
func harnessStart(p api.SessionCreateParams) (api.SessionCreateParams, error) {
	switch p.Harness {
	case detect.KindClaude:
		if len(p.Argv) == 0 {
			p.Argv = []string{"claude"}
		}
	case detect.KindOpencode:
		if len(p.Argv) == 0 {
			p.Argv = []string{"opencode"}
		}
	case detect.KindCodex:
		argv := slices.Clone(p.Argv)
		if len(argv) == 0 {
			argv = []string{"codex"}
		}
		if !slices.Contains(argv[1:], "--no-daemon") {
			argv = slices.Insert(argv, 1, "--no-daemon")
		}
		p.Argv = argv
	default:
		return p, control.Errorf(api.ErrorKindUnavailable,
			"session.create with harness %q is not built yet; only claude, codex and opencode are. Give argv instead", p.Harness)
	}
	if p.Model != "" {
		p.Argv = append(slices.Clone(p.Argv), "--model", p.Model)
	}
	p.Env = append(slices.Clone(p.Env), "AGENT_KIND="+p.Harness)
	return p, nil
}

// opencodeStart wraps an opencode argv in `gff-hab opencode-pane`, which runs a private
// opencode server for the pane and the TUI attached to it (harness-adapters.md, opencode,
// built choices). It picks the pane's uuid, names the server file the wrapper reports the
// server's URL in, and passes the pane's server password in the environment for the wrapper to
// hand on. It returns the uuid to spawn under.
func (a *actions) opencodeStart(p api.SessionCreateParams) (hp.UUID, api.SessionCreateParams, error) {
	if a.opencode == nil || a.exe == "" {
		return hp.UUID{}, p, control.Errorf(api.ErrorKindUnavailable, "this gff-hab cannot start opencode: it does not know its own executable")
	}
	if p.Argv[0] == "" {
		return hp.UUID{}, p, control.Errorf(api.ErrorKindInvalid, "argv[0] is empty")
	}
	for _, arg := range p.Argv[1:] {
		if arg == "--server" || strings.HasPrefix(arg, "--server=") {
			return hp.UUID{}, p, control.Errorf(api.ErrorKindInvalid, "argv names --server; gff-hab starts opencode's server itself")
		}
	}
	if err := os.MkdirAll(a.opencode.dir, 0o700); err != nil {
		return hp.UUID{}, p, control.Errorf(api.ErrorKindUnavailable, "the opencode state folder: %v", err)
	}
	u := newUUID()
	p.Argv = append([]string{a.exe, "opencode-pane", "-report", a.opencode.serverPath(u.String()), "--"}, p.Argv...)
	p.Env = append(slices.Clone(p.Env), opencodePasswordEnv+"="+a.opencode.password(u))
	return u, p, nil
}

// noteDir records the repository folder a new session works in as its project's most recently
// used directory, then asks for a read of the project's repository state. top is the folder
// project_dir resolved to. With a project given by id and a cwd, the cwd's repository counts
// when it has the project's identity.
func (a *actions) noteDir(ctx context.Context, project, top string, checkCwd bool, cwd string) {
	if project == "" {
		return
	}
	if top == "" && checkCwd {
		t, remote, root, err := gitIdentity(ctx, cwd)
		if err == nil {
			if pr, ok := a.store.ProjectByIdentity(remote, root); ok && pr.ID == project {
				top = t
			}
		}
	}
	if top != "" {
		_ = a.store.AddProjectDir(project, top) // the project may be gone meanwhile
	}
	if a.refresh != nil {
		a.refresh(project)
	}
}

var _ control.SessionRecords = (*actions)(nil)

// RenameSession renames a session record (session.rename). A name another session holds is a
// conflict.
func (a *actions) RenameSession(session, name string) error {
	if !channels.ValidName(name) {
		return control.Errorf(api.ErrorKindInvalid, "session name %q: use %s", name, channels.NameRule)
	}
	if a.isReserved(name) {
		return control.Errorf(api.ErrorKindInvalid, "session name %q is kept for the Capcom service; capcom and names that start with capcom- are not free", name)
	}
	if label := a.onPeer(name); label != "" {
		return control.Errorf(api.ErrorKindConflict, "name %q in use on %s", name, label)
	}
	if _, err := a.store.RenameSession(session, name); err != nil {
		return storeErr(err)
	}
	return nil
}

// CloseSession removes a session record (session.close). A live pane keeps running, unfiled
// (hab-logic-api.md 2.2).
func (a *actions) CloseSession(session string) error {
	if err := a.store.CloseSession(session); err != nil {
		return storeErr(err)
	}
	return nil
}

// ClosePane releases a pane through the link (hab-logic-api.md 3.9).
func (a *actions) ClosePane(ctx context.Context, pane string) error {
	u, ok := parseUUID(pane)
	if !ok {
		return control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane)
	}
	err := a.sp.lk.ClosePane(ctx, u)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, link.ErrNoPane):
		return control.Errorf(api.ErrorKindNotFound, "pane %s does not exist", pane)
	case errors.Is(err, link.ErrLost):
		return control.Errorf(api.ErrorKindUnavailable, "pane %s is lost: its holder is not connected", pane)
	case errors.Is(err, link.ErrReleasing):
		return control.Errorf(api.ErrorKindConflict, "pane %s is being released already", pane)
	}
	return control.Errorf(api.ErrorKindUnavailable, "close pane %s: %v", pane, err)
}

// freeName returns base, or base-2, base-3 and so on: the first name no session holds.
// It keeps to channels.NameRule: other characters in base become -, and base is cut to 56.
func (a *actions) freeName(base string) string {
	base = strings.Map(func(r rune) rune {
		if r < 0x80 && channels.ValidName(string(r)) {
			return r
		}
		return '-'
	}, base)
	if len(base) > 56 {
		base = base[:56]
	}
	if base == "" || base == "." || base == ".." || strings.Trim(base, "-") == "" || a.isReserved(base) {
		base = "session"
	}
	name := base
	for i := 2; ; i++ {
		if _, taken := a.store.SessionByName(name); !taken && a.onPeer(name) == "" && !a.isReserved(name) {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// projectFor finds the project of the git repository that holds dir, by its remote, else its root
// commit, and creates it when it is new. The new project takes the repository folder's name. It
// also returns the repository's top folder.
func (a *actions) projectFor(ctx context.Context, dir string) (id, top string, err error) {
	top, remote, root, err := gitIdentity(ctx, dir)
	if err != nil {
		return "", "", err
	}
	if pr, ok := a.store.ProjectByIdentity(remote, root); ok {
		return pr.ID, top, nil
	}
	pr, err := a.store.CreateProject(filepath.Base(top), remote, root)
	if errors.Is(err, structure.ErrConflict) { // created meanwhile by another call
		if pr, ok := a.store.ProjectByIdentity(remote, root); ok {
			return pr.ID, top, nil
		}
	}
	if err != nil {
		return "", "", storeErr(err)
	}
	return pr.ID, top, nil
}

// gitIdentity reads the identity of the repository that holds dir: its top folder, the remote
// (origin, else the first remote) and, with no remote, the root commit.
func gitIdentity(ctx context.Context, dir string) (top, remote, root string, err error) {
	top, err = git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %s is not inside a git repository", dir)
	}
	if remote, err = git(ctx, dir, "remote", "get-url", "origin"); err != nil {
		remote = ""
		if list, err := git(ctx, dir, "remote"); err == nil && list != "" {
			first, _, _ := strings.Cut(list, "\n")
			remote, _ = git(ctx, dir, "remote", "get-url", first)
		}
	}
	if remote != "" {
		return top, remote, "", nil
	}
	roots, err := git(ctx, dir, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || roots == "" {
		return "", "", "", control.Errorf(api.ErrorKindInvalid, "project_dir %s: the repository has no remote and no commit", dir)
	}
	list := strings.Fields(roots)
	slices.Sort(list) // a repository with several roots: pick one the same way every time
	return top, "", list[0], nil
}

// git runs git in dir and returns its trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// mergeEnv returns base with each KEY=value of extra added; an extra entry replaces the base
// entry of the same key.
func mergeEnv(base, extra []string) ([]string, error) {
	keys := map[string]bool{}
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, control.Errorf(api.ErrorKindInvalid, "env entry %q is not KEY=value", kv)
		}
		keys[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...), nil
}

// parseUUID reads a pane id: 32 hex digits, with or without the dashes.
func parseUUID(s string) (hp.UUID, bool) {
	var u hp.UUID
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != len(u) {
		return u, false
	}
	copy(u[:], b)
	return u, true
}

// storeErr maps a structure store error to its API kind.
func storeErr(err error) error {
	for _, k := range []struct {
		err  error
		kind api.ErrorKind
	}{
		{structure.ErrNotFound, api.ErrorKindNotFound},
		{structure.ErrConflict, api.ErrorKindConflict},
		{structure.ErrInvalid, api.ErrorKindInvalid},
	} {
		if errors.Is(err, k.err) {
			return control.Errorf(k.kind, "%v", err)
		}
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", err)
}
