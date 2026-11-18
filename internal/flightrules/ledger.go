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
	"slices"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/instructions"
)

// MaxEntries bounds one session's ledger, hook entries apart. Past it, new entries are dropped
// with a warning; a repeat of a known entry still updates it.
const MaxEntries = 4000

// MaxHookEntries bounds a ledger's hook entries on their own: a hook whose text changes on every
// call fills this, never the room for instruction files.
const MaxHookEntries = 1000

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
	// Repeats marks a source whose text changes from load to load for reasons that are not a
	// new source: a hook that prints a clock, a date, an environment snapshot. The ledger keeps
	// one entry per kind, source and event, with the latest text and a count, instead of a
	// new entry per text.
	Repeats     bool
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
	// ID, when set, marks only that entry gone, not every text of the source.
	ID string
}

// Source reads one session's records for one harness.
type Source interface {
	// Harness names the harness, as detect names it.
	Harness() string
	// Update feeds what the harness recorded since the last call into l. A source whose
	// records were replaced calls l.Reset first and reads them again.
	Update(ctx context.Context, l *Ledger) error
	// Text returns the text of the observation whose entry id is id, read again from the
	// records, or ErrNoEntry. It may run at the same time as Update.
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
	if o.Repeats {
		return EntryID(o.Kind, o.Source+"\x00"+o.Event, "latest")
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
	hooks    int                        // hook entries, counted against MaxHookEntries
	headers  map[string]string          // by entry id: the render digest in a rendered file's header
	warnings []string
	warnKeys []string // the key of each warning, for Unwarn
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
	l.hooks = 0
	l.headers = map[string]string{}
	l.warnings, l.warnKeys = nil, nil
	l.warned = map[string]bool{}
}

// Warn adds a warning once per key.
func (l *Ledger) Warn(key, format string, args ...any) {
	if l.warned[key] {
		return
	}
	l.warned[key] = true
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
	l.warnKeys = append(l.warnKeys, key)
}

func sourceKey(kind api.FlightRuleKind, source string) string {
	return string(kind) + "\x00" + source
}

// Observe adds an observation: a new entry, or a repeat of a known one, which moves its last
// seen time and brings it back if it was gone.
func (l *Ledger) Observe(o Observation) {
	id := o.ID()
	if e := l.byID[id]; e != nil {
		e.Count++
		if o.At == "" || o.At >= e.LastSeen {
			if o.At != "" {
				e.LastSeen = o.At
			}
			if o.Repeats && !o.Placeholder {
				// The newest text stands for the source.
				e.Digest, e.Bytes, e.Note = Digest(o.Text), int64(len(o.Text)), o.Note
			}
		}
		e.Gone = ""
		l.latest[sourceKey(e.Kind, e.Source)] = e
		return
	}
	if o.Kind == api.FlightRuleHook {
		if l.hooks >= MaxHookEntries {
			l.Warn("full-hooks", "the ledger holds %d hook entries; later ones are left out", MaxHookEntries)
			return
		}
	} else if len(l.entries)-l.hooks >= MaxEntries {
		l.Warn("full", "the ledger holds %d entries besides hooks; later ones are left out", MaxEntries)
		return
	}
	if o.Kind == api.FlightRuleHook {
		l.hooks++
	}
	e := &api.FlightRule{
		ID: id, Kind: o.Kind, Scope: o.Scope, Source: o.Source, Event: o.Event, Path: o.Path,
		FirstSeen: o.At, LastSeen: o.At, Reason: o.Reason, Attribution: o.Attribution, Note: o.Note,
		Count: 1,
	}
	if o.Placeholder {
		e.Attribution = api.FlightUnknown
	} else {
		e.Digest = Digest(o.Text)
		e.Bytes = int64(len(o.Text))
		if o.Kind == api.FlightRuleInstruction || o.Kind == api.FlightRuleMemory {
			if l.Configured != nil {
				e.ConfiguredSources = l.Configured(o.Source, o.Text)
			}
			if d := instructions.HeaderDigest(o.Text); d != "" {
				l.headers[id] = d
			}
		}
	}
	if e.Attribution == "" {
		e.Attribution = api.FlightUnknown
	}
	l.entries = append(l.entries, e)
	l.byID[id] = e
	l.latest[sourceKey(e.Kind, e.Source)] = e
}

// Remove marks every entry of a source gone: its newest text and any earlier one.
func (l *Ledger) Remove(r Removal) {
	for _, e := range l.entries {
		if e.Source != r.Source || e.Gone != "" || !slices.Contains(r.Kinds, e.Kind) || r.ID != "" && e.ID != r.ID {
			continue
		}
		e.Gone = r.At
		if e.Gone == "" {
			e.Gone = e.LastSeen
		}
	}
}

// MarkStale sets each entry's stale flag against the last render of one target (spec 11.4):
// the newest entry of the target's source, still in the context, whose header digest differs
// from the render's. Every other entry is not stale. ok false clears every flag.
func (l *Ledger) MarkStale(r instructions.Rendered, ok bool) {
	for _, e := range l.entries {
		e.Stale = false
		d := l.headers[e.ID]
		if !ok || d == "" || e.Gone != "" || l.latest[sourceKey(e.Kind, e.Source)] != e {
			continue
		}
		e.Stale = d != r.Digest && samePath(e.Source, r.Target)
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

// Unwarn drops the warning of a key, for a condition that has passed.
func (l *Ledger) Unwarn(key string) {
	if !l.warned[key] {
		return
	}
	delete(l.warned, key)
	i := slices.Index(l.warnKeys, key)
	l.warnings = slices.Delete(l.warnings, i, i+1)
	l.warnKeys = slices.Delete(l.warnKeys, i, i+1)
}

// Has says whether the ledger holds an entry with id.
func (l *Ledger) Has(id string) bool { return l.byID[id] != nil }

// Warnings returns a copy of the warnings, in the order they were raised.
func (l *Ledger) Warnings() []string { return append([]string(nil), l.warnings...) }

// HasText says whether the ledger holds an entry with id that has text: a placeholder has none.
func (l *Ledger) HasText(id string) bool { e := l.byID[id]; return e != nil && e.Digest != "" }

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
