package lbextra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/push"
)

// Engine turns texts into vectors (13, vector search). The hab ships no model: an engine is a
// local server the user runs, named in [logbook.vectors].
type Engine interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Engine call and key command bounds. The key command's backoff is that of [summaries]
// (app.md 16.5) and push (docs/push.md): after a failed run the next waits 30 s, doubling to
// 30 minutes; after a good run, a re-run on a refused key waits 5 minutes.
const (
	engineTimeout = 60 * time.Second
	maxKeyOutput  = 4 << 10
	keyRetryFirst = 30 * time.Second
	keyRetryMax   = 30 * time.Minute
	keyRerunAfter = 5 * time.Minute
)

// ErrNoKey reports that key_command has not printed a key yet.
var ErrNoKey = errors.New("logbook vectors: the key command has not printed a key")

// EngineOptions are an HTTPEngine's seams. All may be zero.
type EngineOptions struct {
	HTTP *http.Client // nil: a client with a 60 s timeout
	Now  func() time.Time
	Logf func(string, ...any)
	// KeyRun replaces running key_command; for tests.
	KeyRun func(ctx context.Context) (string, error)
}

// HTTPEngine asks an embeddings server that speaks the OpenAI-compatible
// `POST <base_url>/embeddings {"model", "input": [...]}` shape, such as a local model server,
// for vectors. With a key_command it sends the key the command printed as a bearer token; the
// key stays in memory only. Safe for concurrent use.
type HTTPEngine struct {
	endpoint string
	model    string
	hc       *http.Client
	now      func() time.Time
	logf     func(string, ...any)
	keyRun   func(ctx context.Context) (string, error)

	ctx  context.Context
	stop context.CancelFunc

	mu       sync.Mutex
	key      string
	keyBusy  bool
	keyNext  time.Time
	keyWait  time.Duration
	keyFails bool
}

// NewHTTPEngine returns an engine for <baseURL>/embeddings and starts loading the key in the
// background when keyCommand is not nil.
func NewHTTPEngine(baseURL, model string, keyCommand []string, o EngineOptions) *HTTPEngine {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: engineTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	e := &HTTPEngine{endpoint: strings.TrimRight(baseURL, "/") + "/embeddings", model: model, hc: o.HTTP, now: o.Now, logf: o.Logf, keyRun: o.KeyRun}
	e.ctx, e.stop = context.WithCancel(context.Background())
	if e.keyRun == nil && keyCommand != nil {
		argv := keyCommand
		e.keyRun = func(ctx context.Context) (string, error) {
			var key string
			err := push.RunSecret(ctx, "logbook.vectors key_command", argv, 0, maxKeyOutput, func(out []byte) error {
				for _, l := range strings.Split(string(out), "\n") {
					if l = strings.TrimSpace(l); l != "" {
						key = strings.Clone(l)
						return nil
					}
				}
				return errors.New("printed no key")
			})
			if err != nil {
				// The command can put its secret anywhere on stderr, including short tokens.
				return "", errors.New("logbook.vectors key_command failed")
			}
			return key, nil
		}
	}
	if e.keyRun != nil {
		e.mu.Lock()
		e.loadKeyLocked()
		e.mu.Unlock()
	}
	return e
}

// Close ends a key command run that is on; later calls fail at once.
func (e *HTTPEngine) Close() { e.stop() }

// loadKeyLocked starts a key command run when none is on and the backoff allows. Called with
// mu held.
func (e *HTTPEngine) loadKeyLocked() {
	if e.keyBusy || e.ctx.Err() != nil || e.now().Before(e.keyNext) {
		return
	}
	e.keyBusy = true
	go func() {
		key, err := e.keyRun(e.ctx)
		e.mu.Lock()
		defer e.mu.Unlock()
		e.keyBusy = false
		if e.ctx.Err() != nil {
			return
		}
		if err != nil || key == "" {
			if err == nil {
				err = errors.New("logbook.vectors key_command printed no key")
			}
			e.keyWait = min(max(2*e.keyWait, keyRetryFirst), keyRetryMax)
			e.keyNext = e.now().Add(e.keyWait)
			if !e.keyFails {
				e.keyFails = true
				e.logf("logbook vectors are off: %v; trying again in %v, backing off to %v", err, e.keyWait, keyRetryMax)
			}
			return
		}
		if e.keyFails {
			e.logf("logbook vectors: the key command printed a key")
		}
		e.key, e.keyFails, e.keyWait = key, false, 0
		e.keyNext = e.now().Add(keyRerunAfter)
	}()
}

// StatusError is an answer with a status other than 200.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("embeddings server: answered HTTP %d", e.Code)
}

// Embed sends one request for every text.
func (e *HTTPEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	key := e.key
	if e.keyRun != nil && key == "" {
		e.loadKeyLocked()
		e.mu.Unlock()
		return nil, ErrNoKey
	}
	e.mu.Unlock()
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(e.ctx, cancel)()
	vs, err := e.call(ctx, key, texts)
	var se *StatusError
	if errors.As(err, &se) && (se.Code == 401 || se.Code == 403) && e.keyRun != nil {
		// A refused key: run the key command again (within its backoff), as [summaries] does.
		// The key held stays until a new one is printed.
		e.mu.Lock()
		e.loadKeyLocked()
		e.mu.Unlock()
	}
	return vs, err
}

func (e *HTTPEngine) call(ctx context.Context, key string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embeddings server: a bad base_url")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode}
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("embeddings server: %v", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings server: %d vectors for %d texts", len(out.Data), len(texts))
	}
	vs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || vs[d.Index] != nil || len(d.Embedding) == 0 {
			return nil, errors.New("embeddings server: a bad vector index")
		}
		vs[d.Index] = d.Embedding
	}
	return vs, nil
}

// transportError names a failed exchange by its kind only, as [summaries] does: a url.Error's
// text holds the URL, which may carry a query the owner typed.
func transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("embeddings server: %w", context.DeadlineExceeded)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("embeddings server: %w", context.DeadlineExceeded)
	}
	return errors.New("embeddings server: could not be reached")
}

// FakeEngine embeds by hashed words: texts that share words land close. Tests use it; it is
// not semantic.
type FakeEngine struct{ Dims int }

// Embed hashes each lower-case word into one of Dims buckets.
func (f FakeEngine) Embed(_ context.Context, texts []string) ([][]float32, error) {
	dims := f.Dims
	if dims <= 0 {
		dims = 64
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[h.Sum32()%uint32(dims)]++
		}
		out[i] = v
	}
	return out, nil
}

// Vectors is the index of one replica's records: one vector per record that is not deleted, by
// the hash of its text, saved to Path after each change. Sync runs one at a time; it holds the
// index's lock only between engine calls, so a Search never waits for an embed.
type Vectors struct {
	Engine Engine
	Path   string

	syncMu sync.Mutex // one Sync at a time

	mu     sync.Mutex // guards the fields below; never held across an Engine call
	cursor int64
	loaded bool
	recs   map[string]vecEntry
}

// vecEntry is one record's vector. A nil Vec marks a record the engine refused alone: it is
// tried again when its text changes, and never ranked.
type vecEntry struct {
	Hash string
	Vec  []float32
}

type vecFile struct {
	Recs map[string]vecEntry
}

// embedBatch is how many texts go in one Embed call.
const embedBatch = 32

// MaxText caps the text the index embeds for one record, in bytes, cut on a rune boundary.
// Embedding servers refuse inputs past their model's context (8,192 tokens for common models);
// the head of a record carries its meaning.
const MaxText = 8000

// Text is what the index embeds for a record: title, body, tags and notes, at most MaxText bytes.
func Text(r *logbook.Record) string {
	var b strings.Builder
	b.WriteString(r.Title)
	if r.Body != "" {
		b.WriteString("\n" + r.Body)
	}
	if len(r.Tags) > 0 {
		b.WriteString("\n" + strings.Join(r.Tags, " "))
	}
	for _, n := range r.Notes {
		if b.Len() > MaxText {
			break
		}
		b.WriteString("\n" + n.Text)
	}
	t := b.String()
	if len(t) <= MaxText {
		return t
	}
	t = t[:MaxText]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t
}

// refusedAlone reports an engine error that names the input, not the server: a batch that gets
// it is sent again one text at a time, so one record cannot hold up the rest.
func refusedAlone(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusBadRequest || se.Code == http.StatusRequestEntityTooLarge ||
		se.Code == http.StatusUnprocessableEntity)
}

// Sync embeds the records that changed since the last call (every record at the first), and
// drops the deleted and the gone. A record whose text changed loses its vector at once, before
// the new one is asked for: a forgotten text (5.7) leaves the index, memory and file, even while
// the engine is down, and its vector is made again from what is left.
func (v *Vectors) Sync(ctx context.Context, st *logbook.Store) (int, error) {
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	v.mu.Lock()
	if !v.loaded {
		v.load()
		v.loaded = true
	}
	cursor := v.cursor
	v.mu.Unlock()
	var ids []string
	var next int64
	var err error
	if cursor == 0 {
		if next, err = st.Cursor(ctx); err != nil {
			return 0, err
		}
		if ids, err = st.IDs(ctx); err != nil {
			return 0, err
		}
	} else if ids, next, err = st.Changes(ctx, cursor); err != nil {
		return 0, err
	}
	type job struct{ id, hash, text string }
	type state struct {
		gone bool
		hash string
		text string
	}
	states := make(map[string]state, len(ids))
	for _, id := range ids {
		r, err := st.Get(ctx, id)
		if errors.Is(err, logbook.ErrNotFound) || err == nil && r.Deleted {
			states[id] = state{gone: true}
			continue
		}
		if err != nil {
			return 0, err
		}
		t := Text(r)
		sum := sha256.Sum256([]byte(t))
		states[id] = state{hash: hex.EncodeToString(sum[:]), text: t}
	}
	var jobs []job
	changed := false
	v.mu.Lock()
	if cursor == 0 {
		for id := range v.recs {
			if _, ok := states[id]; !ok {
				delete(v.recs, id)
				changed = true
			}
		}
	}
	for _, id := range ids {
		s := states[id]
		e, ok := v.recs[id]
		if ok && !s.gone && e.Hash == s.hash {
			continue
		}
		if ok {
			delete(v.recs, id)
			changed = true
		}
		if !s.gone {
			jobs = append(jobs, job{id, s.hash, s.text})
		}
	}
	var serr error
	if changed {
		serr = v.save()
	}
	v.mu.Unlock()
	if serr != nil {
		return 0, serr
	}
	n := 0
	put := func(part []job, vs [][]float32) {
		v.mu.Lock()
		defer v.mu.Unlock()
		for j, jb := range part {
			v.recs[jb.id] = vecEntry{Hash: jb.hash, Vec: vs[j]}
			if vs[j] != nil {
				n++
			}
		}
	}
	saveNow := func() error {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.save()
	}
	for i := 0; i < len(jobs); i += embedBatch {
		part := jobs[i:min(i+embedBatch, len(jobs))]
		texts := make([]string, len(part))
		for j, jb := range part {
			texts[j] = jb.text
		}
		vs, err := v.Engine.Embed(ctx, texts)
		if err == nil && len(vs) != len(part) {
			err = fmt.Errorf("embeddings: %d vectors for %d texts", len(vs), len(part))
		}
		if err != nil && refusedAlone(err) {
			// One text may be what the server refuses: ask for each alone, and mark the
			// ones it still refuses, so the rest of the index moves on.
			vs, err = make([][]float32, len(part)), nil
			for j := range part {
				one, oerr := v.Engine.Embed(ctx, texts[j:j+1])
				switch {
				case oerr == nil && len(one) == 1:
					vs[j] = one[0]
				case oerr == nil:
					err = fmt.Errorf("embeddings: %d vectors for 1 text", len(one))
				case !refusedAlone(oerr):
					err = oerr
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			if n > 0 {
				_ = saveNow()
			}
			return n, err // the cursor stays: the next call tries these again
		}
		put(part, vs)
	}
	v.mu.Lock()
	v.cursor = next
	v.mu.Unlock()
	if n > 0 || len(jobs) > 0 {
		return n, saveNow()
	}
	return n, nil
}

// Search embeds the words and returns the ids of the k records closest to them, closest first.
func (v *Vectors) Search(ctx context.Context, words string, k int) ([]string, error) {
	q, err := v.Engine.Embed(ctx, []string{words})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(q) != 1 {
		return nil, errors.New("embeddings: no vector for the query")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	type hit struct {
		id    string
		score float64
	}
	var hits []hit
	for id, e := range v.recs {
		if s, ok := cosine(q[0], e.Vec); ok && s > 0 {
			hits = append(hits, hit{id, s})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	out := []string{}
	for _, h := range hits[:min(k, len(hits))] {
		out = append(out, h.id)
	}
	return out, nil
}

func cosine(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	return dot / math.Sqrt(na*nb), true
}

// load reads the saved index; a missing or broken file is an empty index that Sync fills.
func (v *Vectors) load() {
	v.recs = map[string]vecEntry{}
	f, err := os.Open(v.Path)
	if err != nil {
		return
	}
	defer f.Close()
	var vf vecFile
	if gob.NewDecoder(f).Decode(&vf) == nil && vf.Recs != nil {
		v.recs = vf.Recs
	}
}

func (v *Vectors) save() error {
	if v.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(v.Path), 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(vecFile{Recs: v.recs}); err != nil {
		return err
	}
	return writeAtomic(v.Path, b.Bytes())
}
