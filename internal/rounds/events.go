package rounds

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The hab events an event trigger can name (rounds.md 4.3). Each comes from the runner's own
// hab only.
const (
	EventProjectUpdated = "project.updated"
	EventAgentState     = "agent.state"
	EventCommandUpdated = "command.updated"
	EventHabWoke        = "hab.woke"
	EventNoticeRaised   = "notice.raised"
	EventLogbookUpdated = "logbook.updated"
)

// Events lists the event names, in the spec's order.
var Events = []string{EventProjectUpdated, EventAgentState, EventCommandUpdated, EventHabWoke, EventNoticeRaised, EventLogbookUpdated}

// EventFilter is an event trigger's filter: which events of its kind make a run (rounds.md 4.3).
// Each event takes only its own fields; an empty field matches everything.
type EventFilter struct {
	// Project is a project id: project.updated, agent.state, notice.raised, logbook.updated.
	Project string `json:"project,omitempty"`
	// Session is a session id: agent.state, command.updated, notice.raised.
	Session string `json:"session,omitempty"`
	// Field is project.updated's changed fields: head, branch, behind.
	Field []string `json:"field,omitempty"`
	// State is agent.state's new states: done, error.
	State []string `json:"state,omitempty"`
	// Level is notice.raised's lowest level: passive, active, time-sensitive.
	Level string `json:"level,omitempty"`
	// Text is notice.raised's case-insensitive part of the title or body.
	Text string `json:"text,omitempty"`
	// Type, Change, Status, Tags and By are logbook.updated's: the record type; created (the
	// default), status, deleted or any; for status, the new statuses; any of these tags; and
	// person, agent, hab or any (the default).
	Type   string   `json:"type,omitempty"`
	Change string   `json:"change,omitempty"`
	Status []string `json:"status,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	By     string   `json:"by,omitempty"`
}

// Logbook change words of an event filter.
const (
	ChangeCreated = "created"
	ChangeStatus  = "status"
	ChangeDeleted = "deleted"
	ChangeAny     = "any"
)

// Actor kinds of the by filter: who wrote a Logbook op (logbook.md 3.3).
const (
	ByPerson = "person"
	ByAgent  = "agent"
	ByHab    = "hab"
	ByAny    = "any"
)

// NoticeLevels are notice.raised's levels, lowest first.
var NoticeLevels = []string{"passive", "active", "time-sensitive"}

// LevelRank is a notice level's place in NoticeLevels, or -1.
func LevelRank(level string) int { return slices.Index(NoticeLevels, level) }

// eventFields names, per event, the filter fields it takes.
var eventFields = map[string][]string{
	EventProjectUpdated: {"project", "field"},
	EventAgentState:     {"project", "session", "state"},
	EventCommandUpdated: {"session"},
	EventHabWoke:        {},
	EventNoticeRaised:   {"project", "session", "level", "text"},
	EventLogbookUpdated: {"project", "type", "change", "status", "tags", "by"},
}

// set lists the filter's fields that hold a value.
func (f *EventFilter) set() []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, x := range []struct {
		name string
		on   bool
	}{{"project", f.Project != ""}, {"session", f.Session != ""}, {"field", len(f.Field) > 0},
		{"state", len(f.State) > 0}, {"level", f.Level != ""}, {"text", f.Text != ""},
		{"type", f.Type != ""}, {"change", f.Change != ""}, {"status", len(f.Status) > 0},
		{"tags", len(f.Tags) > 0}, {"by", f.By != ""}} {
		if x.on {
			out = append(out, x.name)
		}
	}
	return out
}

// checkFilter checks an event trigger's event and filter.
func checkFilter(event string, f *EventFilter) error {
	allowed, ok := eventFields[event]
	if !ok {
		if event == "" {
			return fmt.Errorf("event: required for an event trigger; one of %s", strings.Join(Events, ", "))
		}
		return fmt.Errorf("event %q: want one of %s", event, strings.Join(Events, ", "))
	}
	for _, name := range f.set() {
		if !slices.Contains(allowed, name) {
			if len(allowed) == 0 {
				return fmt.Errorf("filter: %s takes no filter", event)
			}
			return fmt.Errorf("filter: %s is not a field of %s; it takes %s", name, event, strings.Join(allowed, ", "))
		}
	}
	if f == nil {
		return nil
	}
	words := func(field string, got, want []string) error {
		for _, w := range got {
			if !slices.Contains(want, w) {
				return fmt.Errorf("filter: %s %q: want %s", field, w, strings.Join(want, ", "))
			}
		}
		return nil
	}
	if err := words("field", f.Field, []string{"head", "branch", "behind"}); err != nil {
		return err
	}
	if err := words("state", f.State, []string{"done", "error"}); err != nil {
		return err
	}
	if f.Level != "" && LevelRank(f.Level) < 0 {
		return fmt.Errorf("filter: level %q: want %s", f.Level, strings.Join(NoticeLevels, ", "))
	}
	if f.Change != "" && !slices.Contains([]string{ChangeCreated, ChangeStatus, ChangeDeleted, ChangeAny}, f.Change) {
		return fmt.Errorf("filter: change %q: want created, status, deleted or any", f.Change)
	}
	if len(f.Status) > 0 && f.Change != ChangeStatus {
		return fmt.Errorf("filter: status is for change status only")
	}
	if f.By != "" && !slices.Contains([]string{ByPerson, ByAgent, ByHab, ByAny}, f.By) {
		return fmt.Errorf("filter: by %q: want person, agent, hab or any", f.By)
	}
	for _, x := range [][]string{f.Field, f.State, f.Status, f.Tags} {
		for _, w := range x {
			if w == "" {
				return fmt.Errorf("filter: an empty word in a list")
			}
		}
	}
	return nil
}

// equalFilter compares two filters by value.
func equalFilter(a, b *EventFilter) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if a == nil || len(a.set()) == 0 {
		ja = []byte("null")
	}
	if b == nil || len(b.set()) == 0 {
		jb = []byte("null")
	}
	return string(ja) == string(jb)
}

// Event is one event of a batch (rounds.md 4.5): a hab event that matched a round's filter.
type Event struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Title string    `json:"title"`
	// Level is a notice's level, the batch's level hint (rounds.md 5.2); empty for others.
	Level string `json:"level,omitempty"`
	// Data is the event's own fields, as JSON.
	Data json.RawMessage `json:"data,omitempty"`
}

// size is the event's share of a batch's byte bound.
func (e Event) size() int { return len(e.Kind) + len(e.Title) + len(e.Level) + len(e.Data) + 64 }

// Batch is a list of events and how many older ones were dropped to keep it inside its bounds.
type Batch struct {
	Events  []Event `json:"events,omitempty"`
	Dropped int     `json:"dropped,omitempty"`
}

// Len is how many events the batch served: those it holds and those it dropped.
func (b Batch) Len() int { return len(b.Events) + b.Dropped }

// Level is the highest notice level in the batch, or "".
func (b Batch) Level() string {
	best := ""
	for _, e := range b.Events {
		if LevelRank(e.Level) > LevelRank(best) {
			best = e.Level
		}
	}
	return best
}

// Cap drops the oldest events past maxEvents or maxBytes, and counts them.
func (b *Batch) Cap(maxEvents, maxBytes int) {
	n := 0
	for _, e := range b.Events {
		n += e.size()
	}
	drop := 0
	for len(b.Events)-drop > maxEvents || (n > maxBytes && drop < len(b.Events)) {
		n -= b.Events[drop].size()
		drop++
	}
	if drop > 0 {
		b.Events = append([]Event(nil), b.Events[drop:]...)
		b.Dropped += drop
	}
}

// Pending is an event round's pending batch and its timing (rounds.md 4.5), kept in the round's
// state.
type Pending struct {
	Batch
	// Opened is when the first event of the batch arrived: the run is due at Opened plus the
	// round's debounce, or at the cooldown's end when that is later.
	Opened *time.Time `json:"opened,omitempty"`
}

// KeepNoFiles is Limits.EventFilesKeep for "keep no event files": zero takes the default.
const KeepNoFiles = -1

// Limits are the batch bounds of rounds.md 4.7. A zero field takes its default.
type Limits struct {
	BatchMaxEvents int
	BatchMaxBytes  int
	EventFilesKeep int
	StateSaveEvery time.Duration
}

// Defaults of rounds.md 4.7.
const (
	DefaultBatchMaxEvents = 200
	DefaultBatchMaxBytes  = 256 << 10
	DefaultEventFilesKeep = 20
	DefaultStateSaveEvery = 10 * time.Second
)

// withDefaults fills unset limits.
func (l Limits) withDefaults() Limits {
	if l.BatchMaxEvents <= 0 {
		l.BatchMaxEvents = DefaultBatchMaxEvents
	}
	if l.BatchMaxBytes <= 0 {
		l.BatchMaxBytes = DefaultBatchMaxBytes
	}
	switch {
	case l.EventFilesKeep == 0:
		l.EventFilesKeep = DefaultEventFilesKeep
	case l.EventFilesKeep < 0: // KeepNoFiles
		l.EventFilesKeep = 0
	}
	if l.StateSaveEvery <= 0 {
		l.StateSaveEvery = DefaultStateSaveEvery
	}
	return l
}
