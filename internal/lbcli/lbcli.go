// Package lbcli is the Logbook CLI, `gff lb` (docs/specs/logbook.md 10.2). It reaches the hab
// through the pane's route when it runs in a pane (GFF_HAB), else through the control socket.
// When gff-hab is down it opens logbook.db in the hab's state directory itself and runs the same
// verbs with the same package (4.3), so memory keeps working through a hab restart or update. A
// write from a pane never takes that path: it waits for gff-hab (10.5).
package lbcli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habits"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// Env is what the CLI reads from its process. Tests fill it.
type Env struct {
	Args   []string // after the lb verb word
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Client api.ClientInfo // the hello's client, for the control socket
	Name   string         // the command name in messages: "gff lb"
	// Dir is the working folder: in a pane, the file's reads hide the private projects but the
	// one it belongs to (logbook.md 12.1). Empty: the process's.
	Dir string
}

// Caller makes one call to the hab, or to the file.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

const usage = `usage: %[1]s [--json] [--socket path] [--state dir] <verb> [args]

  brief [--project p]             the session brief: what to know about the project now
  find <words> [filters]          search every project: best match first
  ls [filters] [--all]            list records: id, type, status, title
  ready [--project p] [--all]     open tasks and issues with no open blocker
  show <id>                       the record, its links both ways, notes and flags
  history <id>                    every op on the record, losers included
  new <type> <title> [--body text|-] [--tag t]... [--link kind:id]... [--project p]
      [--status s] [--assignee a] [--priority n] [--component c] [--propose]
  edit <id> [--title t] [--body text|-] [--tag t]... [--untag t]... [--status s]
      [--assignee a] [--priority n] [--component c] [--project p]
  note <id> <text|->              append a note
  mark <id> <status>, done <id>   set a status
  claim <id>                      assignee: the calling session; status doing
  link <id> <kind> <id>, unlink <id> <kind> <id>
  supersede <id> <title> [--body text|-]
  delete <id>, restore <id>
  forget <op id> [--yes]          redact a value everywhere; asks first
  conflicts                       records with a flag
  resolve <id> <field> <value or op id>
  proposals [filters]             proposals: pending ones unless --status names another
  approve <id> [--title t] [--body text|-] [--changes json|-]
                                  apply a proposal's draft, or the draft edited: --title
                                  and --body change its first new record, --changes
                                  replaces its changes (a JSON list, as logbook.write's)
  reject <id>                     decide a proposal: nothing it holds lands
  projects                        projects, their identities, and which are private or joined
  project join <a> <b>            make two projects one: b's identities join a's; the older id
                                  stays, and the records of both show under it
  project rename <p> <name>       rename a project
  project private <p> [on|off]    hide a project from agents outside it, or show it again
  status                          replica size, version vector, waiting ops

filters: --type t --status s --project p --tag t --host h --author a --session s
         --since <RFC 3339 time> --general --flagged --deleted --limit n
brief, ls and ready cover the current project unless --project, --general or
--all says otherwise: in a pane the pane's session's, else the working folder's.
In a pane the hab records the pane's harness, session and host as the writer.
When gff-hab is down, reads open logbook.db in the state directory
(--state, else $GFF_HAB_STATE, else the default) and say so; so do writes
outside a pane. A write from a pane waits up to 15 s for gff-hab, then fails.
`

// later names the verbs of later stages (logbook.md 18).
var later = map[string]string{
	"export": "export comes with Logbook stage 9",
	"import": "import comes with Logbook stage 9",
}

// bools are the flags that take no value.
var bools = map[string]bool{"all": true, "json": true, "general": true, "deleted": true, "yes": true, "propose": true, "flagged": true, "help": true, "h": true}

// args is one command line: flags anywhere, values in order, the rest positional.
type args struct {
	flags map[string][]string
	pos   []string
}

func parse(in []string) (args, error) {
	a := args{flags: map[string][]string{}}
	for i := 0; i < len(in); i++ {
		s := in[i]
		if s == "--" {
			a.pos = append(a.pos, in[i+1:]...)
			break
		}
		if !strings.HasPrefix(s, "-") || s == "-" {
			a.pos = append(a.pos, s)
			continue
		}
		name := strings.TrimLeft(s, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		if bools[name] {
			if hasVal && val != "true" && val != "false" {
				return a, fmt.Errorf("--%s takes no value", name)
			}
			if !hasVal || val == "true" {
				a.flags[name] = append(a.flags[name], "true")
			}
			continue
		}
		if !hasVal {
			if i+1 >= len(in) {
				return a, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = in[i]
		}
		a.flags[name] = append(a.flags[name], val)
	}
	return a, nil
}

func (a args) str(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (a args) has(name string) bool { return len(a.flags[name]) > 0 }

// allow refuses a flag the verb does not take.
func (a args) allow(verb string, names ...string) error {
	for k := range a.flags {
		if !slices.Contains(names, k) && !slices.Contains([]string{"json", "socket", "state"}, k) {
			return fmt.Errorf("%s takes no --%s", verb, k)
		}
	}
	return nil
}

var filterFlags = []string{"type", "status", "project", "tag", "host", "author", "session", "since", "general", "deleted", "limit", "flagged"}

// Main runs the CLI and returns its exit code: 0, 1 on an error, 3 on an API error, 64 on a
// usage error.
func Main(env Env) int {
	err := run(env)
	if err == nil {
		return 0
	}
	fmt.Fprintf(env.Stderr, "%s: %v\n", env.Name, err)
	var ue usageError
	var ce *control.Error
	switch {
	case errors.As(err, &ue):
		return 64
	case errors.As(err, &ce):
		return 3
	}
	return 1
}

type usageError struct{ error }

func run(env Env) error {
	a, err := parse(env.Args)
	if err != nil {
		return usageError{err}
	}
	if a.has("help") || a.has("h") || len(a.pos) == 0 {
		fmt.Fprintf(env.Stdout, usage, env.Name)
		if len(a.pos) == 0 && !a.has("help") && !a.has("h") {
			return usageError{errors.New("needs a verb")}
		}
		return nil
	}
	verb := a.pos[0]
	a.pos = a.pos[1:]
	if why, ok := later[verb]; ok {
		return fmt.Errorf("%s: not built yet: %s", verb, why)
	}
	cmd, ok := verbs[verb]
	if !ok {
		return usageError{fmt.Errorf("no verb %q; %s --help lists them", verb, env.Name)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d := &door{env: env, socket: a.str("socket"), state: a.str("state")}
	defer d.close()
	res, text, err := cmd(ctx, &cli{env: env, a: a, d: d, verb: verb})
	if err != nil {
		return err
	}
	if d.local != nil {
		fmt.Fprintf(env.Stderr, "%s: gff-hab is not running; used %s directly\n", env.Name, d.path)
	}
	if a.has("json") {
		b, err := json.Marshal(api.NonNil(res))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "%s\n", b)
		return err
	}
	_, err = io.WriteString(env.Stdout, text)
	return err
}

// --- reaching the hab ---

// door picks how calls reach Logbook: the pane route, the control socket, or the file.
type door struct {
	env     Env
	socket  string
	state   string
	remote  Caller
	closer  func()
	local   *lbsvc.Service
	store   *logbook.Store
	path    string
	picked  bool
	inPane  bool  // $GFF_HAB is set
	pane    bool  // calls go through the pane route
	pickErr error // a $GFF_HAB that does not parse
}

func (d *door) close() {
	if d.closer != nil {
		d.closer()
	}
	if d.store != nil {
		d.store.Close()
	}
}

type hookCaller struct{ addr string }

func (h hookCaller) Call(ctx context.Context, method string, params, result any) error {
	return hook.Call(ctx, h.addr, method, params, result)
}

// writeMethods change Logbook. In a pane they never reach the file (10.5).
var writeMethods = []string{"logbook.write", "logbook.resolve", "logbook.decide"}

func isWrite(method string) bool { return slices.Contains(writeMethods, method) }

// paneRetry is how long a write from a pane waits for gff-hab to answer, and paneRetryEvery how
// often it tries: a gff-hab restart is short, and the holder keeps the pane running (10.5).
var (
	paneRetry      = 15 * time.Second
	paneRetryEvery = 250 * time.Millisecond
)

// Call sends one call to the hab. When the socket does not answer (nothing listens, or no socket
// is there), a read opens the file and runs there; so does a write outside a pane, as a person. A
// write from a pane never touches the file: it tries the hab again for up to paneRetry, then
// fails. Any other error, such as an HTTP status from the pane route, is the hab's answer and is
// returned as it is. A write carries a key, so one the hab took before it went away is not
// applied again from the file.
func (d *door) Call(ctx context.Context, method string, params, result any) error {
	if err := d.pick(); err != nil {
		return err
	}
	if d.pane && isWrite(method) {
		return d.paneWrite(ctx, method, params, result)
	}
	if d.local == nil {
		err := d.callRemote(ctx, method, params, result)
		if err == nil || !unreachable(err) {
			return err
		}
		if err := d.openLocal(ctx, err); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var pn *lbsvc.Pane
	if d.inPane {
		// A pane reads the file only while gff-hab is down: it still sees no private project
		// but its own (12.1).
		pn = &lbsvc.Pane{Project: d.paneProject(ctx)}
	}
	res, err := d.local.Dispatch(ctx, d.actor(), pn, method, raw)
	if err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}

// paneWrite sends a pane's write to the hab, trying again while the hab's socket does not
// answer, for up to paneRetry.
func (d *door) paneWrite(ctx context.Context, method string, params, result any) error {
	deadline := time.Now().Add(paneRetry)
	for {
		err := d.callRemote(ctx, method, params, result)
		if err == nil || !unreachable(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("gff-hab did not answer for %s (%v): nothing was written. A write from a pane goes only through gff-hab, never to the file; run it again once gff-hab runs", paneRetry, errors.Unwrap(err))
		}
		t := time.NewTimer(paneRetryEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// pick chooses the door once: the pane route when $GFF_HAB is set, else the control socket. In a
// pane, --socket reaches the control socket for reads only: a write there would land as a
// person, so it is refused, as confirm answer refuses (10.5). A $GFF_HAB that does not parse is
// an error, never a reason to use the file.
func (d *door) pick() error {
	if d.picked {
		return nil
	}
	d.picked = true
	addr := d.env.Getenv(hook.Env)
	if addr == "" {
		return nil
	}
	d.inPane = true
	if _, _, err := hook.ParseAddr(addr); err != nil {
		d.pickErr = fmt.Errorf("$%s: %w", hook.Env, err)
		return d.pickErr
	}
	if d.socket == "" {
		d.remote, d.pane = hookCaller{addr}, true
	}
	return nil
}

func (d *door) callRemote(ctx context.Context, method string, params, result any) error {
	if d.pickErr != nil {
		return d.pickErr
	}
	if d.inPane && !d.pane && isWrite(method) {
		return fmt.Errorf("%s from a pane goes through $%s, not --socket: over the control socket it would land as a person. Run it without --socket", method, hook.Env)
	}
	if d.remote == nil {
		path := d.socket
		if path == "" {
			p, err := control.DefaultSocket(runtime.GOOS, d.env.Getenv, os.UserConfigDir)
			if err != nil {
				// No socket path: no gff-hab can listen at the default one.
				return unreachableError{err}
			}
			path = p
		}
		c, err := control.Dial(path)
		if err != nil {
			if dialFailed(err) {
				return unreachableError{err}
			}
			return err
		}
		if _, err := c.Hello(ctx, d.env.Client, "", ""); err != nil {
			c.Close()
			return fmt.Errorf("gff-hab at %s: %w", path, err)
		}
		d.remote, d.closer = c, func() { c.Close() }
	}
	err := d.remote.Call(ctx, method, params, result)
	var ce *control.Error
	var se *hook.StatusError
	switch {
	case err == nil, errors.As(err, &ce):
		return err
	case errors.As(err, &se):
		return control.Errorf(api.ErrorKindUnavailable, "%v", se)
	case d.pane && dialFailed(err):
		return unreachableError{err}
	}
	return err
}

// dialFailed reports whether err is a socket that does not answer: nothing listens on it, or
// there is no socket file. Only then is the hab down; any other error is an answer.
func dialFailed(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

type unreachableError struct{ error }

func (e unreachableError) Unwrap() error { return e.error }

func unreachable(err error) bool {
	var u unreachableError
	return errors.As(err, &u)
}

// openLocal opens the hab's file. It never makes one: a hab that never ran has no origin to
// write as.
func (d *door) openLocal(ctx context.Context, why error) error {
	dir, err := d.stateDir()
	if err != nil {
		return fmt.Errorf("gff-hab is not reachable (%v), and the state directory is unknown: %w", why, err)
	}
	d.path = filepath.Join(dir, "logbook.db")
	if _, err := os.Stat(d.path); err != nil {
		return fmt.Errorf("gff-hab is not reachable (%v), and there is no Logbook file at %s: start gff-hab once to make it, or name its state directory with --state", why, d.path)
	}
	st, err := logbook.Open(ctx, d.path, logbook.Options{})
	if err != nil {
		return fmt.Errorf("gff-hab is not reachable (%v), and %s does not open: %w", why, d.path, err)
	}
	svc, err := lbsvc.New(ctx, lbsvc.Config{Store: st})
	if err != nil {
		st.Close()
		return err
	}
	d.store, d.local = st, svc
	return nil
}

// stateDir is the hab's state directory: --state, else $GFF_HAB_STATE, else the default.
func (d *door) stateDir() (string, error) {
	if d.state != "" {
		return d.state, nil
	}
	if dir := d.env.Getenv("GFF_HAB_STATE"); dir != "" {
		return dir, nil
	}
	return control.DefaultState()
}

// paneProject is the project of the working folder's repository, by its identity (3.1), or ""
// when it has none on this replica.
func (d *door) paneProject(ctx context.Context) string {
	id, _ := d.paneProjectIdentity(ctx)
	return id
}

// paneProjectIdentity is paneProject and the folder's identity.
func (d *door) paneProjectIdentity(ctx context.Context) (string, string) {
	identity := folderIdentity(ctx, d.env.Dir)
	if identity == "" {
		return "", ""
	}
	id, _ := d.local.Lookup(ctx, identity)
	return id, identity
}

// inPaneEnv reports whether the CLI runs in a pane: $GFF_HAB is set.
func (d *door) inPaneEnv() bool { return d.env.Getenv(hook.Env) != "" }

// actor is who writes through the file: a person at this host's shell. A pane never writes the
// file (Call), so the actor is never an agent.
func (d *door) actor() api.LogbookActor {
	host := d.env.Getenv("GFF_HAB_HOST_ID")
	if host == "" {
		host, _ = os.Hostname()
	}
	return api.LogbookActor{Kind: "person", Device: "cli", Host: host}
}

// --- verbs ---

type cli struct {
	env  Env
	a    args
	d    *door
	verb string
}

type verbFunc func(ctx context.Context, c *cli) (any, string, error)

var verbs map[string]verbFunc

func init() {
	verbs = map[string]verbFunc{
		"brief": brief, "find": find, "ls": list, "ready": ready, "show": show, "history": history,
		"new": newRecord, "edit": edit, "note": note, "mark": mark, "done": done, "claim": claim,
		"link": link, "unlink": link, "supersede": supersede, "delete": del, "restore": del,
		"forget": forget, "conflicts": conflicts, "resolve": resolve, "proposals": proposals,
		"approve": decide, "reject": decide, "projects": projects, "project": project, "status": status,
	}
}

func (c *cli) need(verb string, n int, what string) error {
	if len(c.a.pos) < n {
		return usageError{fmt.Errorf("%s needs %s", verb, what)}
	}
	return nil
}

func (c *cli) filter() (api.LogbookFilter, error) {
	f := api.LogbookFilter{Type: c.a.str("type"), Status: c.a.str("status"), Project: c.a.str("project"), Tag: c.a.str("tag"),
		Host: c.a.str("host"), Author: c.a.str("author"), Session: c.a.str("session"), Since: c.a.str("since"),
		General: c.a.has("general"), Deleted: c.a.has("deleted"), Flagged: c.a.has("flagged")}
	if s := c.a.str("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return f, usageError{errors.New("--limit is a positive number")}
		}
		f.Limit = n
	}
	return f, nil
}

func lines(r api.LogbookRecords) string {
	var b strings.Builder
	for _, x := range r.Records {
		st := x.Status
		if st == "" {
			st = "-"
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s", x.ID, x.Type, st, x.Title)
		if x.Deleted {
			b.WriteString("  (deleted)")
		}
		b.WriteString("\n")
	}
	if len(r.Records) == 0 {
		b.WriteString("no records\n")
	}
	if r.More {
		b.WriteString("more: narrow the filters or raise --limit\n")
	}
	return b.String()
}

// brief prints the session brief (11.2) of the current project, or of --project.
func brief(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("brief", "project"); err != nil {
		return nil, "", usageError{err}
	}
	p := api.LogbookBriefParams{Project: c.a.str("project")}
	if p.Project == "" {
		var err error
		if p.Project, err = c.current(ctx); err != nil {
			return nil, "", err
		}
	}
	var r api.LogbookBrief
	if err := c.d.Call(ctx, "logbook.brief", p, &r); err != nil {
		return nil, "", err
	}
	return r, r.Text, nil
}

// current is the current project outside a pane: the project that holds the working folder's
// identity (10.2), or "" when it has none. In a pane it is "": the hab takes the pane's
// session's project, and the file's reads take the working folder's (Call).
func (c *cli) current(ctx context.Context) (string, error) {
	if c.d.inPaneEnv() {
		return "", nil
	}
	identity := folderIdentity(ctx, c.env.Dir)
	if identity == "" {
		return "", nil
	}
	var r api.LogbookProjects
	if err := c.d.Call(ctx, "logbook.projects", api.LogbookProjectsParams{}, &r); err != nil {
		return "", err
	}
	for _, p := range r.Projects {
		if slices.Contains(p.Identities, identity) {
			if p.JoinedInto != "" {
				return p.JoinedInto, nil
			}
			return p.ID, nil
		}
	}
	return "", nil
}

// folderIdentity is the identity of dir's repository (3.1), or "" (dir empty: the process's).
func folderIdentity(ctx context.Context, dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir == "" {
		return ""
	}
	f, ok := catalog.ReadFolder(ctx, dir)
	if !ok {
		return ""
	}
	return lbsvc.Identity(f.Remote, f.RootCommit)
}

func find(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("find", filterFlags...); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("find", 1, "words"); err != nil {
		return nil, "", err
	}
	f, err := c.filter()
	if err != nil {
		return nil, "", err
	}
	p := api.LogbookFindParams{Words: strings.Join(c.a.pos, " "), Type: f.Type, Status: f.Status, Project: f.Project, Tag: f.Tag,
		Host: f.Host, Author: f.Author, Since: f.Since, Limit: f.Limit, Session: f.Session, General: f.General, Deleted: f.Deleted, Flagged: f.Flagged}
	var r api.LogbookRecords
	if err := c.d.Call(ctx, "logbook.find", p, &r); err != nil {
		return nil, "", err
	}
	return r, lines(r), nil
}

func list(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("ls", append(filterFlags, "all")...); err != nil {
		return nil, "", usageError{err}
	}
	f, err := c.filter()
	if err != nil {
		return nil, "", err
	}
	f.All = c.a.has("all")
	if lbsvc.DefaultProject(f.Project, f.General, f.All) {
		if f.Project, err = c.current(ctx); err != nil {
			return nil, "", err
		}
	}
	return c.listWith(ctx, f)
}

func (c *cli) listWith(ctx context.Context, f api.LogbookFilter) (any, string, error) {
	var r api.LogbookRecords
	if err := c.d.Call(ctx, "logbook.list", api.LogbookListParams{LogbookFilter: f}, &r); err != nil {
		return nil, "", err
	}
	return r, lines(r), nil
}

func ready(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("ready", "project", "limit", "all"); err != nil {
		return nil, "", usageError{err}
	}
	f, err := c.filter()
	if err != nil {
		return nil, "", err
	}
	all := c.a.has("all")
	if lbsvc.DefaultProject(f.Project, false, all) {
		if f.Project, err = c.current(ctx); err != nil {
			return nil, "", err
		}
	}
	var r api.LogbookRecords
	if err := c.d.Call(ctx, "logbook.ready", api.LogbookReadyParams{Project: f.Project, Limit: f.Limit, All: all}, &r); err != nil {
		return nil, "", err
	}
	return r, lines(r), nil
}

func conflicts(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("conflicts", "project", "limit"); err != nil {
		return nil, "", usageError{err}
	}
	f, err := c.filter()
	if err != nil {
		return nil, "", err
	}
	f.Flagged = true
	f.All = f.Project == ""
	return c.listWith(ctx, f)
}

func proposals(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("proposals", "status", "project", "host", "author", "session", "since", "flagged", "deleted", "limit"); err != nil {
		return nil, "", usageError{err}
	}
	f, err := c.filter()
	if err != nil {
		return nil, "", err
	}
	f.Type = logbook.TypeProposal
	if f.Status == "" {
		f.Status = "pending"
	}
	f.All = f.Project == "" && !f.General
	return c.listWith(ctx, f)
}

// decide approves or rejects a proposal (9.2). Only a person decides: in a pane the hab refuses
// it, and it never reaches the file there.
func decide(ctx context.Context, c *cli) (any, string, error) {
	verb := c.verb
	names := []string{}
	if verb == "approve" {
		names = []string{"title", "body", "changes"}
	}
	if err := c.a.allow(verb, names...); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need(verb, 1, "a proposal id"); err != nil {
		return nil, "", err
	}
	if len(c.a.pos) > 1 {
		return nil, "", usageError{fmt.Errorf("%s takes one proposal id", verb)}
	}
	var k [16]byte
	_, _ = rand.Read(k[:])
	p := api.LogbookDecideParams{ID: c.a.pos[0], Decision: api.LogbookDecisionReject, IdempotencyKey: "cli-" + hex.EncodeToString(k[:])}
	if verb == "approve" {
		p.Decision = api.LogbookDecisionApprove
		edits, err := c.edits(ctx, p.ID)
		if err != nil {
			return nil, "", err
		}
		p.Edits = edits
	}
	var r api.LogbookWritten
	if err := c.d.Call(ctx, "logbook.decide", p, &r); err != nil {
		return nil, "", err
	}
	if r.State == api.LogbookWriteStateRejected {
		return r, fmt.Sprintf("rejected: %s\n", p.ID), nil
	}
	var made []string
	for _, id := range r.IDs {
		if id != p.ID {
			made = append(made, id)
		}
	}
	text := fmt.Sprintf("approved: %s\n", p.ID)
	if len(made) > 0 {
		text += strings.Join(made, "\n") + "\n"
	}
	return r, text, nil
}

// edits reads approve's edit flags: --changes replaces the draft's changes; --title and --body
// change the first create in the draft, which edits reads from the proposal. None: nil, the draft
// as it is.
func (c *cli) edits(ctx context.Context, id string) ([]api.LogbookChange, error) {
	if c.a.has("changes") {
		if c.a.has("title") || c.a.has("body") {
			return nil, usageError{errors.New("--changes replaces the draft whole: give it or --title and --body, not both")}
		}
		v, err := c.text(c.a.str("changes"))
		if err != nil {
			return nil, err
		}
		var chs []api.LogbookChange
		if err := json.Unmarshal([]byte(v), &chs); err != nil || len(chs) == 0 {
			return nil, usageError{fmt.Errorf("--changes is a JSON list of changes, as logbook.write takes: %v", err)}
		}
		return chs, nil
	}
	if !c.a.has("title") && !c.a.has("body") {
		return nil, nil
	}
	var r api.LogbookRecordResult
	if err := c.d.Call(ctx, "logbook.get", api.LogbookGetParams{ID: id}, &r); err != nil {
		return nil, err
	}
	var draft struct {
		Changes []api.LogbookChange `json:"changes"`
	}
	if err := json.Unmarshal(r.Record.Draft, &draft); err != nil {
		return nil, fmt.Errorf("%s's draft does not read as changes: %v", id, err)
	}
	i := slices.IndexFunc(draft.Changes, func(ch api.LogbookChange) bool { return ch.Op == api.LogbookChangeOpCreate })
	if i < 0 {
		return nil, usageError{fmt.Errorf("%s's draft makes no record: --title and --body edit a new record; use --changes", id)}
	}
	if c.a.has("title") {
		draft.Changes[i].Title = c.a.str("title")
	}
	if c.a.has("body") {
		b, err := c.text(c.a.str("body"))
		if err != nil {
			return nil, err
		}
		draft.Changes[i].Body = b
	}
	return draft.Changes, nil
}

func who(a *api.LogbookActor) string {
	if a == nil {
		return "unknown"
	}
	var parts []string
	session := a.Session // an op written before session_name shows the id
	if a.SessionName != "" {
		session = a.SessionName
	}
	for _, p := range []string{a.Kind, a.Harness, session, a.Device, a.Host} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if a.ApprovedBy != "" {
		parts = append(parts, "approved by "+a.ApprovedBy)
	}
	return strings.Join(parts, " ")
}

func show(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("show"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("show", 1, "a record id"); err != nil {
		return nil, "", err
	}
	var r api.LogbookRecordResult
	if err := c.d.Call(ctx, "logbook.get", api.LogbookGetParams{ID: c.a.pos[0]}, &r); err != nil {
		return nil, "", err
	}
	x := r.Record
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", x.ID, x.Type)
	if x.Status != "" {
		fmt.Fprintf(&b, "  %s", x.Status)
	}
	fmt.Fprintf(&b, "\n%s\n", x.Title)
	field := func(name, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, v)
		}
	}
	field("project", x.Project)
	field("tags", strings.Join(x.Tags, ", "))
	field("assignee", x.Assignee)
	if x.Priority > 0 {
		field("priority", strconv.Itoa(x.Priority))
	}
	field("component", x.Component)
	field("target", x.Target)
	field("decided by", x.DecidedBy)
	field("expires", x.Expires)
	field("created", x.Created+" by "+who(x.CreatedBy))
	field("updated", x.Updated)
	if x.Source != nil {
		field("source", strings.TrimSpace(x.Source.Session+" "+x.Source.Message))
		field("quote", x.Source.Quote)
	}
	if x.Deleted {
		b.WriteString("deleted\n")
	}
	for _, s := range x.SupersededBy {
		field("superseded by", s)
	}
	for _, l := range x.Links {
		here := ""
		if !l.Here {
			here = " (not here yet)"
		}
		fmt.Fprintf(&b, "link: %s %s%s\n", l.Kind, l.To, here)
	}
	for _, l := range x.Backlinks {
		fmt.Fprintf(&b, "linked from: %s %s\n", l.To, l.Kind)
	}
	for _, f := range x.Flags {
		fmt.Fprintf(&b, "flag: %s %s\n", f.Kind, f.Field)
		for _, v := range f.Values {
			fmt.Fprintf(&b, "  %s %s %s by %s\n", v.Op, v.At, v.Value, who(v.By))
		}
	}
	if x.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(x.Body, "\n"))
	}
	if len(x.Draft) > 0 {
		fmt.Fprintf(&b, "\ndraft: %s\n", x.Draft)
	}
	for _, n := range x.Notes {
		fmt.Fprintf(&b, "\n%s %s:\n%s\n", n.At, who(n.By), strings.TrimRight(n.Text, "\n"))
	}
	return r, b.String(), nil
}

func history(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("history"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("history", 1, "a record id"); err != nil {
		return nil, "", err
	}
	var r api.LogbookHistory
	if err := c.d.Call(ctx, "logbook.history", api.LogbookHistoryParams{ID: c.a.pos[0]}, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, o := range r.Ops {
		fmt.Fprintf(&b, "%s  %s  %s %s", o.ID, o.At, o.Kind, o.Field)
		if len(o.Value) > 0 && len(o.Value) < 200 {
			fmt.Fprintf(&b, " %s", o.Value)
		}
		if o.Target != "" {
			fmt.Fprintf(&b, " %s", o.Target)
		}
		if !o.Folded {
			b.WriteString(" (not folded by this build)")
		}
		fmt.Fprintf(&b, "  by %s\n", who(o.By))
	}
	return r, b.String(), nil
}

// text reads a value that may be - for stdin.
func (c *cli) text(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(io.LimitReader(c.env.Stdin, logbook.MaxBody+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// write sends one batch with a fresh key.
func (c *cli) write(ctx context.Context, p api.LogbookWriteParams) (api.LogbookWritten, string, error) {
	var k [16]byte
	_, _ = rand.Read(k[:])
	p.IdempotencyKey = "cli-" + hex.EncodeToString(k[:])
	var r api.LogbookWritten
	if err := c.d.Call(ctx, "logbook.write", p, &r); err != nil {
		return r, "", err
	}
	if r.State == api.LogbookWriteStateProposed {
		if r.Note != "" {
			return r, fmt.Sprintf("proposed: %s\n%s\n", r.Proposal, r.Note), nil
		}
		return r, fmt.Sprintf("proposed: %s waits for review\n", r.Proposal), nil
	}
	return r, strings.Join(r.IDs, "\n") + "\n", nil
}

func parseLink(s string) (*api.LogbookLink, error) {
	kind, to, ok := strings.Cut(s, ":")
	if !ok || kind == "" || to == "" {
		return nil, usageError{fmt.Errorf("--link %q: want kind:id, such as blocks:task-k3m9q2xz", s)}
	}
	return &api.LogbookLink{Kind: kind, To: to}, nil
}

func newRecord(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("new", "body", "tag", "link", "project", "status", "assignee", "priority", "component", "propose"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("new", 2, "a type and a title"); err != nil {
		return nil, "", err
	}
	body, err := c.text(c.a.str("body"))
	if err != nil {
		return nil, "", err
	}
	ch := api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: c.a.pos[0], Title: strings.Join(c.a.pos[1:], " "), Body: body,
		Project: c.a.str("project"), Status: c.a.str("status"), Assignee: c.a.str("assignee"), Component: c.a.str("component"),
		Tags: c.a.flags["tag"]}
	if s := c.a.str("priority"); s != "" {
		if ch.Priority, err = strconv.Atoi(s); err != nil {
			return nil, "", usageError{errors.New("--priority is 1 to 3")}
		}
	}
	for _, l := range c.a.flags["link"] {
		ln, err := parseLink(l)
		if err != nil {
			return nil, "", err
		}
		ch.Links = append(ch.Links, *ln)
	}
	return c.write(ctx, api.LogbookWriteParams{Propose: c.a.has("propose"), Changes: []api.LogbookChange{ch}})
}

func setTo(id, field string, v any) api.LogbookChange {
	b, _ := json.Marshal(v)
	return api.LogbookChange{Op: api.LogbookChangeOpSet, ID: id, Field: field, Value: api.RawJSON(b)}
}

func edit(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("edit", "title", "body", "tag", "untag", "status", "assignee", "priority", "component", "project"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("edit", 1, "a record id"); err != nil {
		return nil, "", err
	}
	id := c.a.pos[0]
	var chs []api.LogbookChange
	for _, f := range []string{"title", "status", "assignee", "component", "project"} {
		if c.a.has(f) {
			chs = append(chs, setTo(id, f, c.a.str(f)))
		}
	}
	if c.a.has("body") {
		body, err := c.text(c.a.str("body"))
		if err != nil {
			return nil, "", err
		}
		chs = append(chs, setTo(id, "body", body))
	}
	if s := c.a.str("priority"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, "", usageError{errors.New("--priority is 1 to 3, or 0 for none")}
		}
		chs = append(chs, setTo(id, "priority", n))
	}
	for _, t := range c.a.flags["tag"] {
		chs = append(chs, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: id, Tag: t})
	}
	for _, t := range c.a.flags["untag"] {
		chs = append(chs, api.LogbookChange{Op: api.LogbookChangeOpRemove, ID: id, Tag: t})
	}
	if len(chs) == 0 {
		return nil, "", usageError{errors.New("edit needs a change: --title, --body, --tag, --untag, --status and the like")}
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: chs})
}

func note(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("note"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("note", 2, "a record id and the text"); err != nil {
		return nil, "", err
	}
	t, err := c.text(strings.Join(c.a.pos[1:], " "))
	if err != nil {
		return nil, "", err
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: c.a.pos[0], Text: t}}})
}

func mark(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("mark"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("mark", 2, "a record id and a status"); err != nil {
		return nil, "", err
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{setTo(c.a.pos[0], "status", c.a.pos[1])}})
}

func done(ctx context.Context, c *cli) (any, string, error) {
	if err := c.need("done", 1, "a record id"); err != nil {
		return nil, "", err
	}
	c.a.pos = []string{c.a.pos[0], "done"}
	return mark(ctx, c)
}

func claim(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("claim"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("claim", 1, "a record id"); err != nil {
		return nil, "", err
	}
	id := c.a.pos[0]
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{setTo(id, "assignee", lbsvc.Self), setTo(id, "status", "doing")}})
}

func link(ctx context.Context, c *cli) (any, string, error) {
	verb := c.verb
	op := api.LogbookChangeOpAdd
	if verb == "unlink" {
		op = api.LogbookChangeOpRemove
	}
	if err := c.a.allow(verb); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need(verb, 3, "a record id, a link kind and the target id"); err != nil {
		return nil, "", err
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: op, ID: c.a.pos[0], Link: &api.LogbookLink{Kind: c.a.pos[1], To: c.a.pos[2]}}}})
}

func supersede(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("supersede", "body", "tag", "project"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("supersede", 2, "the finding's id and the new title"); err != nil {
		return nil, "", err
	}
	body, err := c.text(c.a.str("body"))
	if err != nil {
		return nil, "", err
	}
	ch := api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: logbook.TypeFinding, Title: strings.Join(c.a.pos[1:], " "), Body: body,
		Project: c.a.str("project"), Tags: c.a.flags["tag"], Links: []api.LogbookLink{{Kind: logbook.LinkSupersedes, To: c.a.pos[0]}}}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{ch}})
}

func del(ctx context.Context, c *cli) (any, string, error) {
	verb := c.verb
	if err := c.a.allow(verb); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need(verb, 1, "a record id"); err != nil {
		return nil, "", err
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{setTo(c.a.pos[0], "deleted", verb == "delete")}})
}

func forget(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("forget", "yes"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("forget", 1, "an op id (seq@origin, from history)"); err != nil {
		return nil, "", err
	}
	op := c.a.pos[0]
	if !c.a.has("yes") {
		fmt.Fprintf(c.env.Stderr, "Forget the value of op %s on every replica? Older backups keep it. [y/N] ", op)
		line, _ := bufio.NewReader(c.env.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, "", errors.New("nothing was forgotten")
		}
	}
	return c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpForget, OpID: op}}})
}

func resolve(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("resolve"); err != nil {
		return nil, "", usageError{err}
	}
	if err := c.need("resolve", 3, "a record id, a field and a value or an op id"); err != nil {
		return nil, "", err
	}
	var k [16]byte
	_, _ = rand.Read(k[:])
	p := api.LogbookResolveParams{ID: c.a.pos[0], Field: c.a.pos[1], IdempotencyKey: "cli-" + hex.EncodeToString(k[:])}
	v := strings.Join(c.a.pos[2:], " ")
	if _, err := logbook.ParseOpID(v); err == nil {
		p.Op = v
	} else if json.Valid([]byte(v)) && !strings.HasPrefix(v, "\"") && v != "null" {
		p.Value = api.RawJSON(v)
	} else {
		b, _ := json.Marshal(v)
		p.Value = api.RawJSON(b)
	}
	var r api.LogbookWritten
	if err := c.d.Call(ctx, "logbook.resolve", p, &r); err != nil {
		return nil, "", err
	}
	return r, strings.Join(r.IDs, "\n") + "\n", nil
}

func projects(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("projects"); err != nil {
		return nil, "", usageError{err}
	}
	var r api.LogbookProjects
	if err := c.d.Call(ctx, "logbook.projects", api.LogbookProjectsParams{}, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, p := range r.Projects {
		fmt.Fprintf(&b, "%s  %s  %s", p.ID, p.Name, strings.Join(p.Identities, ", "))
		if p.Private {
			b.WriteString("  private")
		}
		if p.JoinedInto != "" {
			fmt.Fprintf(&b, "  joined into %s", p.JoinedInto)
		}
		b.WriteByte('\n')
	}
	if len(r.Projects) == 0 {
		b.WriteString("no projects\n")
	}
	return r, b.String(), nil
}

// project runs the project verbs (logbook.md 3.3): join, rename, private.
func project(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("project"); err != nil {
		return nil, "", usageError{err}
	}
	if len(c.a.pos) == 0 {
		return nil, "", usageError{errors.New("project needs join, rename or private")}
	}
	sub, rest := c.a.pos[0], c.a.pos[1:]
	get := func(id string) (api.LogbookRecord, error) {
		var r api.LogbookRecordResult
		if err := c.d.Call(ctx, "logbook.get", api.LogbookGetParams{ID: id}, &r); err != nil {
			return r.Record, err
		}
		if r.Record.Type != logbook.TypeProject {
			return r.Record, fmt.Errorf("%s is a %s, not a project", id, r.Record.Type)
		}
		return r.Record, nil
	}
	shown := func(r api.LogbookRecord) string {
		if r.JoinedInto != "" {
			return r.JoinedInto
		}
		return r.ID
	}
	switch sub {
	case "join":
		if len(rest) != 2 {
			return nil, "", usageError{errors.New("project join needs two projects")}
		}
		a, err := get(rest[0])
		if err != nil {
			return nil, "", err
		}
		b, err := get(rest[1])
		if err != nil {
			return nil, "", err
		}
		if shown(a) == shown(b) {
			return nil, "", fmt.Errorf("%s and %s are one project already, shown as %s", a.ID, b.ID, shown(a))
		}
		// b's identities join a's set, and both get one identity of their own, so projects with
		// no identity join too. The fold joins projects that share one (5.6).
		var chs []api.LogbookChange
		for _, id := range append(slices.Clone(b.Identities), "project:"+b.ID) {
			chs = append(chs, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: a.ID, Identity: id})
		}
		chs = append(chs, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: b.ID, Identity: "project:" + b.ID})
		w, _, err := c.write(ctx, api.LogbookWriteParams{Changes: chs})
		if err != nil {
			return nil, "", err
		}
		if w.State == api.LogbookWriteStateProposed {
			return w, fmt.Sprintf("proposed: %s waits for review\n", w.Proposal), nil
		}
		after, err := get(a.ID)
		if err != nil {
			return nil, "", err
		}
		return w, fmt.Sprintf("joined: %s and %s are one project, shown as %s\n", a.ID, b.ID, shown(after)), nil
	case "rename":
		if len(rest) < 2 {
			return nil, "", usageError{errors.New("project rename needs a project and a name")}
		}
		if _, err := get(rest[0]); err != nil {
			return nil, "", err
		}
		w, text, err := c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{setTo(rest[0], "title", strings.Join(rest[1:], " "))}})
		return w, text, err
	case "private":
		if len(rest) < 1 || len(rest) > 2 || (len(rest) == 2 && rest[1] != "on" && rest[1] != "off") {
			return nil, "", usageError{errors.New("project private needs a project, then on (the default) or off")}
		}
		p, err := get(rest[0])
		if err != nil {
			return nil, "", err
		}
		if len(rest) == 1 || rest[1] == "on" {
			w, _, err := c.write(ctx, api.LogbookWriteParams{Changes: []api.LogbookChange{setTo(p.ID, "private", true)}})
			if err != nil {
				return nil, "", err
			}
			return w, fmt.Sprintf("private: %s is hidden from agents outside it\n", shown(p)), nil
		}
		// Off clears the mark on every project joined with it: one mark keeps the group private.
		var all api.LogbookProjects
		if err := c.d.Call(ctx, "logbook.projects", api.LogbookProjectsParams{}, &all); err != nil {
			return nil, "", err
		}
		var chs []api.LogbookChange
		for _, x := range all.Projects {
			if x.Marked && (x.ID == shown(p) || x.JoinedInto == shown(p)) {
				chs = append(chs, setTo(x.ID, "private", false))
			}
		}
		if len(chs) == 0 {
			return all, fmt.Sprintf("%s is not private\n", shown(p)), nil
		}
		w, _, err := c.write(ctx, api.LogbookWriteParams{Changes: chs})
		if err != nil {
			return nil, "", err
		}
		return w, fmt.Sprintf("not private: agents read %s again\n", shown(p)), nil
	}
	return nil, "", usageError{fmt.Errorf("no project verb %q: join, rename or private", sub)}
}

func status(ctx context.Context, c *cli) (any, string, error) {
	if err := c.a.allow("status"); err != nil {
		return nil, "", usageError{err}
	}
	var r api.LogbookStatus
	if err := c.d.Call(ctx, "logbook.status", api.LogbookStatusParams{}, &r); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "origin %s\n%d records, %d ops\n", r.Origin, r.Records, r.Ops)
	if r.Waiting > 0 {
		fmt.Fprintf(&b, "%d ops wait behind a gap or an incomplete batch\n", r.Waiting)
	}
	if r.Unfolded > 0 {
		fmt.Fprintf(&b, "%d ops kept for a newer build\n", r.Unfolded)
	}
	for _, o := range r.Ahead {
		fmt.Fprintf(&b, "clock ahead: %s\n", o)
	}
	for _, o := range r.Shared {
		fmt.Fprintf(&b, "shared origin: %s\n", o)
	}
	for _, v := range r.Vector {
		fmt.Fprintf(&b, "  %s  %d\n", v.Origin, v.Seq)
	}
	return r, b.String(), nil
}

// FileBrief builds a pane's brief from the hab's file, for `gff-hab hook` when gff-hab does not
// answer (11.3): the state directory from $GFF_HAB_STATE (env.Getenv), the project from the
// working folder env.Dir, private projects but that one left out (12.1). The brief honours the
// switches gff-hab would (habits.On): logbook.habits.brief from <state>/settings.json under the
// [settings] locks of the hab's config file configPath, and that file's [logbook.habits_off]; a
// missing or broken file counts as none. Turned off, the brief is empty. It never writes.
func FileBrief(ctx context.Context, env Env, configPath string) (api.LogbookBrief, error) {
	d := &door{env: env, inPane: true, picked: true}
	defer d.close()
	dir, err := d.stateDir()
	if err != nil {
		return api.LogbookBrief{}, err
	}
	value := settings.Read(dir, configPath)
	if !habits.On(habits.Brief, value, nil) {
		return api.LogbookBrief{}, nil // off everywhere: the file is not opened
	}
	if err := d.openLocal(ctx, errors.New("gff-hab did not answer")); err != nil {
		return api.LogbookBrief{}, err
	}
	project, identity := d.paneProjectIdentity(ctx)
	off, err := habits.LoadOff(configPath)
	if err != nil {
		off = nil
	}
	if !habits.On(habits.Brief, value, off, project, identity) {
		return api.LogbookBrief{}, nil
	}
	res, err := d.local.Dispatch(ctx, d.actor(), &lbsvc.Pane{Project: project}, "logbook.brief", nil)
	if err != nil {
		return api.LogbookBrief{}, err
	}
	return res.(api.LogbookBrief), nil
}
