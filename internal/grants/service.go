package grants

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// HolderInfo is one running holder, from the holder registry.
type HolderInfo struct {
	Instance string
	Pid      int
	Draining bool
	// Bundle is the GFF Holder.app the holder runs from; "" when it is not in a bundle.
	Bundle string
}

// Job is one launchd job of the install, for the background item check.
type Job struct {
	Label    string
	Process  api.PermissionProcess
	Instance string // for a holder job
}

// ProbeFunc runs a probe pane on the current holder with these checks and request. It returns
// the holder instance the pane ran in. An error that wraps ErrProbeStart means the pane did not
// start in time; the check then cannot run (7.6, exit 2).
type ProbeFunc func(ctx context.Context, checks []string, request string) (instance string, res []Check, err error)

// ErrProbeStart means the probe pane did not start.
var ErrProbeStart = errors.New("the probe pane did not start")

// Config is what a Service needs. Every func may be nil, which skips what it feeds.
type Config struct {
	GOOS      string
	OSVersion string
	// Probe runs a probe pane on the current holder. Nil: no holder link, the holder rows say
	// the check could not run.
	Probe ProbeFunc
	// Holders lists the running holders.
	Holders func() []HolderInfo
	// HabBundle is gff-hab's own GFF Hab.app; "" when it does not run from a bundle.
	HabBundle string
	Run       Runner
	// Sys runs gff-hab's own checks (Full Disk Access, domain).
	Sys System
	// Grants is the opt-in list (macos.grants): accessibility, screen-recording,
	// developer-tools, pasteboard and automation:<id> turn grants on; no-full-disk-access drops
	// the holder's Full Disk Access for per-folder grants (Q1).
	Grants []string
	// ProjectDirs lists the projects' folders, for gff-hab's own Full Disk Access (4.1).
	ProjectDirs func() []string
	// Jobs lists the launchd jobs; nil when gff-hab runs with no init system.
	Jobs func() []Job
	// UID is the user's id, for gui/<uid>.
	UID int
	// LogicUnit is the systemd unit of gff-hab; "" when not installed with systemd.
	LogicUnit string
	// StateDir is gff-hab's state directory. The records go to <state>/macos (7.7).
	StateDir string
	Now      func() time.Time
}

// Service runs the permission check. One check runs at a time.
type Service struct {
	cfg Config
	sem chan struct{}
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sys == nil {
		cfg.Sys = NewSystem()
	}
	return &Service{cfg: cfg, sem: make(chan struct{}, 1)}
}

// InvalidError is a request the check refuses.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string { return e.msg }

// optIn reports whether g is turned on in the Grants list.
func (s *Service) optIn(g string) bool { return slices.Contains(s.cfg.Grants, g) }

// fdaDropped reports whether the user dropped the holder's Full Disk Access (Q1).
func (s *Service) fdaDropped() bool { return s.optIn("no-" + FullDiskAccess) }

// holderGrants is every holder grant the check covers, in output order, and whether each is
// required.
func (s *Service) holderGrants() (names []string, required map[string]bool) {
	required = map[string]bool{Domain: true, LocalNetwork: true}
	names = []string{Domain, FullDiskAccess}
	if s.fdaDropped() {
		names = append(names, FilesAndFolders)
		required[FilesAndFolders] = true
	} else {
		required[FullDiskAccess] = true
	}
	names = append(names, LocalNetwork)
	for _, g := range []string{Accessibility, ScreenRecording, DeveloperTools, Pasteboard} {
		names = append(names, g)
		required[g] = s.optIn(g) && g != Pasteboard
	}
	for _, g := range s.cfg.Grants {
		if strings.HasPrefix(g, AutomationPrefix) && Known(g) {
			names = append(names, g)
			required[g] = true
		}
	}
	return names, required
}

// Check runs hab.permissions.
func (s *Service) Check(ctx context.Context, p api.PermissionsParams) (api.Permissions, error) {
	for _, g := range p.Grants {
		if !Known(g) {
			return api.Permissions{}, &InvalidError{fmt.Sprintf("unknown grant %q", g)}
		}
	}
	if p.Request != "" && !Requestable(p.Request) {
		return api.Permissions{}, &InvalidError{fmt.Sprintf("%q cannot be requested", p.Request)}
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return api.Permissions{}, ctx.Err()
	}
	want := func(g string) bool {
		return len(p.Grants) == 0 || slices.Contains(p.Grants, g) || g == p.Request
	}
	res := api.Permissions{OS: s.cfg.GOOS, OSVersion: s.cfg.OSVersion}
	if s.cfg.GOOS != "darwin" {
		res.Rows = s.linux(ctx, want)
		return res, nil
	}
	res.Rows = s.darwin(ctx, p, want)
	if len(p.Grants) == 0 && p.Request == "" && s.fullPass(res.Rows) {
		// This check is the proof an owed re-grant waited for (6.4).
		res.Rows = slices.DeleteFunc(res.Rows, func(r api.PermissionRow) bool { return r.Grant == RegrantOwed })
	}
	return res, nil
}

func row(proc api.PermissionProcess, inst string, c Check, required bool) api.PermissionRow {
	r := api.PermissionRow{Process: proc, Instance: inst, Grant: c.Grant, State: c.State, Required: required, Detail: c.Detail}
	if c.State != api.PermissionStateGranted && c.State != api.PermissionStateNote {
		r.Settings = SettingsURL(c.Grant)
	}
	if r.Detail == "" && r.Settings != "" && (c.State == api.PermissionStateDenied || c.State == api.PermissionStateUnanswered) {
		place := MenuPath(c.Grant)
		r.Detail = "gff setup opens " + place[strings.LastIndex(place, "> ")+2:]
	}
	return r
}

func (s *Service) darwin(ctx context.Context, p api.PermissionsParams, want func(string) bool) []api.PermissionRow {
	var rows []api.PermissionRow
	holders := []HolderInfo{}
	if s.cfg.Holders != nil {
		holders = s.cfg.Holders()
	}

	// The holder: one probe pane on the current holder runs every check it can make.
	names, required := s.holderGrants()
	var probe []string
	for _, g := range names {
		if !want(g) {
			continue
		}
		on := required[g] || g == p.Request || (len(p.Grants) > 0 && slices.Contains(p.Grants, g))
		if !on && g != FullDiskAccess && g != Domain {
			continue // an opt-in grant that is off is reported without a probe
		}
		probe = append(probe, g)
	}
	current := ""
	byGrant := map[string]Check{}
	if len(probe) > 0 {
		switch {
		case s.cfg.Probe == nil:
			rows = append(rows, row(api.PermissionProcessHolder, "", Check{Grant: Domain, State: api.PermissionStateError,
				Detail: "gff-hab runs without its holder link, so no probe pane can start"}, true))
		default:
			inst, checks, err := s.cfg.Probe(ctx, probe, p.Request)
			if err != nil {
				rows = append(rows, row(api.PermissionProcessHolder, inst, Check{Grant: Domain, State: api.PermissionStateError,
					Detail: "probe: " + err.Error()}, true))
			} else {
				current = inst
				for _, c := range checks {
					byGrant[c.Grant] = c
				}
			}
		}
	}
	if current == "" {
		for _, h := range holders {
			if !h.Draining {
				current = h.Instance
				break
			}
		}
	}
	var cur *HolderInfo
	for i := range holders {
		if holders[i].Instance == current {
			cur = &holders[i]
		}
	}

	// The holder's signature: on its installed bundle, run by gff-hab.
	var curSig *SigInfo
	if cur != nil && cur.Bundle != "" && s.cfg.Run != nil && (want(Signature) || want(LocalNetwork) || want(FilesAndFolders)) {
		si := CheckSignature(ctx, s.cfg.Run, cur.Bundle)
		curSig = &si
	}
	if want(Signature) {
		switch {
		case curSig != nil:
			rows = append(rows, row(api.PermissionProcessHolder, current, curSig.Check, true))
			if curSig.AdHoc {
				rows = append(rows, row(api.PermissionProcessHolder, current, Check{Grant: DevBuild, State: api.PermissionStateNote,
					Detail: "dev build: grants last until the next rebuild"}, false))
			}
		case cur != nil:
			rows = append(rows, row(api.PermissionProcessHolder, current, Check{Grant: Signature, State: api.PermissionStateNote,
				Detail: "the holder does not run from GFF Holder.app; its grants follow its path and last until the next build"}, false))
		}
	}

	if len(byGrant) > 0 {
		for _, g := range names {
			if !want(g) {
				continue
			}
			c, probed := byGrant[g]
			if !probed {
				c = Check{Grant: g, State: api.PermissionStateOff, Detail: "not turned on in macos.grants"}
				if g == FullDiskAccess && s.fdaDropped() {
					c.Detail = "dropped in macos.grants; Files and Folders is checked instead"
				}
				rows = append(rows, row(api.PermissionProcessHolder, current, c, false))
				continue
			}
			if g == FullDiskAccess && s.fdaDropped() && c.State != api.PermissionStateGranted {
				c.State, c.Detail = api.PermissionStateOff, "dropped in macos.grants; Files and Folders is checked instead"
			}
			remembered := false
			if (g == LocalNetwork || g == FilesAndFolders) && curSig != nil {
				c, remembered = s.seen(api.PermissionProcessHolder, curSig.Requirement, c, g == p.Request)
			}
			r := row(api.PermissionProcessHolder, current, c, required[g])
			r.Remembered = remembered
			if g == FullDiskAccess && c.State != api.PermissionStateGranted && cur != nil && cur.Bundle != "" {
				r.Reveal = cur.Bundle
			}
			if g == Pasteboard && c.State == api.PermissionStateGranted && !required[g] {
				r.Detail = "a pane read back the clipboard"
			}
			rows = append(rows, r)
		}
	}

	// Draining holders take no new panes (hab-holder.md 4.3), so no probe runs in them. TCC keys
	// bundled code on its bundle id and requirement (6.2): one with the current holder's
	// requirement shares its grants.
	if want(Signature) {
		for _, h := range holders {
			if h.Instance == current {
				continue
			}
			rows = append(rows, s.otherHolder(ctx, h, curSig))
		}
	}

	// gff-hab itself.
	if want(Domain) {
		name, err := s.cfg.Sys.ManagerName(ctx)
		c := Check{Grant: Domain, State: api.PermissionStateGranted, Detail: "Aqua"}
		if err != nil {
			c.State, c.Detail = api.PermissionStateError, "launchctl managername: "+err.Error()
		} else if name != "Aqua" {
			c.State, c.Detail = api.PermissionStateError, fmt.Sprintf("launchd domain %q, not Aqua: gff-hab must run as a launch agent of a graphical login", name)
		}
		rows = append(rows, row(api.PermissionProcessHab, "", c, true))
	}
	if want(BackgroundItem) {
		rows = append(rows, s.background(ctx)...)
	}
	if want(Signature) && s.cfg.HabBundle != "" && s.cfg.Run != nil {
		si := CheckSignature(ctx, s.cfg.Run, s.cfg.HabBundle)
		rows = append(rows, row(api.PermissionProcessHab, "", si.Check, true))
		if si.AdHoc {
			rows = append(rows, row(api.PermissionProcessHab, "", Check{Grant: DevBuild, State: api.PermissionStateNote,
				Detail: "dev build: grants last until the next rebuild"}, false))
		}
	}
	if want(FullDiskAccess) {
		c := OpenCheck(ctx, s.cfg.Sys, FullDiskAccess, FDAPath, FDATimeout)
		need := s.protectedProject()
		if c.State != api.PermissionStateGranted {
			if need == "" {
				c.Detail = strings.TrimPrefix(c.Detail+"; ", "; ") + "not needed: no project is in a protected folder"
			} else {
				c.Detail = "a project is in " + need
			}
		}
		r := row(api.PermissionProcessHab, "", c, need != "")
		if c.State != api.PermissionStateGranted && s.cfg.HabBundle != "" {
			r.Reveal = s.cfg.HabBundle
		}
		rows = append(rows, r)
	}
	if want(RegrantOwed) {
		if b, err := os.ReadFile(s.regrantPath()); err == nil {
			rows = append(rows, row(api.PermissionProcessHab, "", Check{Grant: RegrantOwed, State: api.PermissionStateDenied,
				Detail: "the signing identity changed: " + firstLine(string(b)) + "; grant each row again, then check"}, true))
		}
	}
	return rows
}

// otherHolder is the row of a holder that is not the current one: draining, or a second
// holder of this build left from a retried start.
func (s *Service) otherHolder(ctx context.Context, h HolderInfo, cur *SigInfo) api.PermissionRow {
	c := Check{Grant: Signature}
	required := false
	switch {
	case h.Bundle == "" || s.cfg.Run == nil:
		c.State, c.Detail = api.PermissionStateNote, "not probed: it takes no new panes"
	default:
		si := CheckSignature(ctx, s.cfg.Run, h.Bundle)
		c = si.Check
		required = true
		switch {
		case si.State != api.PermissionStateGranted:
		case cur != nil && SameRequirement(si.Requirement, cur.Requirement):
			c.Detail = "same identity as the current holder, so it shares its grants; not probed: it takes no new panes"
		case cur != nil:
			c.State, c.Detail = api.PermissionStateDenied, "another signing identity than the current holder: its panes lack the grants made since; they run until they end"
		default:
			c.Detail += "; not probed: it takes no new panes"
		}
	}
	r := row(api.PermissionProcessHolder, h.Instance, c, required)
	r.Draining = h.Draining
	return r
}

// background checks that every launchd job is loaded and not switched off in Login Items.
func (s *Service) background(ctx context.Context) []api.PermissionRow {
	if s.cfg.Jobs == nil || s.cfg.Run == nil {
		return []api.PermissionRow{row(api.PermissionProcessHab, "", Check{Grant: BackgroundItem, State: api.PermissionStateNote,
			Detail: "gff-hab was not started by gff-hab install"}, false)}
	}
	domain := fmt.Sprintf("gui/%d", s.cfg.UID)
	disabled := map[string]bool{}
	if out, err := s.cfg.Run(ctx, "launchctl", "print-disabled", domain); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			l, v, ok := strings.Cut(strings.TrimSpace(line), "=>")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if v == "disabled" || v == "true" {
				disabled[strings.Trim(strings.TrimSpace(l), `"`)] = true
			}
		}
	}
	var rows []api.PermissionRow
	for _, j := range s.cfg.Jobs() {
		c := Check{Grant: BackgroundItem, State: api.PermissionStateGranted}
		if _, err := s.cfg.Run(ctx, "launchctl", "print", domain+"/"+j.Label); err != nil {
			c.State, c.Detail = api.PermissionStateDenied, "job "+j.Label+" is not loaded: allow it in Login Items"
		} else if disabled[j.Label] {
			c.State, c.Detail = api.PermissionStateDenied, "job "+j.Label+" is switched off in Login Items"
		}
		rows = append(rows, row(j.Process, j.Instance, c, true))
	}
	return rows
}

// protectedProject returns a project folder in a protected location, or "".
func (s *Service) protectedProject() string {
	if s.cfg.ProjectDirs == nil {
		return ""
	}
	home := s.cfg.Sys.Home()
	for _, d := range s.cfg.ProjectDirs() {
		if Protected(home, d) {
			return d
		}
	}
	return ""
}

// Protected reports whether dir lies in a location Files and Folders guards: Desktop,
// Documents, Downloads, iCloud Drive, or a volume under /Volumes.
func Protected(home, dir string) bool {
	dir = filepath.Clean(dir)
	under := func(base string) bool {
		return dir == base || strings.HasPrefix(dir, base+string(filepath.Separator))
	}
	if home != "" {
		for _, f := range append(slices.Clone(ProtectedFolders), filepath.Join("Library", "Mobile Documents")) {
			if under(filepath.Join(home, f)) {
				return true
			}
		}
	}
	return strings.HasPrefix(dir, "/Volumes/")
}

// linux checks lingering and the systemd unit (7.1).
func (s *Service) linux(ctx context.Context, want func(string) bool) []api.PermissionRow {
	var rows []api.PermissionRow
	if s.cfg.LogicUnit == "" || s.cfg.Run == nil {
		return append(rows, row(api.PermissionProcessHab, "", Check{Grant: Units, State: api.PermissionStateNote,
			Detail: "gff-hab was not started by gff-hab install"}, false))
	}
	if want(Lingering) {
		c := Check{Grant: Lingering, State: api.PermissionStateGranted}
		out, err := s.cfg.Run(ctx, "loginctl", "show-user", fmt.Sprint(s.cfg.UID), "--property=Linger")
		if err != nil || strings.TrimSpace(string(out)) != "Linger=yes" {
			c.State, c.Detail = api.PermissionStateDenied, "programs end at logout: run loginctl enable-linger"
		}
		rows = append(rows, row(api.PermissionProcessHab, "", c, true))
	}
	if want(Units) {
		c := Check{Grant: Units, State: api.PermissionStateGranted, Detail: s.cfg.LogicUnit}
		if out, err := s.cfg.Run(ctx, "systemctl", "--user", "is-enabled", s.cfg.LogicUnit); err != nil || strings.TrimSpace(string(out)) != "enabled" {
			c.State, c.Detail = api.PermissionStateDenied, s.cfg.LogicUnit+" is not enabled: run gff-hab install"
		}
		rows = append(rows, row(api.PermissionProcessHab, "", c, true))
	}
	return rows
}

// --- Records in <state>/macos (7.7) ---

func (s *Service) macosDir() string     { return filepath.Join(s.cfg.StateDir, "macos") }
func (s *Service) regrantPath() string  { return filepath.Join(s.macosDir(), RegrantFile) }
func (s *Service) lastPassPath() string { return filepath.Join(s.macosDir(), "last-pass") }

// RegrantFile is the re-grant record gff-hab install writes when the signing identity changes
// (6.4), under <state>/macos.
const RegrantFile = "regrant-owed"

// fullPass records a full check whose required rows are all granted: the time of the last full
// pass, and the end of an owed re-grant, whose proof is exactly that check (6.4).
func (s *Service) fullPass(rows []api.PermissionRow) bool {
	for _, r := range rows {
		if r.Required && r.Grant != RegrantOwed && r.State != api.PermissionStateGranted {
			return false
		}
	}
	if s.cfg.StateDir == "" {
		return true
	}
	if err := os.MkdirAll(s.macosDir(), 0o700); err != nil {
		return true
	}
	os.Remove(s.regrantPath())
	writeFileAtomic(s.lastPassPath(), []byte(s.cfg.Now().UTC().Format(time.RFC3339)+"\n"))
	return true
}

var writeMu sync.Mutex

// WriteFileAtomic writes a <state>/macos record through a temporary file (mode 0600) and a
// rename.
func WriteFileAtomic(path string, data []byte) error { return writeFileAtomic(path, data) }

func writeFileAtomic(path string, data []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}
