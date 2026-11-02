package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/readtools"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// capcomDir is the folder under the state directory where the session host keeps Capcom's
// folders (capcom.md 7.1); the read scope allows it by default.
const capcomDir = "capcom"

// noticeKeep is how many raised notices notice.get can read back.
const noticeKeep = 500

// noticeLog keeps the last notices this hab raised, in memory, for notice.get.
type noticeLog struct {
	mu    sync.Mutex
	order []string
	byID  map[string]api.Notice
}

func (l *noticeLog) add(n api.Notice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = map[string]api.Notice{}
	}
	l.byID[n.ID] = n
	l.order = append(l.order, n.ID)
	if len(l.order) > noticeKeep {
		delete(l.byID, l.order[0])
		l.order = l.order[1:]
	}
}

func (l *noticeLog) get(id string) (api.Notice, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.byID[id]
	return n, ok
}

// readPeers is the peer list as the read tools reach it.
type readPeers struct{ m *peers.Manager }

func (r readPeers) List() api.PeerList { return r.m.List() }

func (r readPeers) Links() []readtools.Link {
	ls := r.m.Links()
	out := make([]readtools.Link, 0, len(ls))
	for _, l := range ls {
		rl := readtools.Link{Label: l.Label, DeviceID: l.DeviceID, Online: l.Online, Caps: l.Caps}
		if l.Conn != nil {
			rl.Caller = l.Conn
		}
		out = append(out, rl)
	}
	return out
}

// readsSetup is what newReads needs.
type readsSetup struct {
	self, host string
	state      string
	configPath string
	store      *structure.Store
	agents     *harness.Store
	catalog    *catalog.Service
	peers      *peers.Manager
	notices    *noticeLog
	getenv     func(string) string
	logf       func(string, ...any)
}

// newReads builds the read tools' service: the transcripts of Claude Code and Codex, the read
// scope from the defaults and the [read] section of the hab's config file, and the peer links.
func newReads(rs readsSetup) *readtools.Service {
	tr := &readtools.Transcripts{}
	if d, err := transcript.ClaudeConfigDir(rs.getenv); err == nil {
		tr.ClaudeDir = d
	}
	if d, err := transcript.CodexHome(rs.getenv); err == nil {
		tr.CodexHome = d
	}
	home, _ := os.UserHomeDir()
	projects := &projectFolders{catalog: rs.catalog, host: rs.self}
	src := &readtools.ScopeSource{
		Host: rs.host, Home: home, Config: rs.configPath, Logf: rs.logf,
		Roots: func() []string {
			roots := append(tr.Roots(), filepath.Join(rs.state, capcomDir))
			roots = append(roots, projects.folders()...)
			for _, p := range rs.store.ProjectDirs() {
				roots = append(roots, p.Dirs...)
			}
			return roots
		},
	}
	cfg := readtools.Config{
		Self: rs.self, Host: rs.host, Transcripts: tr, Scope: src.Scope,
		ProjectName: projects.name,
		Live: func() []readtools.Live {
			var out []readtools.Live
			for _, ss := range rs.store.Snapshot(api.HostInfo{ID: rs.host}).Sessions {
				if ss.Pane == "" {
					continue
				}
				if st, ok := rs.agents.Get(ss.Pane); ok && st.Session != "" {
					out = append(out, readtools.Live{Harness: st.Harness, Native: st.Session, Session: ss.ID, Name: ss.Name, State: st.State})
				}
			}
			return out
		},
		Notice:  rs.notices.get,
		Session: rs.store.Session,
	}
	if rs.peers != nil {
		cfg.Peers = readPeers{rs.peers}
	}
	return readtools.New(cfg)
}

// projectFolders maps folders to this hab's catalog projects, from a copy at most 5 s old.
type projectFolders struct {
	catalog *catalog.Service
	host    string // this hab's device id

	mu    sync.Mutex
	at    time.Time
	byDir map[string]string // folder: project name
}

func (p *projectFolders) load() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byDir != nil && time.Since(p.at) < 5*time.Second {
		return p.byDir
	}
	m := map[string]string{}
	if p.catalog != nil {
		for _, h := range p.catalog.List().Hosts {
			if !h.Own {
				continue
			}
			for _, pr := range h.Catalog.Projects {
				for _, f := range pr.Folders {
					m[f.Path] = pr.Name
				}
			}
		}
	}
	p.byDir, p.at = m, time.Now()
	return m
}

func (p *projectFolders) folders() []string {
	m := p.load()
	out := make([]string, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	return out
}

// name is the project of the folder, or of the nearest folder above it.
func (p *projectFolders) name(folder string) string {
	m := p.load()
	for f := filepath.Clean(folder); ; f = filepath.Dir(f) {
		if n, ok := m[f]; ok {
			return n
		}
		if f == filepath.Dir(f) {
			return ""
		}
	}
}

// readHook answers the read methods on a pane's hook route: an agent's own calls, which read
// every host (capcom.md 2.3).
func readHook(rs control.Reads, cat control.Catalog, pl control.Peers, lb control.Logbook) hook.ReadHandler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		decode := func(v any) error {
			if err := json.Unmarshal(params, v); err != nil {
				return bad(err)
			}
			return nil
		}
		switch method {
		case "catalog.list":
			if cat == nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "catalog.list needs the catalog, which this hab is running without")
			}
			return api.NonNil(cat.List()), nil
		case "peers.list":
			if pl == nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "peers.list needs the peer list, which this hab is running without")
			}
			return api.NonNil(pl.List()), nil
		case "logbook.find", "logbook.get":
			if lb == nil {
				return nil, control.Errorf(api.ErrorKindUnavailable, "%s needs Logbook, which this hab does not have yet; the logbook capability is absent", method)
			}
			if method == "logbook.find" {
				var p api.LogbookFindParams
				if err := decode(&p); err != nil {
					return nil, err
				}
				return lb.Find(ctx, p)
			}
			var p api.LogbookGetParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			return lb.Get(ctx, p)
		}
		if rs == nil {
			return nil, control.Errorf(api.ErrorKindUnavailable, "%s needs the read tools, which this hab is running without", method)
		}
		switch method {
		case "transcript.list":
			var p api.TranscriptListParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			r, err := rs.Sessions(ctx, false, p)
			return api.NonNil(r), err
		case "transcript.search":
			var p api.TranscriptSearchParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			r, err := rs.Search(ctx, false, p)
			return api.NonNil(r), err
		case "transcript.read":
			var p api.TranscriptReadParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			r, err := rs.Transcript(ctx, false, p)
			return api.NonNil(r), err
		case "notice.get":
			var p api.NoticeGetParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			return rs.Notice(ctx, false, p)
		default: // file.read
			var p api.FileReadParams
			if err := decode(&p); err != nil {
				return nil, err
			}
			return rs.File(ctx, false, p)
		}
	}
}
