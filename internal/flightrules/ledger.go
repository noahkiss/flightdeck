// Package flightrules builds flight rules: the per-session ledger of the context that reached
// an agent (docs/specs/instructions-mcp-flight-rules.md 11). Each harness has a Source that
// reads what the harness recorded, in place, and turns it into observations. The Ledger keeps
// digests and sizes, never text; the text is read again from the record when a client asks.
//
// A harness plugs in by implementing Source. The Claude Code source reads its session
// transcript; a source for another harness may run that harness's discovery and match files
// against a recorded block, with the attribution that says so.
package flightrules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
)

// MaxEntries bounds one session's ledger. Past it, new entries are dropped with a warning; a
// repeat of a known entry still updates it.
const MaxEntries = 5000

// MaxText bounds what Text returns.
const MaxText = 1 << 20

// ErrNoEntry: no entry with that id is in the ledger, or the record that carried it is gone.
var ErrNoEntry = errors.New("no flight rules entry with that id")

// Observation is one piece of context a Source saw in what its harness recorded.
type Observation struct {
	Kind   api.FlightRuleKind
	Scope  api.FlightRuleScope
	Source string // a path, hook, skill or server name; for a placeholder, what the record is
	Event  string // a hook's event
	Path   string // the file behind a source that is not a path
	Text   string // the text that reached the agent; digested here, never kept
	// Placeholder marks a record the source knows only by its type: it has no text, digest or
	// size, and its attribution is unknown.
	Placeholder bool
	At          string // RFC 3339 time of the record
	Reason      string
	Attribution api.FlightRuleAttribution
	Note        string
}

// Removal says the harness recorded that a source left the context.
type Removal struct {
	Kinds  []api.FlightRuleKind
	Source string
	At     string
}

// Source reads one session's records for one harness.
type Source interface {
	// Harness names the harness, as detect names it.
	Harness() string
	// Update feeds what the harness recorded since the last call into l. A source whose
	// records were replaced calls l.Reset first and reads them again.
	Update(ctx context.Context, l *Ledger) error
	// Text returns the text of the observation whose entry id is id, read again from the
	// records, or ErrNoEntry.
	Text(ctx context.Context, id string) (string, error)
}

// EntryID is an entry's stable id: kind, source and digest. A placeholder has no digest, so
// one placeholder stands for every record of its type.
func EntryID(kind api.FlightRuleKind, source, digest string) string {
	h := sha256.New()
	for _, f := range []string{string(kind), source, digest} {
		fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Digest is the hex SHA-256 of a text.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ID is the entry id an observation gets.
func (o Observation) ID() string {
	if o.Placeholder {
		return EntryID(o.Kind, o.Source, "")
	}
	return EntryID(o.Kind, o.Source, Digest(o.Text))
}

// Ledger is one session's entries, oldest first. It is not safe for concurrent use; a Session
// guards it.
type Ledger struct {
	// Configured names the configured sources behind a file Flightdeck rendered, from its
	// path and text; nil names none (spec 11.1, configured_source).
	Configured func(path, text string) []string

	entries  []*api.FlightRule
	byID     map[string]*api.FlightRule
	latest   map[string]*api.FlightRule // by kind and source: the newest entry of the source
	warnings []string
	warned   map[string]bool
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	l := &Ledger{}
	l.Reset()
	return l
}

// Reset empties the ledger, for a source that reads its records again from the start.
func (l *Ledger) Reset() {
	l.entries = nil
	l.byID = map[string]*api.FlightRule{}
	l.latest = map[string]*api.FlightRule{}
	l.warnings = nil
	l.warned = map[string]bool{}
}

// Warn adds a warning once per key.
func (l *Ledger) Warn(key, format string, args ...any) {
	if l.warned[key] {
		return
	}
	l.warned[key] = true
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func sourceKey(kind api.FlightRuleKind, source string) string {
	return string(kind) + "\x00" + source
}

// Observe adds an observation: a new entry, or a repeat of a known one, which moves its last
// seen time and brings it back if it was gone.
func (l *Ledger) Observe(o Observation) {
	id := o.ID()
	if e := l.byID[id]; e != nil {
		if o.At != "" && o.At > e.LastSeen {
			e.LastSeen = o.At
		}
		e.Gone = ""
		l.latest[sourceKey(e.Kind, e.Source)] = e
		return
	}
	if len(l.entries) >= MaxEntries {
		l.Warn("full", "the ledger holds %d entries; later ones are left out", MaxEntries)
		return
	}
	e := &api.FlightRule{
		ID: id, Kind: o.Kind, Scope: o.Scope, Source: o.Source, Event: o.Event, Path: o.Path,
		FirstSeen: o.At, LastSeen: o.At, Reason: o.Reason, Attribution: o.Attribution, Note: o.Note,
	}
	if o.Placeholder {
		e.Attribution = api.FlightUnknown
	} else {
		e.Digest = Digest(o.Text)
		e.Bytes = int64(len(o.Text))
		if l.Configured != nil && (o.Kind == api.FlightRuleInstruction || o.Kind == api.FlightRuleMemory) {
			e.ConfiguredSources = l.Configured(o.Source, o.Text)
		}
	}
	if e.Attribution == "" {
		e.Attribution = api.FlightUnknown
	}
	l.entries = append(l.entries, e)
	l.byID[id] = e
	l.latest[sourceKey(e.Kind, e.Source)] = e
}

// Remove marks the newest entry of a source gone.
func (l *Ledger) Remove(r Removal) {
	for _, k := range r.Kinds {
		if e := l.latest[sourceKey(k, r.Source)]; e != nil && e.Gone == "" {
			e.Gone = r.At
			if e.Gone == "" {
				e.Gone = e.LastSeen
			}
		}
	}
}

// Entries returns a copy of the entries, oldest first.
func (l *Ledger) Entries() []api.FlightRule {
	out := make([]api.FlightRule, 0, len(l.entries))
	for _, e := range l.entries {
		c := *e
		c.ConfiguredSources = append([]string(nil), e.ConfiguredSources...)
		out = append(out, c)
	}
	return out
}

// Warnings returns a copy of the warnings, in the order they were raised.
func (l *Ledger) Warnings() []string { return append([]string(nil), l.warnings...) }

// Has says whether the ledger holds an entry with id.
func (l *Ledger) Has(id string) bool { return l.byID[id] != nil }

// Truncate cuts text to at most n bytes on a character boundary.
func Truncate(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n], true
}
