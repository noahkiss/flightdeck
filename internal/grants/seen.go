package grants

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Local Network and Files and Folders cannot be checked without a dialog (7.3). A walk's
// request answers them; gff-hab keeps that answer in <state>/macos/seen.json, keyed by the
// process and its designated requirement, because TCC keys the grant the same way. A later
// --check reports the kept answer, with its date, instead of unknown, so an update to a build
// with the same requirement keeps a passing check (P12). A new requirement (a dev rebuild, a
// new team) finds no answer, as TCC finds no grant. A kept answer is marked remembered, because
// the user may have changed the grant since; the walk asks again.

const seenFile = "seen.json"

type seenEntry struct {
	State  api.PermissionState `json:"state"`
	Detail string              `json:"detail,omitempty"`
	At     string              `json:"at"`
}

func seenKey(proc api.PermissionProcess, requirement, grant string) string {
	h := sha256.Sum256([]byte(normReq(requirement)))
	return string(proc) + " " + hex.EncodeToString(h[:8]) + " " + grant
}

func (s *Service) seenPath() string { return filepath.Join(s.macosDir(), seenFile) }

func (s *Service) loadSeen() map[string]seenEntry {
	m := map[string]seenEntry{}
	if b, err := os.ReadFile(s.seenPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// seen merges a probe's answer for a grant that needs a dialog with the kept answers. A
// request's answer that is granted or denied is kept; an unknown answer is replaced by the kept
// one, when there is one, and remembered is true. The user may have changed the grant since, so
// the walk asks again for each remembered row (7.5).
func (s *Service) seen(proc api.PermissionProcess, requirement string, c Check, requested bool) (_ Check, remembered bool) {
	if s.cfg.StateDir == "" || requirement == "" {
		return c, false
	}
	key := seenKey(proc, requirement, c.Grant)
	m := s.loadSeen()
	if requested {
		if c.State == api.PermissionStateGranted || c.State == api.PermissionStateDenied {
			m[key] = seenEntry{State: c.State, Detail: c.Detail, At: s.cfg.Now().UTC().Format(time.RFC3339)}
			if b, err := json.MarshalIndent(m, "", "  "); err == nil && os.MkdirAll(s.macosDir(), 0o700) == nil {
				writeFileAtomic(s.seenPath(), append(b, '\n'))
			}
		}
		return c, false
	}
	if c.State != api.PermissionStateUnknown {
		return c, false
	}
	e, ok := m[key]
	if !ok {
		return c, false
	}
	at := e.At
	if t, err := time.Parse(time.RFC3339, e.At); err == nil {
		at = t.Local().Format("2006-01-02")
	}
	out := Check{Grant: c.Grant, State: e.State, Detail: "seen by gff setup on " + at}
	if e.Detail != "" {
		out.Detail += ": " + e.Detail
	}
	return out, true
}
