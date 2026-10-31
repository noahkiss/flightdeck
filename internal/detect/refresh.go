package detect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultCatalog is herdr's public catalogue index. Each entry's path is relative to it.
const DefaultCatalog = "https://herdr.dev/agent-detection/index.toml"

// MaxManifestBytes bounds one fetched file.
const MaxManifestBytes = 256 << 10

// RefreshOptions configure Refresh.
type RefreshOptions struct {
	// From is the catalogue: an https URL of an index.toml, a local index.toml, or a local
	// directory holding one. Empty means DefaultCatalog. A local catalogue works offline.
	// Plain http is refused, and so is a redirect to anything but https.
	From string
	// Dir is the data directory the new manifests go into. Load(Dir) gives the current set.
	Dir string
	// DryRun computes the changes and writes nothing.
	DryRun bool
	// Client fetches https catalogues. Nil means a client with a 30 s timeout. Refresh sets its
	// redirect check on a copy, so a redirect never leaves https.
	Client *http.Client
}

// Change is one manifest that a refresh replaces or adds.
type Change struct {
	ID         string
	OldVersion string // empty when the set had no manifest for this id
	NewVersion string
	OldFrom    string // where the current copy came from (vendored or data)
	Diff       string // unified diff from the current copy to the new one
	data       []byte
}

// RefreshResult says what a refresh did or, with DryRun, would do.
type RefreshResult struct {
	Changes   []Change
	Unchanged []string  // ids whose catalogue version equals the current one
	Skipped   []Problem // entries not used: older than the current copy, unusable, or unreadable
}

type catalog struct {
	SchemaVersion int `toml:"schema_version"`
	Agents        []struct {
		ID   string `toml:"id"`
		Path string `toml:"path"`
	} `toml:"agents"`
}

// Refresh reads the catalogue, compares every manifest in it with the current set from
// Load(opts.Dir), and writes each newer, usable one into opts.Dir. It never downgrades: a
// catalogue manifest older than the current copy is skipped. A manifest that needs a newer
// engine, or fails to parse, is skipped and the current copy stays. Each file is written to a
// temporary name and renamed into place, so a failed refresh leaves the last good copy.
//
// It fails as a whole only when the catalogue index cannot be read or is malformed.
func Refresh(ctx context.Context, opts RefreshOptions) (RefreshResult, error) {
	var res RefreshResult
	if opts.Dir == "" {
		return res, errors.New("refresh: no data directory")
	}
	from := opts.From
	if from == "" {
		from = DefaultCatalog
	}
	fetch, indexName, err := fetcher(from, opts.Client)
	if err != nil {
		return res, err
	}
	raw, err := fetch(ctx, indexName)
	if err != nil {
		return res, fmt.Errorf("refresh: catalogue: %w", err)
	}
	var cat catalog
	md, err := toml.Decode(string(raw), &cat)
	if err != nil {
		return res, fmt.Errorf("refresh: catalogue: %w", err)
	}
	if len(md.Undecoded()) > 0 {
		return res, fmt.Errorf("refresh: catalogue has unknown keys %v", md.Undecoded())
	}
	if cat.SchemaVersion != 1 {
		return res, fmt.Errorf("refresh: catalogue schema_version %d is not supported", cat.SchemaVersion)
	}

	current := Load(opts.Dir)
	seen := map[string]bool{}
	for _, a := range cat.Agents {
		id, p := strings.TrimSpace(a.ID), strings.TrimSpace(a.Path)
		if id == "" || !safeRelPath(p) {
			return res, fmt.Errorf("refresh: catalogue entry %q has an unsafe or empty path %q", a.ID, a.Path)
		}
		if seen[id] {
			return res, fmt.Errorf("refresh: catalogue lists %s twice", id)
		}
		seen[id] = true

		data, err := fetch(ctx, p)
		if err != nil {
			res.Skipped = append(res.Skipped, Problem{p, err.Error()})
			continue
		}
		m, err := Parse(data)
		if err != nil {
			res.Skipped = append(res.Skipped, Problem{p, err.Error()})
			continue
		}
		if err := plausibleVersion(m.Version, time.Now()); err != nil {
			res.Skipped = append(res.Skipped, Problem{p, err.Error()})
			continue
		}
		if m.ID != id {
			res.Skipped = append(res.Skipped, Problem{p, fmt.Sprintf("manifest id %q does not match catalogue id %q", m.ID, id)})
			continue
		}
		cur := current.entries[m.ID]
		ch := Change{ID: m.ID, NewVersion: m.Version, data: data}
		var oldRaw []byte
		if cur != nil {
			switch c := CompareVersions(m.Version, cur.Manifest.Version); {
			case c == 0:
				res.Unchanged = append(res.Unchanged, m.ID)
				continue
			case c < 0:
				res.Skipped = append(res.Skipped, Problem{p, fmt.Sprintf("version %s is older than the current %s; not downgrading", m.Version, cur.Manifest.Version)})
				continue
			}
			ch.OldVersion, ch.OldFrom, oldRaw = cur.Manifest.Version, cur.From, cur.Manifest.Raw
		}
		ch.Diff = UnifiedDiff(m.ID+".toml "+orNone(ch.OldVersion), m.ID+".toml "+m.Version, string(oldRaw), string(data))
		res.Changes = append(res.Changes, ch)
	}
	sort.Slice(res.Changes, func(i, j int) bool { return res.Changes[i].ID < res.Changes[j].ID })
	if opts.DryRun || len(res.Changes) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return res, err
	}
	for _, ch := range res.Changes {
		name := filepath.Join(opts.Dir, ch.ID+".toml")
		if filepath.Dir(name) != filepath.Clean(opts.Dir) {
			return res, fmt.Errorf("refresh: manifest id %q would write outside %s", ch.ID, opts.Dir)
		}
		if err := writeAtomic(name, ch.data); err != nil {
			return res, fmt.Errorf("refresh: write %s: %w", ch.ID, err)
		}
	}
	return res, nil
}

// plausibleVersion refuses a date-shaped version (first segment 1000 or more, as in 2026.10.11.1)
// from after next year. Versions only ever rise, so one absurd version would otherwise outrank
// every later vendored or fetched copy for good.
func plausibleVersion(v string, now time.Time) error {
	first, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	n, err := strconv.Atoi(first)
	if err == nil && n >= 1000 && n > now.Year()+1 {
		return fmt.Errorf("version %s is dated after %d; refusing it", v, now.Year()+1)
	}
	return nil
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// safeRelPath accepts a plain relative path with no scheme, no leading slash and no "..".
func safeRelPath(p string) bool {
	if p == "" || strings.Contains(p, "://") || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}

type fetchFunc func(ctx context.Context, rel string) ([]byte, error)

// fetcher returns a function that reads a file relative to the catalogue, and the index's name.
func fetcher(from string, client *http.Client) (fetchFunc, string, error) {
	if strings.Contains(from, "://") {
		u, err := url.Parse(from)
		if err != nil {
			return nil, "", fmt.Errorf("refresh: catalogue URL: %w", err)
		}
		if u.Scheme != "https" {
			return nil, "", fmt.Errorf("refresh: catalogue %s: only https URLs or local paths are allowed", from)
		}
		c := http.Client{Timeout: 30 * time.Second}
		if client != nil {
			c = *client
		}
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL)
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		}
		client = &c
		index := path.Base(u.Path)
		base := *u
		base.Path = path.Dir(u.Path) + "/"
		base.RawQuery, base.Fragment = "", ""
		return func(ctx context.Context, rel string) ([]byte, error) {
			target := base.ResolveReference(&url.URL{Path: rel})
			if rel == index {
				target = u
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("%s: %s", target, resp.Status)
			}
			return readCapped(resp.Body, target.String())
		}, index, nil
	}
	dir, index := from, "index.toml"
	if fi, err := os.Stat(from); err != nil {
		return nil, "", fmt.Errorf("refresh: catalogue: %w", err)
	} else if !fi.IsDir() {
		dir, index = filepath.Dir(from), filepath.Base(from)
	}
	return func(_ context.Context, rel string) ([]byte, error) {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readCapped(f, rel)
	}, index, nil
}

func readCapped(r io.Reader, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxManifestBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, MaxManifestBytes)
	}
	return b, nil
}

func writeAtomic(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = io.Copy(f, bytes.NewReader(data))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, name)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
