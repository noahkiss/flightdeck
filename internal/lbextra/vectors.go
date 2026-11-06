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

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Engine turns texts into vectors (13, vector search). The hab ships no model: an engine is a
// local server the user runs, named in [logbook.vectors].
type Engine interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HTTPEngine asks an embeddings server that speaks the OpenAI-compatible
// `POST {"model", "input": [...]}` shape, such as a local model server, for vectors.
type HTTPEngine struct {
	URL    string
	Model  string
	Client *http.Client
}

// Embed sends one request for every text.
func (e HTTPEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := e.Client
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings server: %s", resp.Status)
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
// the hash of its text, saved to Path after each change.
type Vectors struct {
	Engine Engine
	Path   string

	mu     sync.Mutex
	cursor int64
	loaded bool
	recs   map[string]vecEntry
}

type vecEntry struct {
	Hash string
	Vec  []float32
}

type vecFile struct {
	Recs map[string]vecEntry
}

// embedBatch is how many texts go in one Embed call.
const embedBatch = 32

// Text is what the index embeds for a record: title, body, tags and notes.
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
		b.WriteString("\n" + n.Text)
	}
	return b.String()
}

// Sync embeds the records that changed since the last call (every record at the first), and
// drops the deleted and the gone. A forgotten text changes the record's text, so its vector is
// made again from what is left.
func (v *Vectors) Sync(ctx context.Context, st *logbook.Store) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.loaded {
		v.load()
		v.loaded = true
	}
	var ids []string
	var next int64
	var err error
	if v.cursor == 0 {
		if next, err = st.Cursor(ctx); err != nil {
			return 0, err
		}
		if ids, err = st.IDs(ctx); err != nil {
			return 0, err
		}
		for id := range v.recs {
			if !slices.Contains(ids, id) {
				delete(v.recs, id)
			}
		}
	} else if ids, next, err = st.Changes(ctx, v.cursor); err != nil {
		return 0, err
	}
	type job struct{ id, hash, text string }
	var jobs []job
	changed := false
	for _, id := range ids {
		r, err := st.Get(ctx, id)
		if errors.Is(err, logbook.ErrNotFound) || err == nil && r.Deleted {
			if _, ok := v.recs[id]; ok {
				delete(v.recs, id)
				changed = true
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		t := Text(r)
		sum := sha256.Sum256([]byte(t))
		h := hex.EncodeToString(sum[:])
		if e, ok := v.recs[id]; ok && e.Hash == h {
			continue
		}
		jobs = append(jobs, job{id, h, t})
	}
	n := 0
	for i := 0; i < len(jobs); i += embedBatch {
		part := jobs[i:min(i+embedBatch, len(jobs))]
		texts := make([]string, len(part))
		for j, jb := range part {
			texts[j] = jb.text
		}
		vs, err := v.Engine.Embed(ctx, texts)
		if err != nil {
			if changed || n > 0 {
				_ = v.save()
			}
			return n, err // the cursor stays: the next call tries these again
		}
		if len(vs) != len(part) {
			return n, fmt.Errorf("embeddings: %d vectors for %d texts", len(vs), len(part))
		}
		for j, jb := range part {
			v.recs[jb.id] = vecEntry{Hash: jb.hash, Vec: vs[j]}
			n++
		}
	}
	v.cursor = next
	if changed || n > 0 {
		return n, v.save()
	}
	return n, nil
}

// Search embeds the words and returns the ids of the k records closest to them, closest first.
func (v *Vectors) Search(ctx context.Context, words string, k int) ([]string, error) {
	q, err := v.Engine.Embed(ctx, []string{words})
	if err != nil {
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
