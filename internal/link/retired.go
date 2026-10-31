package link

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// retiredFile is the file in the state directory that keeps the retired pane uuids across
// restarts.
const retiredFile = "retired.json"

// retiredVersion is the format version of the retired file.
const retiredVersion = 1

// retiredDoc is the retired file: each retired pane uuid with the time it was retired.
type retiredDoc struct {
	Version int                  `json:"v"`
	Retired map[string]time.Time `json:"retired"`
}

// loadRetired reads the retired uuids in dir and drops those retired more than ttl before now.
// A missing file is an empty set. pruned reports whether the file held entries it dropped, or
// was unusable, so the caller rewrites it.
func loadRetired(dir string, ttl time.Duration, now time.Time) (set map[hp.UUID]time.Time, pruned bool, err error) {
	set = map[hp.UUID]time.Time{}
	b, err := os.ReadFile(filepath.Join(dir, retiredFile))
	if errors.Is(err, os.ErrNotExist) {
		return set, false, nil
	}
	if err != nil {
		return set, true, err
	}
	var doc retiredDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return set, true, fmt.Errorf("%s: %w", retiredFile, err)
	}
	if doc.Version != retiredVersion {
		return set, true, fmt.Errorf("%s: version %d, want %d", retiredFile, doc.Version, retiredVersion)
	}
	for s, at := range doc.Retired {
		u, err := parseUUID(s)
		if err != nil || now.Sub(at) >= ttl {
			pruned = true
			continue
		}
		set[u] = at
	}
	return set, pruned, nil
}

// saveRetired writes set to dir atomically, without the entries retired more than ttl before now.
func saveRetired(dir string, set map[hp.UUID]time.Time, ttl time.Duration, now time.Time) error {
	doc := retiredDoc{Version: retiredVersion, Retired: make(map[string]time.Time, len(set))}
	for u, at := range set {
		if now.Sub(at) < ttl {
			doc.Retired[u.String()] = at.UTC()
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, retiredFile, append(b, '\n'))
}

// parseUUID parses the 8-4-4-4-12 hex form that hp.UUID.String writes.
func parseUUID(s string) (hp.UUID, error) {
	var u hp.UUID
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 || len(s) != 36 {
		return u, fmt.Errorf("bad uuid %q", s)
	}
	if _, err := hex.Decode(u[:], []byte(h)); err != nil {
		return u, fmt.Errorf("bad uuid %q", s)
	}
	return u, nil
}
