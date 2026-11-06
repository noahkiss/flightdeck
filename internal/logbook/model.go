package logbook

import (
	"encoding/json"
	"slices"
)

// The record types (logbook.md 2.2).
const (
	TypeFinding  = "finding"
	TypeTask     = "task"
	TypePlan     = "plan"
	TypeGuide    = "guide"
	TypeIssue    = "issue"
	TypeProposal = "proposal"
	// TypeProject is a project (3.1): a record of its own kind with a name (its title) and a set
	// of identities. Projects that share an identity are joined on fold (5.6).
	TypeProject = "project"
)

// Types lists the record types this build folds.
var Types = []string{TypeFinding, TypeTask, TypePlan, TypeGuide, TypeIssue, TypeProposal, TypeProject}

// idPrefix is the id prefix of a type, where it differs from the type's name.
var idPrefix = map[string]string{TypeProject: "proj"}

// statuses per type; the first is the status a new record gets. A finding has none.
var statuses = map[string][]string{
	TypeTask:     {"open", "doing", "blocked", "shelved", "done", "dropped"},
	TypeIssue:    {"open", "doing", "blocked", "shelved", "done", "dropped"},
	TypePlan:     {"open", "done", "dropped"},
	TypeGuide:    {"current", "retired"},
	TypeProposal: {"pending", "approved", "edited", "rejected", "expired"},
}

// decisions are the proposal statuses that decide it; the first one written stands (5.6).
var decisions = []string{"approved", "edited", "rejected", "expired"}

// approvals are the decisions that apply a proposal's draft (9.2).
var approvals = []string{"approved", "edited"}

// Decision reports whether status decides a proposal.
func Decision(status string) bool { return slices.Contains(decisions, status) }

// ValidStatus reports whether status belongs to the type.
func ValidStatus(typ, status string) bool { return slices.Contains(statuses[typ], status) }

// fieldKind is how a field folds.
type fieldKind int

const (
	scalarString fieldKind = iota
	scalarBool
	scalarInt
	scalarJSON // any JSON value, kept whole (a proposal's draft)
	setField   // add-wins set (5.5)
	logField   // append log (5.4)
)

type fieldSpec struct {
	kind fieldKind
	// types the field belongs to; nil means every type.
	types []string
	// text fields are what forget redacts and what FTS indexes.
	text bool
}

// fields is this build's field registry. An op naming a field outside it is stored and
// forwarded but not folded (4.4).
var fields = map[string]fieldSpec{
	"type":       {kind: scalarString},
	"project":    {kind: scalarString},
	"title":      {kind: scalarString, text: true},
	"body":       {kind: scalarString, text: true},
	"deleted":    {kind: scalarBool},
	"status":     {kind: scalarString, types: []string{TypeTask, TypeIssue, TypePlan, TypeGuide, TypeProposal}},
	"assignee":   {kind: scalarString, types: []string{TypeTask, TypeIssue}, text: true},
	"triage":     {kind: scalarString, types: []string{TypeTask, TypeIssue}},
	"priority":   {kind: scalarInt, types: []string{TypeTask, TypeIssue}},
	"component":  {kind: scalarString, types: []string{TypeTask, TypeIssue}, text: true},
	"pinned":     {kind: scalarBool, types: []string{TypeGuide}},
	"target":     {kind: scalarString, types: []string{TypeProposal}},
	"draft":      {kind: scalarJSON, types: []string{TypeProposal}, text: true},
	"decided_by": {kind: scalarString, types: []string{TypeProposal}},
	// expires is a memory proposal's end (instructions-mcp-flight-rules.md 12.3): RFC 3339. A
	// decision after it is refused; fixed at create.
	"expires":    {kind: scalarString, types: []string{TypeProposal}},
	"identities": {kind: setField, types: []string{TypeProject}},
	// private hides a project from agents outside it (12.1, stage 5).
	"private": {kind: scalarBool, types: []string{TypeProject}},
	"tags":    {kind: setField},
	"links":   {kind: setField},
	"notes":   {kind: logField},
}

// fieldFor reports the field's spec when this build knows it for the type.
func fieldFor(typ, name string) (fieldSpec, bool) {
	f, ok := fields[name]
	if !ok {
		return f, false
	}
	if f.types != nil && !slices.Contains(f.types, typ) {
		// A task retyped to an issue keeps its fields: the two share every field.
		return f, false
	}
	return f, true
}

// mutable reports whether a local set may change the field on a record of this type (2.2, 5.6).
// The fold applies the same rule to incoming ops.
func mutable(createType, field string) bool {
	switch createType {
	case TypeFinding:
		return field == "project" || field == "deleted"
	case TypeProposal:
		return field == "status" || field == "decided_by" || field == "deleted"
	case TypeProject:
		return field == "title" || field == "body" || field == "deleted" || field == "private"
	}
	if field == "type" {
		return createType == TypeTask || createType == TypeIssue
	}
	return true
}

// The link kinds (2.4).
const (
	LinkSupersedes = "supersedes"
	LinkStage      = "stage"
	LinkPartOf     = "part_of"
	LinkBlocks     = "blocks"
	LinkRelates    = "relates"
	LinkFrom       = "from"
	LinkDuplicates = "duplicates"
)

var linkKinds = []string{LinkSupersedes, LinkStage, LinkPartOf, LinkBlocks, LinkRelates, LinkFrom, LinkDuplicates}

// Link is a member of a record's links set: a typed link to another record by id. Pos orders a
// plan's stages.
type Link struct {
	Kind string `json:"kind"`
	To   string `json:"to"`
	Pos  int    `json:"pos,omitempty"`
}

func (l Link) key() string { return l.Kind + " " + l.To }

// Actor is who asked for a change (2.1, 2.5): an agent through its pane, or a person through a
// client device.
type Actor struct {
	Kind    string `json:"kind,omitempty"` // "agent" or "person"
	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`
	Pane    string `json:"pane,omitempty"`
	Host    string `json:"host,omitempty"`
	Device  string `json:"device,omitempty"`
	// ApprovedBy is the device that approved the proposal this change came from (9.2): the
	// actor stays the proposing agent. Only an approve batch sets it.
	ApprovedBy string `json:"approved_by,omitempty"`
}

// Source is where a record came from: a session, a message and a quote of the user's words.
type Source struct {
	Session string `json:"session,omitempty"`
	Message string `json:"message,omitempty"`
	Quote   string `json:"quote,omitempty"`
}

// Limits from 2.2 and 14.
const (
	MaxTitle = 200
	MaxBody  = 256 << 10
	MaxTag   = 40
)

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
