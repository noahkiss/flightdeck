package readtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// Capability is the capability word of the read tools.
const Capability = "read-tools"

// Limits (capcom.md 4, D11).
const (
	DefaultBudget  = 5 * time.Second // per hab, for a search or a list
	MaxHits        = 50
	DefaultList    = 50
	MaxList        = 200
	DefaultPage    = 50
	MaxPage        = 200
	DefaultFileRun = 64 << 10
	MaxFileRun     = 256 << 10
	snippetChars   = 240
)

// forwardGrace is how much longer a forwarded call may take than the peer's own budget: the
// peer stops at its budget and answers with what it found.
const forwardGrace = time.Second

// Live is a harness session that runs in a pane on this hab now.
type Live struct {
	Harness string
	Native  string // the harness's own session id
	Session string // the Flightdeck session id
	Name    string
	State   api.AgentStatus
}

// Link is one peer as the read tools reach it: over the link this hab dialed.
type Link struct {
	Label    string
	DeviceID string
	Online   bool
	Caps     []string
	Caller   api.Caller // nil while there is no link
}

// Peers is this hab's peer list.
type Peers interface {
	List() api.PeerList
	Links() []Link
}

// Config is what New needs. Every func may be nil.
type Config struct {
	Self        string // this hab's device id
	Host        string // this hab's host id
	Transcripts *Transcripts
	Scope       func() Scope
	ProjectName func(folder string) string // the catalog project that holds a folder
	Live        func() []Live
	Notice      func(id string) (api.Notice, bool)
	Session     func(id string) (api.Session, bool)
	Peers       Peers
	Budget      time.Duration // per hab; default DefaultBudget
}

// Service answers the read tools for this hab, and for every host through the peer links.
type Service struct {
	cfg Config
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Budget <= 0 {
		cfg.Budget = DefaultBudget
	}
	if cfg.Scope == nil {
		cfg.Scope = func() Scope { return Scope{Host: cfg.Host, Broken: true} }
	}
	return &Service{cfg: cfg}
}

// --- Targets ---

// target is one host a read goes to.
type target struct {
	host   api.ReadHost
	caller api.Caller // nil for this hab, or for a peer that cannot be asked
}

// targets picks the hosts for a read: host "" is every host when all is set, else this hab.
// local answers from this hab only (a call over a peer link: one hop).
func (s *Service) targets(host string, all, local bool) ([]target, error) {
	self := target{host: api.ReadHost{Label: s.cfg.Host, DeviceID: s.cfg.Self, Own: true, State: api.ReadHostStateOk}}
	if local || host == s.cfg.Host || host == "self" || (host == "" && !all) {
		return []target{self}, nil
	}
	out := []target{}
	if host == "" {
		out = append(out, self)
	}
	if s.cfg.Peers == nil {
		if host != "" {
			return nil, control.Errorf(api.ErrorKindNotFound, "no host %q: this hab has no peers", host)
		}
		return out, nil
	}
	links := map[string]Link{}
	for _, l := range s.cfg.Peers.Links() {
		links[l.Label] = l
	}
	found := false
	for _, p := range s.cfg.Peers.List().Peers {
		if host != "" && p.Label != host {
			continue
		}
		found = true
		t := target{host: api.ReadHost{Label: p.Label, DeviceID: p.DeviceID}}
		l, ok := links[p.Label]
		switch {
		case !ok || !l.Online || l.Caller == nil:
			t.host.State = api.ReadHostStateOffline
			t.host.Reason = p.Reason
			if p.State == api.PeerStateOnline {
				t.host.Reason = "only its own link to this hab is up; reads need a link this hab dialed"
			}
			if t.host.Reason == "" {
				t.host.Reason = string(p.State)
			}
		case !slices.Contains(l.Caps, Capability):
			t.host.State = api.ReadHostStateUpdateNeeded
			t.host.Reason = "its hab has no read tools"
		default:
			t.host.State = api.ReadHostStateOk
			t.caller = l.Caller
			if l.DeviceID != "" {
				t.host.DeviceID = l.DeviceID
			}
		}
		out = append(out, t)
	}
	if host != "" && !found {
		return nil, control.Errorf(api.ErrorKindNotFound, "no host %q in this hab's peer list", host)
	}
	return out, nil
}

// one picks the single host of a read that names at most one.
func (s *Service) one(host string, local bool) (target, error) {
	ts, err := s.targets(host, false, local)
	if err != nil {
		return target{}, err
	}
	t := ts[0]
	if !t.host.Own && t.caller == nil {
		return t, control.Errorf(api.ErrorKindUnavailable, "host %s cannot be read now: %s", t.host.Label, t.host.Reason)
	}
	return t, nil
}

// fan runs a read on each target at once: this hab with local, each askable peer with remote
// under the budget plus forwardGrace. It returns one answer per target, in target order.
func fan[T any](ctx context.Context, s *Service, ts []target, local func(ctx context.Context) (T, api.ReadHost), remote func(ctx context.Context, c api.Client) (T, error)) ([]T, []api.ReadHost) {
	vals := make([]T, len(ts))
	hosts := make([]api.ReadHost, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		hosts[i] = t.host
		if !t.host.Own && t.caller == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t.host.Own {
				v, h := local(ctx)
				vals[i] = v
				h.Label, h.DeviceID, h.Own = t.host.Label, t.host.DeviceID, true
				hosts[i] = h
				return
			}
			cctx, cancel := context.WithTimeout(ctx, s.cfg.Budget+forwardGrace)
			defer cancel()
			v, err := remote(cctx, api.Client{Caller: t.caller})
			switch {
			case err == nil:
				vals[i] = v
			case cctx.Err() != nil && ctx.Err() == nil:
				hosts[i].State = api.ReadHostStatePartial
				hosts[i].Reason = fmt.Sprintf("no answer in %v", s.cfg.Budget)
			default:
				hosts[i].State = api.ReadHostStateError
				hosts[i].Reason = errText(err)
			}
		}()
	}
	wg.Wait()
	return vals, hosts
}

// errText is an error's hint, without the JSON-RPC wrapping.
func errText(err error) string {
	var ce *control.Error
	if errors.As(err, &ce) && ce.Data.Hint != "" {
		return ce.Data.Hint
	}
	return err.Error()
}

// mergeHost copies what a peer said about itself into this hab's row for it.
func mergeHost(row *api.ReadHost, got []api.ReadHost) {
	for _, h := range got {
		if h.Own {
			if row.State == api.ReadHostStateOk {
				row.State, row.Reason = h.State, h.Reason
			}
			row.LeftOut = h.LeftOut
		}
	}
}

// --- Filters ---

type filter struct {
	harness string
	project string
	since   time.Time
	until   time.Time
}

func parseFilter(harness, project, since, until string) (filter, error) {
	f := filter{harness: harness, project: project}
	if harness != "" && harness != Claude && harness != Codex {
		return f, control.Errorf(api.ErrorKindInvalid, "harness %q: past transcripts are read for claude and codex", harness)
	}
	var err error
	if since != "" {
		if f.since, err = time.Parse(time.RFC3339, since); err != nil {
			return f, control.Errorf(api.ErrorKindInvalid, "since %q is not an RFC 3339 time", since)
		}
	}
	if until != "" {
		if f.until, err = time.Parse(time.RFC3339, until); err != nil {
			return f, control.Errorf(api.ErrorKindInvalid, "until %q is not an RFC 3339 time", until)
		}
	}
	return f, nil
}

func (f filter) keep(e Entry, project string) bool {
	if f.harness != "" && e.Harness != f.harness {
		return false
	}
	if !f.since.IsZero() && e.Updated.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && !e.Started.IsZero() && !e.Started.Before(f.until) {
		return false
	}
	if f.project != "" {
		if filepath.IsAbs(f.project) {
			return within(filepath.Clean(e.Folder), filepath.Clean(f.project))
		}
		return strings.EqualFold(project, f.project) || strings.EqualFold(filepath.Base(e.Folder), f.project)
	}
	return true
}

// entries lists the local transcripts that pass the filter and the scope, with the count the
// scope left out. A transcript is left out when its file is outside the scope or its folder
// matches the deny list (capcom.md 5.2).
func (s *Service) entries(ctx context.Context, f filter) (kept []Entry, projects []string, leftOut int) {
	if s.cfg.Transcripts == nil {
		return nil, nil, 0
	}
	sc := s.cfg.Scope().Resolved()
	for _, e := range s.cfg.Transcripts.List(ctx) {
		project := s.project(e.Folder)
		if !f.keep(e, project) {
			continue
		}
		if !s.inScope(sc, e) {
			leftOut++
			continue
		}
		kept = append(kept, e)
		projects = append(projects, project)
	}
	return kept, projects, leftOut
}

func (s *Service) inScope(sc Scope, e Entry) bool {
	if _, err := sc.Check(e.Path); err != nil {
		return false
	}
	return e.Folder == "" || !sc.Denied(e.Folder)
}

func (s *Service) project(folder string) string {
	if s.cfg.ProjectName == nil || folder == "" {
		return ""
	}
	return s.cfg.ProjectName(folder)
}

func (s *Service) live() map[string]Live {
	out := map[string]Live{}
	if s.cfg.Live == nil {
		return out
	}
	for _, l := range s.cfg.Live() {
		if l.Native != "" {
			out[l.Harness+"/"+l.Native] = l
		}
	}
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- transcript.list ---

// Sessions answers transcript.list.
func (s *Service) Sessions(ctx context.Context, local bool, p api.TranscriptListParams) (api.TranscriptSessions, error) {
	f, err := parseFilter(p.Harness, p.Project, p.Since, p.Until)
	if err != nil {
		return api.TranscriptSessions{}, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultList
	}
	limit = min(limit, MaxList)
	ts, err := s.targets(p.Host, true, local)
	if err != nil {
		return api.TranscriptSessions{}, err
	}
	fwd := p
	fwd.Host = ""
	vals, hosts := fan(ctx, s, ts, func(ctx context.Context) (api.TranscriptSessions, api.ReadHost) {
		return s.localSessions(ctx, f, limit)
	}, func(ctx context.Context, c api.Client) (api.TranscriptSessions, error) {
		return c.TranscriptList(ctx, fwd)
	})
	var out api.TranscriptSessions
	for i, v := range vals {
		if !ts[i].host.Own {
			mergeHost(&hosts[i], v.Hosts)
			for j := range v.Sessions {
				v.Sessions[j].Host, v.Sessions[j].DeviceID = hosts[i].Label, hosts[i].DeviceID
			}
		}
		out.Sessions = append(out.Sessions, v.Sessions...)
		out.More = out.More || v.More
	}
	slices.SortStableFunc(out.Sessions, func(a, b api.TranscriptSession) int { return strings.Compare(b.Updated, a.Updated) })
	if len(out.Sessions) > limit {
		out.Sessions, out.More = out.Sessions[:limit], true
	}
	out.Hosts = hosts
	return out, nil
}

func (s *Service) localSessions(ctx context.Context, f filter, limit int) (api.TranscriptSessions, api.ReadHost) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Budget)
	defer cancel()
	h := api.ReadHost{State: api.ReadHostStateOk}
	es, projects, leftOut := s.entries(ctx, f)
	h.LeftOut = leftOut
	if ctx.Err() != nil {
		h.State, h.Reason = api.ReadHostStatePartial, fmt.Sprintf("ran out of its %v", s.cfg.Budget)
	}
	live := s.live()
	var out api.TranscriptSessions
	for i, e := range es {
		if len(out.Sessions) == limit {
			out.More = true
			break
		}
		ts := api.TranscriptSession{Host: s.cfg.Host, DeviceID: s.cfg.Self, Harness: e.Harness, ID: e.ID, Folder: e.Folder,
			Project: projects[i], Title: e.Title, Started: stamp(e.Started), Updated: stamp(e.Updated)}
		if l, ok := live[e.Harness+"/"+e.ID]; ok {
			ts.Live = &api.TranscriptLive{Session: l.Session, Name: l.Name, State: l.State}
		}
		out.Sessions = append(out.Sessions, ts)
	}
	out.Hosts = []api.ReadHost{h}
	return out, h
}

// --- transcript.search ---

// Search answers transcript.search.
func (s *Service) Search(ctx context.Context, local bool, p api.TranscriptSearchParams) (api.TranscriptHits, error) {
	terms := Terms(p.Words)
	if len(terms) == 0 {
		return api.TranscriptHits{}, control.Errorf(api.ErrorKindInvalid, "transcript.search needs words")
	}
	f, err := parseFilter(p.Harness, p.Project, p.Since, "")
	if err != nil {
		return api.TranscriptHits{}, err
	}
	limit := p.Limit
	if limit <= 0 || limit > MaxHits {
		limit = MaxHits
	}
	ts, err := s.targets(p.Host, true, local)
	if err != nil {
		return api.TranscriptHits{}, err
	}
	fwd := p
	fwd.Host = ""
	vals, hosts := fan(ctx, s, ts, func(ctx context.Context) (api.TranscriptHits, api.ReadHost) {
		return s.localSearch(ctx, terms, f, limit)
	}, func(ctx context.Context, c api.Client) (api.TranscriptHits, error) {
		return c.TranscriptSearch(ctx, fwd)
	})
	var out api.TranscriptHits
	for i, v := range vals {
		if !ts[i].host.Own {
			mergeHost(&hosts[i], v.Hosts)
			for j := range v.Hits {
				v.Hits[j].Host, v.Hits[j].DeviceID = hosts[i].Label, hosts[i].DeviceID
			}
		}
		out.Hits = append(out.Hits, v.Hits...)
		out.More = out.More || v.More
	}
	slices.SortStableFunc(out.Hits, func(a, b api.TranscriptHit) int { return compareTimes(b.At, a.At) })
	if len(out.Hits) > limit {
		out.Hits, out.More = out.Hits[:limit], true
	}
	out.Hosts = hosts
	return out, nil
}

// compareTimes orders RFC 3339 times; one that does not parse sorts as the oldest.
func compareTimes(a, b string) int {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	switch {
	case ea != nil && eb != nil:
		return strings.Compare(a, b)
	case ea != nil:
		return -1
	case eb != nil:
		return 1
	}
	return ta.Compare(tb)
}

func (s *Service) localSearch(ctx context.Context, terms []string, f filter, limit int) (api.TranscriptHits, api.ReadHost) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Budget)
	defer cancel()
	h := api.ReadHost{State: api.ReadHostStateOk}
	es, projects, leftOut := s.entries(ctx, f)
	h.LeftOut = leftOut
	var out api.TranscriptHits
	for i, e := range es {
		if ctx.Err() != nil {
			break
		}
		if len(out.Hits) >= limit {
			out.More = true
			break
		}
		hits, err := searchFile(ctx, e, terms, limit-len(out.Hits)+1)
		if err != nil && ctx.Err() == nil {
			continue // a file that cannot be read now is skipped
		}
		for _, hit := range hits {
			hit.Host, hit.DeviceID, hit.Project = s.cfg.Host, s.cfg.Self, projects[i]
			out.Hits = append(out.Hits, hit)
		}
	}
	if ctx.Err() != nil {
		h.State, h.Reason = api.ReadHostStatePartial, fmt.Sprintf("ran out of its %v", s.cfg.Budget)
	}
	slices.SortStableFunc(out.Hits, func(a, b api.TranscriptHit) int { return compareTimes(b.At, a.At) })
	if len(out.Hits) > limit {
		out.Hits, out.More = out.Hits[:limit], true
	}
	out.Hosts = []api.ReadHost{h}
	return out, h
}

// searchFile returns up to max hits in one transcript, newest first. A file whose bytes lack a
// term is not parsed.
func searchFile(ctx context.Context, e Entry, terms []string, max int) ([]api.TranscriptHit, error) {
	raw, err := os.ReadFile(e.Path)
	if err != nil {
		return nil, err
	}
	low := bytes.ToLower(raw)
	for _, t := range terms {
		if !bytes.Contains(low, []byte(t)) {
			return nil, nil
		}
	}
	msgs, err := Open(e.Harness, e.Path, e.ID).All(ctx)
	if err != nil {
		return nil, err
	}
	var out []api.TranscriptHit
	for i := len(msgs) - 1; i >= 0 && len(out) < max; i-- {
		m := msgs[i]
		text := messageText(m)
		if snip, ok := Snippet(text, terms); ok {
			out = append(out, api.TranscriptHit{Harness: e.Harness, Session: e.ID, Folder: e.Folder,
				Message: m.ID, Kind: m.Kind, At: m.At, Snippet: snip})
		}
	}
	return out, nil
}

// Terms splits search words into lower-case terms; double quotes keep a phrase together.
func Terms(words string) []string {
	var out []string
	for i, part := range strings.Split(words, `"`) {
		part = strings.ToLower(part)
		if i%2 == 1 {
			if p := strings.Join(strings.Fields(part), " "); p != "" {
				out = append(out, p)
			}
			continue
		}
		out = append(out, strings.Fields(part)...)
	}
	return out
}

// Snippet reports whether text holds every term, any case, and returns the text around the
// first term's first match, at most snippetChars runes, on one line.
func Snippet(text string, terms []string) (string, bool) {
	low := strings.ToLower(text)
	if len(low) != len(text) { // a case change moved bytes: match on a lowered copy only
		text = low
	}
	at := -1
	for _, t := range terms {
		i := strings.Index(low, t)
		if i < 0 {
			return "", false
		}
		if at < 0 {
			at = i
		}
	}
	start := max(0, at-snippetChars/3)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	s := text[start:]
	s = strings.Join(strings.Fields(s), " ")
	s = cutRunes(s, snippetChars)
	if start > 0 {
		s = "…" + s
	}
	return s, true
}

// --- transcript.read ---

// Transcript answers transcript.read.
func (s *Service) Transcript(ctx context.Context, local bool, p api.TranscriptReadParams) (api.TranscriptPage, error) {
	if p.Session == "" {
		return api.TranscriptPage{}, control.Errorf(api.ErrorKindInvalid, "transcript.read needs a session")
	}
	t, err := s.one(p.Host, local)
	if err != nil {
		return api.TranscriptPage{}, err
	}
	if !t.host.Own {
		fwd := p
		fwd.Host = ""
		cctx, cancel := context.WithTimeout(ctx, s.cfg.Budget+forwardGrace)
		defer cancel()
		res, err := api.Client{Caller: t.caller}.TranscriptRead(cctx, fwd)
		if err != nil {
			return res, err
		}
		res.Host = t.host.Label
		return res, nil
	}
	harness, native := p.Harness, p.Session
	if harness != "" && harness != Claude && harness != Codex {
		return api.TranscriptPage{}, control.Errorf(api.ErrorKindInvalid, "harness %q: transcripts are read for claude and codex", harness)
	}
	// A Flightdeck session id or name on this hab names its running harness session.
	if s.cfg.Live != nil {
		for _, l := range s.cfg.Live() {
			if (l.Session == p.Session || l.Name == p.Session) && (harness == "" || harness == l.Harness) {
				harness, native = l.Harness, l.Native
				break
			}
		}
	}
	if s.cfg.Transcripts == nil {
		return api.TranscriptPage{}, control.Errorf(api.ErrorKindUnavailable, "%s reads no transcripts", s.cfg.Host)
	}
	var path string
	for _, h := range []string{Claude, Codex} {
		if harness != "" && h != harness {
			continue
		}
		if found, err := s.cfg.Transcripts.Find(h, native); err == nil {
			harness, path = h, found
			break
		}
	}
	if path == "" {
		return api.TranscriptPage{}, control.Errorf(api.ErrorKindNotFound, "%s has no transcript for session %s", s.cfg.Host, p.Session)
	}
	sc := s.cfg.Scope()
	real, err := sc.Check(path)
	if err != nil {
		return api.TranscriptPage{}, err
	}
	r := Open(harness, real, native)
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultPage
	}
	limit = min(limit, MaxPage)
	msgs, more, _, err := r.Page(ctx, p.Before, limit)
	if err != nil {
		if errors.Is(err, transcript.ErrNoTranscript) {
			return api.TranscriptPage{}, control.Errorf(api.ErrorKindNotFound, "%s has no transcript for session %s", s.cfg.Host, p.Session)
		}
		return api.TranscriptPage{}, control.Errorf(api.ErrorKindInvalid, "%s: transcript.read: %v", s.cfg.Host, err)
	}
	folder := r.Cwd()
	if folder != "" && sc.Denied(folder) {
		return api.TranscriptPage{}, sc.refuse("the session's folder matches read.deny")
	}
	return api.TranscriptPage{Host: s.cfg.Host, Harness: harness, Session: native, Folder: folder, Messages: msgs, More: more}, nil
}

// --- notice.get ---

// Notice answers notice.get.
func (s *Service) Notice(ctx context.Context, local bool, p api.NoticeGetParams) (api.NoticeRecord, error) {
	if p.ID == "" {
		return api.NoticeRecord{}, control.Errorf(api.ErrorKindInvalid, "notice.get needs an id")
	}
	t, err := s.one(p.Host, local)
	if err != nil {
		return api.NoticeRecord{}, err
	}
	if !t.host.Own {
		fwd := p
		fwd.Host = ""
		cctx, cancel := context.WithTimeout(ctx, s.cfg.Budget+forwardGrace)
		defer cancel()
		res, err := api.Client{Caller: t.caller}.NoticeGet(cctx, fwd)
		if err != nil {
			return res, err
		}
		res.Host = t.host.Label
		return res, nil
	}
	if s.cfg.Notice == nil {
		return api.NoticeRecord{}, control.Errorf(api.ErrorKindNotFound, "%s has no notice %s", s.cfg.Host, p.ID)
	}
	n, ok := s.cfg.Notice(p.ID)
	if !ok {
		return api.NoticeRecord{}, control.Errorf(api.ErrorKindNotFound, "%s has no notice %s; notices are kept in memory since its hab started", s.cfg.Host, p.ID)
	}
	res := api.NoticeRecord{Host: s.cfg.Host, Notice: n}
	if n.Session != "" && s.cfg.Session != nil {
		if ss, ok := s.cfg.Session(n.Session); ok {
			res.Session = &ss
		}
	}
	return res, nil
}

// --- file.read ---

// File answers file.read. The file is opened read-only.
func (s *Service) File(ctx context.Context, local bool, p api.FileReadParams) (api.FileData, error) {
	if p.Path == "" {
		return api.FileData{}, control.Errorf(api.ErrorKindInvalid, "file.read needs a path")
	}
	if p.Offset < 0 {
		return api.FileData{}, control.Errorf(api.ErrorKindInvalid, "offset %d is negative", p.Offset)
	}
	t, err := s.one(p.Host, local)
	if err != nil {
		return api.FileData{}, err
	}
	if !t.host.Own {
		fwd := p
		fwd.Host = ""
		cctx, cancel := context.WithTimeout(ctx, s.cfg.Budget+forwardGrace)
		defer cancel()
		res, err := api.Client{Caller: t.caller}.FileRead(cctx, fwd)
		if err != nil {
			return res, err
		}
		res.Host = t.host.Label
		return res, nil
	}
	sc := s.cfg.Scope()
	real, err := sc.Check(p.Path)
	if err != nil {
		return api.FileData{}, err
	}
	clean, _ := sc.abs(p.Path)
	// Refuse a FIFO or a device before the open: opening a FIFO with no writer never returns.
	if fi, err := os.Stat(real); err == nil && !fi.Mode().IsRegular() {
		return api.FileData{}, control.Errorf(api.ErrorKindInvalid, "%s: %s is not a regular file", s.cfg.Host, clean)
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.FileData{}, control.Errorf(api.ErrorKindNotFound, "%s: no file %s", s.cfg.Host, clean)
		}
		return api.FileData{}, control.Errorf(api.ErrorKindUnavailable, "%s: %v", s.cfg.Host, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return api.FileData{}, control.Errorf(api.ErrorKindUnavailable, "%s: %v", s.cfg.Host, err)
	}
	if !fi.Mode().IsRegular() {
		return api.FileData{}, control.Errorf(api.ErrorKindInvalid, "%s: %s is not a regular file", s.cfg.Host, clean)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultFileRun
	}
	limit = min(limit, MaxFileRun)
	res := api.FileData{Host: s.cfg.Host, Path: clean, Size: fi.Size(), Offset: p.Offset}
	if p.Offset >= fi.Size() {
		return res, nil
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return api.FileData{}, control.Errorf(api.ErrorKindUnavailable, "%s: %v", s.cfg.Host, err)
	}
	buf = buf[:n]
	// A run may end inside a character: drop the partial one; the next run starts with it.
	if !utf8.Valid(buf) {
		for cut := 1; cut < utf8.UTFMax && cut <= len(buf); cut++ {
			if utf8.Valid(buf[:len(buf)-cut]) && utf8.RuneStart(buf[len(buf)-cut]) {
				buf = buf[:len(buf)-cut]
				break
			}
		}
	}
	if !utf8.Valid(buf) || bytes.IndexByte(buf, 0) >= 0 {
		res.Binary = true
		return res, nil
	}
	res.Text = string(buf)
	res.More = p.Offset+int64(len(buf)) < fi.Size()
	return res, nil
}
