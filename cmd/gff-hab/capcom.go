package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// capcomFolders is the folder under the state directory that holds one folder per Capcom
// session (capcom.md 7.1).
const capcomFolders = "capcom"

// capcomService is the Capcom session host (capcom.md 7): the standing conversation and new
// ones, each in its own hab-owned folder, their resume, and the home host assignment that the
// catalog carries.
type capcomService struct {
	self       string // this hab's device id
	host       string // this hab's host label, as the read tools name it
	state      string // the state directory: capcom.json, and the folders under capcom/
	configPath string // the hab's config file, with its [capcom] section
	exe        string // gff-hab's own resolved path, for the MCP server entry
	acts       *actions
	store      *structure.Store
	// links returns the peer links this hab dialed; copies the peer catalogs it holds. Nil:
	// none.
	links  func() []catalog.Link
	copies func() []api.Catalog
	// native returns the harness's own session id that a pane's agent state reports.
	native func(pane string) string
	// claudeTranscript reports whether Claude Code wrote a transcript for a session id. It
	// writes none before the first message, and --resume of such an id fails. Nil: assume it did.
	claudeTranscript func(id string) bool
	// complete reports whether the link has reached every registered holder. Nil: it has.
	complete func() bool
	poke     func() // the catalog is built again
	scope    func() capcom.ReadScope
	res      capcomResolvers // what attached context reads (capcom.md 8.1)
	now      func() time.Time
	logf     func(string, ...any)

	op sync.Mutex // one open, resume or assign at a time

	mu sync.Mutex // guards st
	st capcom.State
}

// newCapcomService loads capcom.json, drops entries whose session record is gone, and writes
// each remaining folder's manual and settings again.
func newCapcomService(c *capcomService) *capcomService {
	if c.now == nil {
		c.now = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	if c.poke == nil {
		c.poke = func() {}
	}
	st, err := capcom.LoadState(c.state)
	if err != nil {
		c.logf("capcom: %v", err)
	}
	// A store that could not load its state started empty: pruning against it would wipe every
	// record a later good start still needs.
	if c.store.Health().Problem == "" {
		kept := make([]capcom.Session, 0, len(st.Sessions))
		for _, rec := range st.Sessions {
			if _, ok := c.store.Session(rec.ID); ok {
				kept = append(kept, rec)
			}
		}
		pruned := len(kept) != len(st.Sessions)
		st.Sessions = kept
		if _, ok := c.store.Session(st.Standing); !ok && st.Standing != "" {
			st.Standing, pruned = "", true
		}
		c.st = st
		if pruned {
			c.saveLocked()
		}
	} else {
		c.st = st
		c.logf("capcom: the structure state did not load; capcom.json is kept as it is")
	}
	// Each folder's settings name this gff-hab's own path. An install retires the build an
	// earlier gff-hab ran from (macOS), so every folder gets the current path at start: a
	// plain-terminal resume and an MCP reconnect keep the tools.
	for _, rec := range st.Sessions {
		if fi, err := os.Stat(rec.Dir); err != nil || !fi.IsDir() {
			continue
		}
		if err := capcom.WriteFolder(rec.Dir, rec.Harness, c.exe, c.readScope()); err != nil {
			c.logf("capcom: rewrite %s: %v", rec.Dir, err)
		}
	}
	return c
}

var _ control.Capcom = (*capcomService)(nil)

// saveLocked writes capcom.json. The caller holds c.mu.
func (c *capcomService) saveLocked() {
	if err := c.trySaveLocked(); err != nil {
		c.logf("capcom: save: %v", err)
	}
}

// trySaveLocked writes capcom.json and returns the error. The caller holds c.mu.
func (c *capcomService) trySaveLocked() error { return capcom.SaveState(c.state, c.st) }

// adoptLocked takes b when it beats the belief held. A home host that sees a later assignment
// drops its mark, and its standing conversation stays resumable in the list: the next open on
// the new home starts a fresh one (capcom.md 7.3). The caller holds c.mu. It reports whether b
// was taken.
func (c *capcomService) adoptLocked(b capcom.Belief) bool {
	cur := c.st.Belief()
	if !b.Beats(cur) {
		return false
	}
	wasHome := cur.Home == c.self
	c.st.Assignment, c.st.Home = b.Assignment, b.Home
	if wasHome && b.Home != c.self {
		c.st.Standing = ""
		c.logf("capcom: assignment %d makes %s the home host; this host drops its mark", b.Assignment, b.Home)
	} else if !wasHome && b.Home == c.self {
		c.st.Standing = "" // a new home starts a fresh standing conversation
	}
	c.saveLocked()
	return true
}

// Observe hears a peer's catalog: a higher assignment there replaces the one held.
func (c *capcomService) Observe(cat api.Catalog) {
	if cat.Capcom == nil {
		return
	}
	c.mu.Lock()
	took := c.adoptLocked(capcom.Belief{Assignment: cat.Capcom.Assignment, Home: cat.Capcom.Home})
	c.mu.Unlock()
	if took {
		c.poke()
	}
}

// Home returns the device id of the host this hab believes runs Capcom, or "" when none does.
func (c *capcomService) Home() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Home
}

// maxSeen is the highest assignment counter in the belief held and in every peer copy.
func (c *capcomService) maxSeen() uint64 {
	var copies []api.Catalog
	if c.copies != nil {
		copies = c.copies()
	}
	c.mu.Lock()
	n := c.st.Assignment
	c.mu.Unlock()
	for _, cat := range copies {
		if cat.Capcom != nil && cat.Capcom.Assignment > n {
			n = cat.Capcom.Assignment
		}
	}
	return n
}

// Section is the catalog's capcom section: the belief, and each Capcom session on this host
// whose record still exists. It also keeps each session's resume id up to date from its
// pane's agent state.
func (c *capcomService) Section() *api.CatalogCapcom {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.Assignment == 0 && len(c.st.Sessions) == 0 {
		return nil
	}
	out := &api.CatalogCapcom{Assignment: c.st.Assignment, Home: c.st.Home, Sessions: []api.CatalogCapcomSession{}}
	dirty := false
	for i := range c.st.Sessions {
		rec := &c.st.Sessions[i]
		ss, ok := c.store.Session(rec.ID)
		if !ok {
			continue
		}
		if rec.Harness != capcom.Claude && ss.Pane != "" && c.native != nil {
			if id := c.native(ss.Pane); id != "" && id != rec.Resume {
				rec.Resume, dirty = id, true
			}
		}
		out.Sessions = append(out.Sessions, api.CatalogCapcomSession{
			Session: rec.ID, Name: ss.Name, Harness: rec.Harness, Standing: rec.ID == c.st.Standing,
		})
	}
	if dirty {
		c.saveLocked()
	}
	return out
}

// Open answers capcom.open: the standing conversation, started when there is none, resumed when
// it exited; new starts a fresh one, which becomes the standing conversation. Context is
// attached to the returned session (capcom.md 8.1). Every item is resolved, and the count with
// what already waits is checked, before the open, so a refused open starts nothing. A file is
// refused: its folder or attachment belongs to the session, so it is attached with
// capcom.attach after the open.
func (c *capcomService) Open(ctx context.Context, p api.CapcomOpenParams) (api.CapcomOpened, error) {
	if len(p.Context) > capcom.MaxPending {
		return api.CapcomOpened{}, control.Errorf(api.ErrorKindInvalid, "capcom.open takes at most %d context items, not %d", capcom.MaxPending, len(p.Context))
	}
	items := make([]capcom.Item, 0, len(p.Context))
	for i, r := range p.Context {
		if string(r.Kind) == capcom.KindFile {
			return api.CapcomOpened{}, control.Errorf(api.ErrorKindInvalid,
				"context item %d: capcom.open takes no file; attach files with capcom.attach after the open", i+1)
		}
		it, err := c.resolve(ctx, capcom.Session{}, r)
		if err != nil {
			return api.CapcomOpened{}, prefixErr(err, "context item %d", i+1)
		}
		items = append(items, it)
	}
	// One open at a time from the count check to the attach: a concurrent open cannot fill the
	// list between them. Another attach still can; addPending then refuses after the open.
	c.op.Lock()
	defer c.op.Unlock()
	if len(items) > 0 && !p.New {
		// The standing conversation's list, with the new items after its own.
		c.mu.Lock()
		var waiting []capcom.Item
		if rec, ok := c.recordLocked(c.st.Standing); ok {
			waiting = rec.Pending
		}
		c.mu.Unlock()
		for _, it := range items {
			var err error
			if waiting, err = capcom.Add(waiting, it); err != nil {
				return api.CapcomOpened{}, control.Errorf(api.ErrorKindConflict, "the standing Capcom conversation, before any open: %v", err)
			}
		}
	}
	res, err := c.open(ctx, p)
	if err != nil {
		return res, err
	}
	pending, err := c.addPending(res.Session, items)
	if err != nil {
		return api.CapcomOpened{}, prefixErr(err, "Capcom session %s is open, but its context was not attached", res.Session)
	}
	if len(pending) > 0 {
		res.Pending = pending
	}
	return res, nil
}

// prefixErr puts a prefix before an error's message and keeps its kind.
func prefixErr(err error, format string, args ...any) error {
	var ce *control.Error
	kind, msg := api.ErrorKindUnavailable, err.Error()
	if errors.As(err, &ce) {
		kind, msg = ce.Data.Kind, ce.Message
	}
	return control.Errorf(kind, "%s: %s", fmt.Sprintf(format, args...), msg)
}

// open opens the standing conversation or a new one. The caller holds c.op.
func (c *capcomService) open(ctx context.Context, p api.CapcomOpenParams) (api.CapcomOpened, error) {
	c.mu.Lock()
	b, standing := c.st.Belief(), c.st.Standing
	rec, _ := c.recordLocked(standing)
	c.mu.Unlock()
	switch {
	case b.Home == "":
		return api.CapcomOpened{}, control.Errorf(api.ErrorKindUnavailable,
			"no host runs Capcom yet; assign one with gff-hab capcom assign <peer label|self>")
	case b.Home != c.self:
		return api.CapcomOpened{}, control.Errorf(api.ErrorKindUnavailable,
			"Capcom runs on %s (assignment %d), not on this host", c.hostName(b.Home), b.Assignment)
	}
	if !p.New && rec != nil {
		if ss, ok := c.store.Session(rec.ID); ok {
			if ss.State == api.SessionStateRunning {
				return api.CapcomOpened{Session: ss.ID, Pane: ss.Pane, Dir: rec.Dir}, nil
			}
			pane, err := c.resume(ctx, *rec, ss)
			if err != nil {
				return api.CapcomOpened{}, err
			}
			return api.CapcomOpened{Session: ss.ID, Pane: pane, Started: true, Dir: rec.Dir}, nil
		}
	}
	return c.start(ctx)
}

// start makes a new folder, writes the manual and settings, and starts the configured harness
// there as a new session of kind capcom, which becomes the standing conversation.
func (c *capcomService) start(ctx context.Context) (api.CapcomOpened, error) {
	cfg, problems, err := capcom.LoadConfig(c.configPath)
	for _, pr := range problems {
		c.logf("capcom: %s", pr)
	}
	if err != nil {
		c.logf("capcom: %v; starting %s", err, cfg.Harness)
	}
	dir := filepath.Join(c.state, capcomFolders, c.now().UTC().Format("20060102-150405")+"-"+randHex(3))
	if err := capcom.WriteFolder(dir, cfg.Harness, c.exe, c.readScope()); err != nil {
		return api.CapcomOpened{}, control.Errorf(api.ErrorKindUnavailable, "capcom folder: %v", err)
	}
	resume := ""
	if cfg.Harness == capcom.Claude {
		resume = newUUID().String()
	}
	st, err := capcom.StartFor(cfg.Harness, resume)
	if err != nil {
		return api.CapcomOpened{}, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	params := api.SessionCreateParams{Harness: st.Harness, Argv: st.Argv, Env: st.Env, Cwd: dir}
	var id, pane string
	for try := 0; ; try++ {
		params.Name = c.acts.freeName("capcom")
		id, pane, err = c.acts.createSession(ctx, params, string(api.SessionKindCapcom), dir)
		var ce *control.Error
		if err == nil || try == freeNameRetries || !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
			break
		}
	}
	if err != nil {
		return api.CapcomOpened{}, err
	}
	c.mu.Lock()
	c.st.Sessions = append(c.st.Sessions, capcom.Session{ID: id, Dir: dir, Harness: cfg.Harness, Resume: resume, Created: c.now().UTC()})
	c.st.Standing = id
	c.saveLocked()
	c.mu.Unlock()
	c.poke()
	return api.CapcomOpened{Session: id, Pane: pane, Started: true, Dir: dir}, nil
}

// Resume serves session.resume for a Capcom session. ok is false for any other session.
func (c *capcomService) Resume(ctx context.Context, session string) (pane string, ok bool, err error) {
	c.op.Lock()
	defer c.op.Unlock()
	c.mu.Lock()
	rec, ok := c.recordLocked(session)
	c.mu.Unlock()
	if !ok {
		return "", false, nil
	}
	ss, found := c.store.Session(session)
	if !found {
		return "", true, control.Errorf(api.ErrorKindNotFound, "session %s does not exist", session)
	}
	pane, err = c.resume(ctx, *rec, ss)
	return pane, true, err
}

// resume rewrites the folder's manual and settings, releases the old pane when it is still
// there, and starts the harness again with the session's resume id. The caller holds c.op.
func (c *capcomService) resume(ctx context.Context, rec capcom.Session, ss api.Session) (string, error) {
	if ss.State != api.SessionStateExited {
		return "", control.Errorf(api.ErrorKindConflict, "session %s is running", ss.ID)
	}
	lost := control.Errorf(api.ErrorKindUnavailable, "session %s: its holder is not connected; its pane may still run", ss.ID)
	if ss.Pane != "" && ss.Exit == nil && c.complete != nil && !c.complete() {
		// Exited with no status while a holder is not reached yet: the harness may still run
		// there, and a second one would share the conversation. With every holder reached, the
		// pane is gone for good (a reboot, a holder that died) and the resume goes ahead.
		return "", lost
	}
	if err := capcom.WriteFolder(rec.Dir, rec.Harness, c.exe, c.readScope()); err != nil {
		return "", control.Errorf(api.ErrorKindUnavailable, "capcom folder: %v", err)
	}
	resumeID := rec.Resume
	if rec.Harness == capcom.Claude && resumeID != "" && c.claudeTranscript != nil && !c.claudeTranscript(resumeID) {
		resumeID = "" // no transcript yet: start again under the same id
	}
	var st capcom.Start
	var err error
	if rec.Harness == capcom.Claude && resumeID == "" && rec.Resume != "" {
		st, err = capcom.StartFor(capcom.Claude, rec.Resume)
	} else {
		st, err = capcom.ResumeFor(rec.Harness, resumeID)
	}
	if err != nil {
		return "", control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	if ss.Pane != "" {
		// An exited pane the holder still keeps: release it, so the session's new pane is its
		// only one. A pane already gone answers not_found; any other answer stops the resume.
		if err := c.acts.ClosePane(ctx, ss.Pane); err != nil {
			var ce *control.Error
			switch {
			case errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound:
			case errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindUnavailable && ss.Exit == nil:
				return "", lost // the link still follows the pane as lost
			default:
				return "", err
			}
		}
	}
	return c.acts.resumeSession(ctx, ss.ID, api.SessionCreateParams{Harness: st.Harness, Argv: st.Argv, Env: st.Env}, rec.Dir)
}

// Assign answers capcom.assign. Self takes the mark with a counter above every one seen. A
// peer gets the assignment forwarded over this hab's link to it, with that counter as the
// floor; this hab then holds the peer's answer as its own belief.
func (c *capcomService) Assign(ctx context.Context, p api.CapcomAssignParams) (api.CapcomAssigned, error) {
	c.op.Lock()
	defer c.op.Unlock()
	floor := max(p.Assignment, c.maxSeen()+1)
	if p.Host == "self" || p.Host == c.self {
		// A host that is home already keeps its standing conversation (adoptLocked); the
		// counter still rises, so every peer agrees on the latest assignment.
		c.mu.Lock()
		c.adoptLocked(capcom.Belief{Assignment: floor, Home: c.self})
		c.mu.Unlock()
		c.poke()
		return api.CapcomAssigned{Home: c.self, Assignment: floor}, nil
	}
	var link *catalog.Link
	if c.links != nil {
		for _, l := range c.links() {
			if l.Label == p.Host || (l.DeviceID != "" && l.DeviceID == p.Host) {
				l := l
				link = &l
				break
			}
		}
	}
	switch {
	case link == nil:
		return api.CapcomAssigned{}, control.Errorf(api.ErrorKindNotFound, "no peer %q in this hab's peer list", p.Host)
	case !link.Online || link.Caller == nil:
		return api.CapcomAssigned{}, control.Errorf(api.ErrorKindUnavailable, "peer %s is offline; assign Capcom to it when it is online", link.Label)
	}
	var res api.CapcomAssigned
	if err := link.Caller.Call(ctx, "capcom.assign", api.CapcomAssignParams{Host: "self", Assignment: floor}, &res); err != nil {
		return api.CapcomAssigned{}, control.Errorf(api.ErrorKindUnavailable, "peer %s: %v", link.Label, err)
	}
	if res.Home == "" || res.Assignment < floor {
		return api.CapcomAssigned{}, control.Errorf(api.ErrorKindUnavailable, "peer %s answered assignment %d for %q", link.Label, res.Assignment, res.Home)
	}
	c.mu.Lock()
	c.adoptLocked(capcom.Belief{Assignment: res.Assignment, Home: res.Home})
	c.mu.Unlock()
	c.poke()
	res.Seq = api.Seq{}
	return res, nil
}

// recordLocked finds a Capcom session record by id. The caller holds c.mu.
func (c *capcomService) recordLocked(id string) (*capcom.Session, bool) {
	if id == "" {
		return nil, false
	}
	for i := range c.st.Sessions {
		if c.st.Sessions[i].ID == id {
			rec := c.st.Sessions[i]
			return &rec, true
		}
	}
	return nil, false
}

// hostName is a peer's label when this hab has a link to it, else its device id.
func (c *capcomService) hostName(device string) string {
	if c.links != nil {
		for _, l := range c.links() {
			if l.DeviceID == device && l.Label != "" {
				return l.Label
			}
		}
	}
	return device
}

func (c *capcomService) readScope() capcom.ReadScope {
	if c.scope == nil {
		return capcom.ReadScope{}
	}
	return c.scope()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// capcomCmd is `gff-hab capcom assign <peer label|self>`, `gff-hab capcom open [-new]` and
// `gff-hab capcom attach|detach [-host h] <session> <kind> <ref>`.
func capcomCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "assign" && args[0] != "open" && args[0] != "attach" && args[0] != "detach") {
		return errors.New("usage: gff-hab capcom assign [-socket path | -via command] <peer label|self>, gff-hab capcom open [-socket path | -via command] [-new], or gff-hab capcom attach|detach [-socket path | -via command] [-host h] <session> <kind> <ref>")
	}
	verb := args[0]
	fs := flag.NewFlagSet("capcom "+verb, flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "a command whose stdin and stdout reach a hab, such as: ssh host gff-hab connect")
	fresh := fs.Bool("new", false, "open: start a fresh Capcom conversation")
	host := fs.String("host", "", "attach, detach: the host a notification was raised on (a peer label); absent: this hab")
	fs.Parse(args[1:])
	switch {
	case verb == "assign" && fs.NArg() != 1:
		return errors.New("capcom assign needs one host: a peer label, a device id, or self")
	case verb == "open" && fs.NArg() != 0:
		return errors.New("capcom open takes no arguments")
	case (verb == "attach" || verb == "detach") && fs.NArg() != 3:
		return errors.New("capcom " + verb + " needs a session, a kind (notification, session, file, command, logbook) and a ref")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	if verb == "attach" || verb == "detach" {
		var r api.CapcomAttached
		var err error
		if verb == "attach" {
			r, err = cl.CapcomAttach(ctx, api.CapcomAttachParams{Session: fs.Arg(0), Kind: api.CapcomContextKind(fs.Arg(1)), Ref: fs.Arg(2), Host: *host})
		} else {
			r, err = cl.CapcomDetach(ctx, api.CapcomDetachParams{Session: fs.Arg(0), Kind: api.CapcomContextKind(fs.Arg(1)), Ref: fs.Arg(2), Host: *host})
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%d waiting for the next message to %s:\n", len(r.Pending), r.Session)
		for _, it := range r.Pending {
			fmt.Fprintf(out, "%s %s on %s: %s\n", it.Kind, it.Ref, it.Host, it.Title)
		}
		return nil
	}
	if verb == "assign" {
		r, err := cl.CapcomAssign(ctx, api.CapcomAssignParams{Host: fs.Arg(0)})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Capcom home host: %s (assignment %d)\n", r.Home, r.Assignment)
		return nil
	}
	r, err := cl.CapcomOpen(ctx, api.CapcomOpenParams{New: *fresh})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "session %s\npane %s\nfolder %s\n", r.Session, r.Pane, r.Dir)
	return nil
}

// osExecutable is os.Executable with links resolved, so the MCP entry names the real file.
func osExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}
