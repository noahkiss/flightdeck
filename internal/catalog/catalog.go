// Package catalog keeps a hab's catalog (docs/specs/mesh.md 5): what the hab publishes about its
// host, and the copies it holds of its direct peers' catalogs.
//
// The own catalog is built from detected facts (system, toolchains, harnesses), declared facts
// (the [catalog] section of the hab's config file), the projects found under the configured
// project roots and in the structure model, and the running agents. Any change raises one
// generation counter, which is saved across restarts. Over each peer link this hab dialed, it
// hands over its own catalog (catalog.put) and pulls the peer's (catalog.get with since), so a
// laptop that only dials out still sends and receives (mesh.md 4.2). A copy outlives its peer's
// link: it is shown stale, and dropped only when the peer leaves the peer list.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/merge"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Capability is the capability a hab with a catalog sends.
const Capability = "catalog"

// Defaults (mesh.md 4.2, 5.1, 8).
const (
	DefaultPull     = 15 * time.Second
	DefaultRescan   = 5 * time.Minute
	DefaultRedetect = time.Hour
	DefaultDebounce = time.Second
	callTimeout     = 10 * time.Second
	stateFile       = "catalog.json"
)

// Link is one peer link this hab dialed.
type Link struct {
	Label    string
	DeviceID string // the pinned id, else the one its welcome gave
	Online   bool
	Caps     []string
	Caller   api.Caller
}

// Peers is the peer list as the catalog needs it. gff-hab adapts internal/peers to it.
type Peers interface {
	// List is the peer list with each peer's state (peers.list).
	List() api.PeerList
	// Links is each peer's link from this hab.
	Links() []Link
}

// Config is what New needs. Self is required.
type Config struct {
	Self string // this hab's device id
	Host string // this hab's host id
	// State is the folder catalog.json goes in: the generation and the peers' copies. Empty:
	// nothing is saved.
	State string
	// ConfigPath is the hab's config file with the [catalog] section. Empty: none.
	ConfigPath string
	// Structure returns this hab's structure with its agents. Nil: no projects or agents from it.
	Structure func() api.Structure
	// ProjectDirs returns each project's working folders. Nil: none.
	ProjectDirs func() []structure.ProjectDir
	Peers       Peers // nil: no peers
	// Notify raises a Flightdeck notification (notice.raise). Nil: none.
	Notify func(api.NoticeRaiseParams)
	// Publish gets each catalog.updated. Nil: none.
	Publish func(api.CatalogUpdated)
	// Detect reads the host's facts. Default Detect.
	Detect func(context.Context) Facts
	// ReadFolder reads one repository folder. Default ReadFolder.
	ReadFolder func(context.Context, string) (Folder, bool)
	Pull       time.Duration // how often each peer link pulls and puts. Default DefaultPull
	Rescan     time.Duration // how often project folders and the config are read. Default DefaultRescan
	Redetect   time.Duration // how often the host's facts are read. Default DefaultRedetect
	Debounce   time.Duration // how long Poke waits for more changes. Default DefaultDebounce
	Now        func() time.Time
	Home       func() (string, error)
	Logf       func(format string, args ...any)
}

// copyRec is a peer's catalog as this hab holds it.
type copyRec struct {
	Catalog api.Catalog `json:"catalog"`
	SeenAt  time.Time   `json:"seen_at"`
}

// saved is catalog.json.
type saved struct {
	Generation uint64              `json:"generation"`
	Own        *api.Catalog        `json:"own,omitempty"` // served after a restart until the first full build
	Copies     map[string]*copyRec `json:"copies"`
}

// Service keeps the own catalog and the peers' copies.
type Service struct {
	cfg  Config
	poke chan struct{}
	kick chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once

	mu        sync.Mutex
	gen       uint64
	own       api.Catalog
	facts     Facts
	declared  Declared
	configOK  bool // a config file was read and parsed at least once
	problems  []string
	folders   map[string]Folder // by path, from the last scan
	scanned   map[string]bool   // paths the last scan tried
	copies    map[string]*copyRec
	put       map[string]uint64   // label -> the generation last handed over on its link
	pulled    map[string]uint64   // device id -> the generation of the copy last pulled; a put clears it
	fails     map[string]*failure // label -> the failed exchanges in a row on that link
	alerted   map[string]bool     // device id -> the always-on alert went out for this outage
	watchFrom time.Time           // since when this hab has been up without a sleep
	lastTick  time.Time
}

// New loads the saved state and starts the catalog. The first catalog is built before New
// returns from the facts and projects saved by the last run, if any; fresh ones follow in the
// background.
func New(cfg Config) (*Service, error) {
	if cfg.Self == "" {
		return nil, errors.New("catalog: no device id")
	}
	s := newService(cfg)
	if err := s.load(); err != nil {
		return nil, err
	}
	// A restart may have changed anything: the first build is a new generation.
	s.gen = s.nextGen()
	s.loadConfig()
	s.build(true)
	s.wg.Add(2)
	go s.run()
	go s.sync()
	return s, nil
}

// newService fills in the defaults and makes the Service, with nothing loaded or started.
func newService(cfg Config) *Service {
	if cfg.Detect == nil {
		cfg.Detect = Detect
	}
	if cfg.ReadFolder == nil {
		cfg.ReadFolder = ReadFolder
	}
	if cfg.Pull <= 0 {
		cfg.Pull = DefaultPull
	}
	if cfg.Rescan <= 0 {
		cfg.Rescan = DefaultRescan
	}
	if cfg.Redetect <= 0 {
		cfg.Redetect = DefaultRedetect
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Home == nil {
		cfg.Home = os.UserHomeDir
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Service{
		cfg:      cfg,
		poke:     make(chan struct{}, 1),
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		declared: Declared{AlertAfter: DefaultAlertAfter},
		folders:  map[string]Folder{},
		scanned:  map[string]bool{},
		copies:   map[string]*copyRec{},
		put:      map[string]uint64{},
		pulled:   map[string]uint64{},
		fails:    map[string]*failure{},
		alerted:  map[string]bool{},
	}
	s.watchFrom = s.now()
	return s
}

// now is the wall clock with no monotonic reading. The monotonic clock stops while the host
// sleeps on Linux and macOS, so a gap measured with it never shows a sleep (mesh.md 8).
func (s *Service) now() time.Time { return s.cfg.Now().Round(0) }

// nextGen is the generation after the current one: one more, and never below the wall clock
// in milliseconds, so a hab that lost catalog.json starts above every generation of its earlier
// life and no puller takes its new catalog for the one it holds.
func (s *Service) nextGen() uint64 {
	g := s.gen + 1
	if ms := s.cfg.Now().UnixMilli(); ms > 0 && uint64(ms) > g {
		g = uint64(ms)
	}
	return g
}

// Close stops the catalog.
func (s *Service) Close() {
	s.once.Do(func() { close(s.done) })
	s.wg.Wait()
}

// Poke says the structure or the agents changed: the catalog is built again after the
// debounce. It never blocks.
func (s *Service) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Kick starts a pull and put round now: a peer link came up. It never blocks.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Problems returns the config file's problems from its last read.
func (s *Service) Problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.problems)
}

// Get answers catalog.get: the whole own catalog when its generation is not since.
func (s *Service) Get(since uint64) api.CatalogGetResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := api.CatalogGetResult{Generation: s.gen}
	if since != s.gen {
		c := cloneCatalog(s.own)
		res.Catalog = &c
	}
	return res
}

// Put answers catalog.put: a peer link handed over its hab's catalog. The control server has
// checked that the catalog's device id is the link's.
func (s *Service) Put(from api.PeerHello, c api.Catalog) error {
	if from.DeviceID == "" || c.DeviceID != from.DeviceID {
		return fmt.Errorf("catalog: device %s handed over the catalog of %s", from.DeviceID, c.DeviceID)
	}
	if c.DeviceID == s.cfg.Self {
		return errors.New("catalog: a catalog with this hab's own device id")
	}
	s.store(c)
	// The next pull asks for everything: a put never stands in for what the peer itself says.
	s.mu.Lock()
	delete(s.pulled, c.DeviceID)
	s.mu.Unlock()
	return nil
}

// store keeps a peer's catalog and says so when it changed.
func (s *Service) store(c api.Catalog) {
	c = api.NonNil(c)
	s.mu.Lock()
	old, had := s.copies[c.DeviceID]
	changed := !had || !reflect.DeepEqual(old.Catalog, c)
	mark := had && old.Catalog.AlwaysOn != c.AlwaysOn || !had && c.AlwaysOn
	s.copies[c.DeviceID] = &copyRec{Catalog: c, SeenAt: s.cfg.Now()}
	if changed {
		s.saveLocked()
	}
	s.mu.Unlock()
	if changed && s.cfg.Publish != nil {
		s.cfg.Publish(api.CatalogUpdated{DeviceID: c.DeviceID, Generation: c.Generation})
	}
	if mark {
		s.Poke() // the hosts this hab watches changed
	}
}

// seen records that a peer confirmed its copy unchanged.
func (s *Service) seen(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.copies[id]; ok {
		c.SeenAt = s.cfg.Now()
	}
}

// List answers catalog.list: this hab's catalog, then its peers' copies in peer list order,
// then copies from linked habs the list lacks, and the projects matched across them.
func (s *Service) List() api.CatalogList {
	var pl api.PeerList
	if s.cfg.Peers != nil {
		pl = s.cfg.Peers.List()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	out := api.CatalogList{Hosts: []api.CatalogEntry{{Own: true, SeenAt: now.UTC().Format(time.RFC3339), Catalog: cloneCatalog(s.own)}}}
	shown := map[string]bool{s.cfg.Self: true}
	add := func(id, label string, online bool) {
		c, ok := s.copies[id]
		if !ok || shown[id] {
			return
		}
		shown[id] = true
		out.Hosts = append(out.Hosts, api.CatalogEntry{Label: label, Stale: !online, SeenAt: c.SeenAt.UTC().Format(time.RFC3339),
			Catalog: cloneCatalog(c.Catalog)})
	}
	for _, p := range pl.Peers {
		if p.DeviceID != "" {
			add(p.DeviceID, p.Label, p.State == api.PeerStateOnline)
		}
	}
	for _, u := range pl.Unlisted {
		add(u.DeviceID, "", true)
	}
	out.Projects = groupProjects(out.Hosts)
	return out
}

// groupProjects matches projects across hosts by merge.ProjectKey: the remote, else the root
// commit; a project with neither stays its own on its host.
func groupProjects(hosts []api.CatalogEntry) []api.CatalogProjectGroup {
	var out []api.CatalogProjectGroup
	idx := map[string]int{}
	for _, h := range hosts {
		for _, p := range h.Catalog.Projects {
			k := projectKey(h.Catalog.DeviceID, p)
			if k == "" {
				continue
			}
			i, ok := idx[k]
			if !ok {
				i = len(out)
				idx[k] = i
				out = append(out, api.CatalogProjectGroup{Key: k, Name: p.Name, Remote: p.Remote, RootCommit: p.RootCommit})
			}
			loc := api.CatalogLocation{DeviceID: h.Catalog.DeviceID, Label: h.Label, Stale: h.Stale, Folders: slices.Clone(p.Folders)}
			// One host's two entries of one key (two folders of one project) are one location.
			if n := len(out[i].Locations); n > 0 && out[i].Locations[n-1].DeviceID == loc.DeviceID {
				out[i].Locations[n-1].Folders = append(out[i].Locations[n-1].Folders, loc.Folders...)
				continue
			}
			out[i].Locations = append(out[i].Locations, loc)
		}
	}
	return out
}

// projectKey is merge's project key for a catalog project; the first folder stands in for the
// project id of a project with no remote and no root commit.
func projectKey(device string, p api.CatalogProject) string {
	id := ""
	if len(p.Folders) > 0 {
		id = p.Folders[0].Path
	}
	if id == "" {
		id = p.Name
	}
	return merge.ProjectKey(device, api.Project{ID: id, Remote: p.Remote, RootCommit: p.RootCommit})
}

// run builds the catalog on pokes, scans project folders and reads the config every Rescan,
// and reads the host's facts every Redetect.
func (s *Service) run() {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	s.detect(ctx)
	s.scan(ctx)
	s.build(false)
	rescan := time.NewTicker(s.cfg.Rescan)
	defer rescan.Stop()
	redetect := time.NewTicker(s.cfg.Redetect)
	defer redetect.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-s.done:
			return
		case <-s.poke:
			if debounce == nil {
				debounce = time.After(s.cfg.Debounce)
			}
		case <-debounce:
			debounce = nil
			s.readNew(ctx)
			s.build(false)
		case <-rescan.C:
			s.loadConfig()
			s.scan(ctx)
			s.build(false)
		case <-redetect.C:
			s.detect(ctx)
			s.build(false)
		}
	}
}

func (s *Service) detect(ctx context.Context) {
	f := s.cfg.Detect(ctx)
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.facts = f
	s.mu.Unlock()
}

func (s *Service) loadConfig() {
	d, problems, err := LoadConfig(s.cfg.ConfigPath, s.cfg.Home)
	s.mu.Lock()
	if err != nil {
		problems = []string{err.Error()}
		if s.configOK {
			d = s.declared // a file that does not parse keeps the last good section
		} // with none yet, the defaults LoadConfig returned (the alert threshold among them)
	} else {
		s.configOK = true
	}
	s.declared, s.problems = d, problems
	s.mu.Unlock()
	for _, p := range problems {
		s.cfg.Logf("catalog: %s", p)
	}
}

// dirs are the folders to read: the project roots' repositories, then the structure's
// project folders.
func (s *Service) dirs() []string {
	s.mu.Lock()
	roots := slices.Clone(s.declared.ProjectRoots)
	s.mu.Unlock()
	out := FindRepos(roots)
	if s.cfg.ProjectDirs != nil {
		for _, pd := range s.cfg.ProjectDirs() {
			for _, d := range pd.Dirs {
				if !slices.Contains(out, d) {
					out = append(out, d)
				}
			}
		}
	}
	return out
}

// scan reads every folder again.
func (s *Service) scan(ctx context.Context) {
	dirs := s.dirs()
	got := map[string]Folder{}
	tried := map[string]bool{}
	for _, d := range dirs {
		if ctx.Err() != nil {
			return
		}
		tried[d] = true
		if f, ok := s.cfg.ReadFolder(ctx, d); ok {
			got[d] = f
		}
	}
	s.mu.Lock()
	s.folders, s.scanned = got, tried
	s.mu.Unlock()
}

// readNew reads only the folders the last scan did not try: a project the structure gained.
func (s *Service) readNew(ctx context.Context) {
	if s.cfg.ProjectDirs == nil {
		return
	}
	var fresh []string
	pds := s.cfg.ProjectDirs()
	s.mu.Lock()
	for _, pd := range pds {
		for _, d := range pd.Dirs {
			if !s.scanned[d] {
				fresh = append(fresh, d)
			}
		}
	}
	s.mu.Unlock()
	for _, d := range fresh {
		f, ok := s.cfg.ReadFolder(ctx, d)
		s.mu.Lock()
		s.scanned[d] = true
		if ok {
			s.folders[d] = f
		}
		s.mu.Unlock()
	}
}

// build makes the own catalog and raises the generation when it changed. first is the build
// in New: it takes the generation New set.
func (s *Service) build(first bool) {
	var st api.Structure
	if s.cfg.Structure != nil {
		st = s.cfg.Structure()
	}
	var dirs []structure.ProjectDir
	if s.cfg.ProjectDirs != nil {
		dirs = s.cfg.ProjectDirs()
	}
	var peerIDs []string
	if s.cfg.Peers != nil {
		for _, l := range s.cfg.Peers.Links() {
			if l.DeviceID != "" && l.DeviceID != s.cfg.Self && !slices.Contains(peerIDs, l.DeviceID) {
				peerIDs = append(peerIDs, l.DeviceID)
			}
		}
	}
	slices.Sort(peerIDs)
	s.mu.Lock()
	var watches []string
	for _, id := range peerIDs {
		if c, ok := s.copies[id]; ok && c.Catalog.AlwaysOn {
			watches = append(watches, id)
		}
	}
	c := api.Catalog{
		DeviceID:     s.cfg.Self,
		Host:         s.cfg.Host,
		System:       s.facts.System,
		Toolchains:   slices.Clone(s.facts.Toolchains),
		Harnesses:    slices.Clone(s.facts.Harnesses),
		Capabilities: words(s.facts.Capabilities, s.declared.Capabilities),
		Services:     slices.Clone(s.declared.Services),
		Actions:      slices.Clone(s.declared.Actions),
		Projects:     projects(s.cfg.Self, st, dirs, s.folders),
		Agents:       agents(st),
		AlwaysOn:     s.declared.AlwaysOn,
		Peers:        peerIDs,
		Watches:      watches,
	}
	c = api.NonNil(c)
	prev := s.own
	prev.Generation, prev.At = 0, ""
	if !first && reflect.DeepEqual(prev, c) {
		s.mu.Unlock()
		return
	}
	if !first {
		s.gen = s.nextGen()
	}
	c.Generation, c.At = s.gen, s.cfg.Now().UTC().Format(time.RFC3339)
	s.own = c
	s.saveLocked()
	s.mu.Unlock()
	if s.cfg.Publish != nil {
		s.cfg.Publish(api.CatalogUpdated{DeviceID: s.cfg.Self, Generation: c.Generation})
	}
	s.Kick()
}

// words merges detected and declared capability words: one entry per word, sorted, marked
// with both sources. A declared word never hides a detected one.
func words(detected, declared []string) []api.CatalogCapability {
	m := map[string]*api.CatalogCapability{}
	for _, w := range detected {
		if m[w] == nil {
			m[w] = &api.CatalogCapability{Name: w}
		}
		m[w].Detected = true
	}
	for _, w := range declared {
		if m[w] == nil {
			m[w] = &api.CatalogCapability{Name: w}
		}
		m[w].Declared = true
	}
	out := make([]api.CatalogCapability, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b api.CatalogCapability) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// projects groups the folders read by project identity. A structure project with the same
// identity gives the name; otherwise the first folder's base name does.
func projects(self string, st api.Structure, dirs []structure.ProjectDir, folders map[string]Folder) []api.CatalogProject {
	names := map[string]string{}
	for _, p := range st.Projects {
		if k := merge.ProjectKey(self, p); k != "" {
			names[k] = p.Name
		}
	}
	// Order: the structure's folders first, in project order, then the scanned ones by path.
	var order []string
	seen := map[string]bool{}
	for _, pd := range dirs {
		for _, d := range pd.Dirs {
			if _, ok := folders[d]; ok && !seen[d] {
				seen[d] = true
				order = append(order, d)
			}
		}
	}
	var rest []string
	for d := range folders {
		if !seen[d] {
			rest = append(rest, d)
		}
	}
	slices.Sort(rest)
	order = append(order, rest...)

	var out []api.CatalogProject
	idx := map[string]int{}
	for _, d := range order {
		f := folders[d]
		p := api.CatalogProject{Remote: f.Remote, RootCommit: f.RootCommit}
		k := merge.ProjectKey(self, api.Project{ID: d, Remote: f.Remote, RootCommit: f.RootCommit})
		if i, ok := idx[k]; ok {
			out[i].Folders = append(out[i].Folders, f.State)
			continue
		}
		p.Name = names[k]
		if p.Name == "" {
			p.Name = filepath.Base(d)
		}
		p.Summary = f.Summary
		p.Folders = []api.CatalogFolder{f.State}
		idx[k] = len(out)
		out = append(out, p)
	}
	return out
}

// agents lists every pane with an agent that has not exited, by its session.
func agents(st api.Structure) []api.CatalogAgent {
	sessions := map[string]api.Session{}
	for _, ss := range st.Sessions {
		if ss.Pane != "" {
			sessions[ss.Pane] = ss
		}
	}
	names := map[string]string{}
	for _, p := range st.Projects {
		names[p.ID] = p.Name
	}
	var out []api.CatalogAgent
	for _, a := range st.Agents {
		if a.State == api.AgentStatusExited {
			continue
		}
		ss, ok := sessions[a.Pane]
		name := ss.Name
		if !ok {
			name = a.Pane // an unfiled pane: its id is its only name
		}
		out = append(out, api.CatalogAgent{Session: name, Harness: a.Harness, Project: names[ss.Project], State: a.State, Since: a.At})
	}
	slices.SortFunc(out, func(a, b api.CatalogAgent) int { return strings.Compare(a.Session, b.Session) })
	return out
}

// load reads catalog.json. A missing file is a first start; one that does not parse is set
// aside and the catalog starts again, with a generation from the wall clock that is above the
// earlier life's, which peers take as "replace". The saved own catalog gives back the facts
// and the folders read, so the first catalog after a restart is not empty.
func (s *Service) load() error {
	if s.cfg.State == "" {
		return nil
	}
	p := filepath.Join(s.cfg.State, stateFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.cfg.Logf("catalog: %s: %v; starting again", p, err)
		return nil
	}
	var sv saved
	if err := json.Unmarshal(b, &sv); err != nil {
		s.cfg.Logf("catalog: %s: %v; starting again", p, err)
		os.Rename(p, p+".bad")
		return nil
	}
	s.gen = sv.Generation
	for id, c := range sv.Copies {
		if c != nil && id != s.cfg.Self {
			s.copies[id] = c
		}
	}
	if o := sv.Own; o != nil && o.DeviceID == s.cfg.Self {
		s.restore(*o)
	}
	return nil
}

// restore takes the facts and the folders read back from a saved own catalog.
func (s *Service) restore(o api.Catalog) {
	s.facts = Facts{System: o.System, Toolchains: o.Toolchains, Harnesses: o.Harnesses}
	for _, c := range o.Capabilities {
		if c.Detected {
			s.facts.Capabilities = append(s.facts.Capabilities, c.Name)
		}
	}
	for _, p := range o.Projects {
		for _, f := range p.Folders {
			if f.Path != "" {
				s.folders[f.Path] = Folder{Remote: p.Remote, RootCommit: p.RootCommit, Summary: p.Summary, State: f}
			}
		}
	}
}

// saveLocked writes catalog.json through a temporary file, synced to disk, and a rename, so a
// crash leaves the old file or the new one. The caller holds s.mu.
func (s *Service) saveLocked() {
	if s.cfg.State == "" {
		return
	}
	var own *api.Catalog
	if s.own.DeviceID != "" {
		own = &s.own
	}
	b, err := json.Marshal(saved{Generation: s.gen, Own: own, Copies: s.copies})
	if err != nil {
		s.cfg.Logf("catalog: save: %v", err)
		return
	}
	if err := writeSynced(filepath.Join(s.cfg.State, stateFile), b); err != nil {
		s.cfg.Logf("catalog: save: %v", err)
	}
}

// writeSynced writes a file through a temporary file that is synced before the rename, then
// syncs the folder so the rename itself is on disk.
func writeSynced(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// cloneCatalog copies a catalog so a caller's changes do not reach the stored one.
func cloneCatalog(c api.Catalog) api.Catalog {
	b, _ := json.Marshal(c)
	var out api.Catalog
	json.Unmarshal(b, &out)
	return api.NonNil(out)
}
