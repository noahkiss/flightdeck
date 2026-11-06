package logbook

import (
	"encoding/json"
	"slices"
	"strings"
)

// The flag kinds a folded record can carry.
const (
	FlagConflict          = "conflict"            // two set ops on one field, neither saw the other (5.2)
	FlagEditedAfterDelete = "edited_after_delete" // 5.6
	FlagIDCollision       = "id_collision"        // 2.3
	FlagDecidedTwice      = "decided_twice"       // 5.6
	FlagSupersededTwice   = "superseded_twice"    // 5.6; derived when the record is read
	// FlagApprovalOverruled: an approve of a proposal lost to a decision stamped before it (9.3).
	// A record the approve made is hidden like a deleted one; a change it made to another record
	// is not folded.
	FlagApprovalOverruled = "approval_overruled"
)

// overrule names, for one record, its ops that came in an approve batch that lost (9.3), each
// with the decision that won on the proposal.
type overrule map[OpID]*Op

// FieldValue is a folded scalar: its value, the op that set it, and the field's heads, the set
// ops no other set op on the field saw. A local set names the heads as its prev.
type FieldValue struct {
	Value json.RawMessage `json:"value"`
	Op    OpID            `json:"op"`
	Heads []OpID          `json:"heads"`
}

// Member is one member of a set field. Adds are every add op of the member that no remove
// named; a remove of the member names them.
type Member struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
	Op    OpID            `json:"op"`
	Adds  []OpID          `json:"adds"`
}

// Note is one entry of a record's notes.
type Note struct {
	Op    OpID   `json:"op"`
	At    HLC    `json:"at"`
	Text  string `json:"text"`
	Actor *Actor `json:"actor,omitempty"`
}

// FlagValue is one side of a flag: the op, its value, who made it and where.
type FlagValue struct {
	Op     OpID            `json:"op"`
	At     HLC             `json:"at"`
	Value  json.RawMessage `json:"value,omitempty"`
	Actor  *Actor          `json:"actor,omitempty"`
	Origin string          `json:"origin"`
}

// Flag marks something the user should look at.
type Flag struct {
	Kind   string      `json:"kind"`
	Field  string      `json:"field,omitempty"`
	Values []FlagValue `json:"values,omitempty"`
}

// folded is one record as the fold computes it: a pure function of its create op, the applied
// ops on it, and its place among creates of the same id.
type folded struct {
	Root      OpID                  `json:"root"`
	ID        string                `json:"id"`
	Type      string                `json:"type"`
	Created   HLC                   `json:"created"`
	CreatedBy *Actor                `json:"created_by,omitempty"`
	Source    json.RawMessage       `json:"source,omitempty"`
	Fields    map[string]FieldValue `json:"fields"`
	Sets      map[string][]Member   `json:"sets"`
	Notes     []Note                `json:"notes"`
	Flags     []Flag                `json:"flags"`
	// Revision counts the folded ops, the create included, and the forgets of them, so it never
	// goes down and moves on a redaction.
	Revision int `json:"revision"`
	// Skipped lists ops on the record a rule kept out of the fold (a set of a finding's body).
	Skipped []OpID `json:"-"`
}

func (f *folded) str(name string) string {
	var s string
	if v, ok := f.Fields[name]; ok {
		_ = json.Unmarshal(v.Value, &s)
	}
	return s
}

func (f *folded) boolean(name string) bool {
	var b bool
	if v, ok := f.Fields[name]; ok {
		_ = json.Unmarshal(v.Value, &b)
	}
	return b
}

// fold folds one record. create is its create op; ops are the other applied, known ops whose root
// is the create, in any order; collision is the create's place among applied creates of the same
// id by stamp (0 keeps the id); ov names the record's ops whose approve lost (9.3).
func fold(create *Op, ops []*Op, collision int, ov overrule) *folded {
	var cv createValue
	_ = json.Unmarshal(create.Value, &cv)
	f := &folded{
		Root:      create.ID(),
		ID:        collisionID(create.Record, collision),
		Type:      cv.Type,
		Created:   create.HLC(),
		CreatedBy: create.Actor,
		Fields:    map[string]FieldValue{},
		Sets:      map[string][]Member{},
		Notes:     []Note{},
		Flags:     []Flag{},
		Revision:  1,
	}
	if len(cv.Source) > 0 && string(cv.Source) != "null" {
		f.Source = cv.Source
	}
	createType := cv.Type
	if collision > 0 {
		f.Flags = append(f.Flags, Flag{Kind: FlagIDCollision})
	}
	ops = slices.Clone(ops)
	slices.SortFunc(ops, opCmp)

	sets := map[string][]*Op{} // scalar field -> set ops, create excluded
	adds := map[string][]*Op{} // set field -> adds
	removed := map[OpID]bool{} // adds named by a remove
	var appends []*Op
	var overruledBy []*Op // the winning decisions over this record's overruled ops
	mine := map[OpID]bool{f.Root: true}
	for _, o := range ops {
		if o.Root == f.Root {
			mine[o.ID()] = true
		}
	}
	for _, o := range ops {
		if o.Root == f.Root && o.Kind == KindForget && o.Target != nil && mine[*o.Target] {
			f.Revision++ // a redaction of one of the record's ops
			continue
		}
		if o.Root != f.Root || o.Kind == KindCreate || o.Kind == KindForget {
			continue
		}
		// A proposal's decision fields are never overruled: its standing decision is the first
		// decision among all its status sets (standing), so it never waits on another proposal's.
		decision := createType == TypeProposal && o.Kind == KindSet && (o.Field == "status" || o.Field == "decided_by")
		if win, ok := ov[o.ID()]; ok && !decision {
			// A change an overruled approve made: counted, so the revision never goes down, but
			// not folded.
			f.Revision++
			overruledBy = append(overruledBy, win)
			continue
		}
		if _, ok := fieldFor(createType, o.Field); !ok {
			f.Skipped = append(f.Skipped, o.ID())
			continue
		}
		switch o.Kind {
		case KindSet:
			if !mutable(createType, o.Field) || (o.Field == "type" && !retypeOK(o.Value)) {
				f.Skipped = append(f.Skipped, o.ID())
				continue
			}
			sets[o.Field] = append(sets[o.Field], o)
		case KindAdd:
			adds[o.Field] = append(adds[o.Field], o)
		case KindRemove:
			for _, p := range o.Prev {
				removed[p] = true
			}
		case KindAppend:
			appends = append(appends, o)
		}
		f.Revision++
	}

	// Scalars: the create's value is the first write of each field it names.
	names := map[string]bool{}
	for n := range cv.Fields {
		names[n] = true
	}
	for n := range sets {
		names[n] = true
	}
	var deleteOp *Op
	for _, name := range sortedKeys(names) {
		cands := slices.Clone(sets[name])
		if v, ok := cv.Fields[name]; ok {
			c := *create
			c.Value = v
			c.Prev = nil
			cands = append(cands, &c)
		}
		slices.SortFunc(cands, opCmp)
		var win *Op
		heads := heads(cands)
		if createType == TypeProposal && (name == "status" || name == "decided_by") {
			win = f.decide(name, cands, sets["status"])
		} else {
			win = cands[len(cands)-1]
			if len(heads) > 1 {
				fl := Flag{Kind: FlagConflict, Field: name}
				for _, h := range heads {
					fl.Values = append(fl.Values, flagValue(h))
				}
				f.Flags = append(f.Flags, fl)
			}
		}
		fv := FieldValue{Value: win.Value, Op: win.ID()}
		for _, h := range heads {
			fv.Heads = append(fv.Heads, h.ID())
		}
		f.Fields[name] = fv
		if name == "deleted" {
			deleteOp = win
		}
	}
	if _, ok := f.Fields["type"]; !ok {
		f.Fields["type"] = FieldValue{Value: jsonString(createType), Op: f.Root, Heads: []OpID{f.Root}}
	}
	f.Type = f.str("type")

	// Sets: add wins; a remove takes away only the adds it names.
	for name, list := range adds {
		byKey := map[string]*Member{}
		for _, a := range list {
			if removed[a.ID()] {
				continue
			}
			k := memberKey(name, a.Value)
			m, ok := byKey[k]
			if !ok {
				m = &Member{Key: k}
				byKey[k] = m
			}
			m.Adds = append(m.Adds, a.ID())
			m.Value, m.Op = a.Value, a.ID() // list is in stamp order: the last add's value stands
		}
		var ms []Member
		for _, k := range sortedKeys(byKey) {
			ms = append(ms, *byKey[k])
		}
		if len(ms) > 0 {
			f.Sets[name] = ms
		}
	}

	for _, a := range appends {
		var text string
		_ = json.Unmarshal(a.Value, &text)
		f.Notes = append(f.Notes, Note{Op: a.ID(), At: a.HLC(), Text: text, Actor: a.Actor})
	}

	// Delete: an edit stamped after the winning delete is folded but the record stays deleted.
	if deleteOp != nil && f.boolean("deleted") {
		var after []FlagValue
		for _, o := range ops {
			if o.Root != f.Root || o.Kind == KindCreate || o.Kind == KindForget ||
				(o.Kind == KindSet && o.Field == "deleted") || slices.Contains(f.Skipped, o.ID()) || ov[o.ID()] != nil {
				continue
			}
			if deleteOp.HLC().Compare(o.HLC()) < 0 {
				after = append(after, flagValue(o))
			}
		}
		if len(after) > 0 {
			f.Flags = append(f.Flags, Flag{Kind: FlagEditedAfterDelete, Values: after})
		}
	}

	// An overruled approve (9.3): the record it made is hidden like a deleted one until a person
	// restores it, and either way the record is flagged.
	createWin, createOver := ov[f.Root]
	if createOver {
		overruledBy = append(overruledBy, createWin)
	}
	if len(overruledBy) > 0 {
		f.Revision++
		fl := Flag{Kind: FlagApprovalOverruled}
		seen := map[OpID]bool{}
		for _, w := range overruledBy {
			if !seen[w.ID()] {
				seen[w.ID()] = true
				fl.Values = append(fl.Values, flagValue(w))
			}
		}
		f.Flags = append(f.Flags, fl)
	}
	if createOver {
		// Only a person's restore stamped after the approve shows the record again: the standing
		// deleted = false, written by a person. One made before the overrule arrived counts too,
		// since it is still stamped after the approve.
		restored := deleteOp != nil && deleteOp.ID() != f.Root && !f.boolean("deleted") && create.before(deleteOp) &&
			deleteOp.Actor != nil && deleteOp.Actor.Kind == "person"
		if !restored {
			fv := FieldValue{Value: json.RawMessage("true"), Op: createWin.ID()}
			if old, ok := f.Fields["deleted"]; ok {
				fv.Heads = old.Heads
			}
			f.Fields["deleted"] = fv
		}
	}
	slices.SortStableFunc(f.Flags, func(a, b Flag) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Field, b.Field)
	})
	return f
}

// decide folds a proposal's decision fields: the decision with the lowest stamp stands, and a
// second decision flags the record "decided twice" (5.6). decided_by follows the standing
// decision's batch. With no decision, the last write wins as for any scalar.
func (f *folded) decide(name string, cands, statusSets []*Op) *Op {
	var decided []*Op
	for _, o := range statusSets {
		var s string
		if json.Unmarshal(o.Value, &s) == nil && slices.Contains(decisions, s) {
			decided = append(decided, o)
		}
	}
	if len(decided) == 0 {
		return cands[len(cands)-1]
	}
	first := decided[0]
	if name == "status" {
		if len(decided) > 1 {
			fl := Flag{Kind: FlagDecidedTwice, Field: "status"}
			for _, d := range decided {
				fl.Values = append(fl.Values, flagValue(d))
			}
			f.Flags = append(f.Flags, fl)
		}
		return first
	}
	for _, o := range cands {
		if o.Origin == first.Origin && o.HLC() == first.HLC() {
			return o
		}
	}
	// No decided_by in the standing batch: the earliest one written after the decision, else the
	// last before it.
	for _, o := range cands {
		if !o.before(first) {
			return o
		}
	}
	return cands[len(cands)-1]
}

// heads returns the candidates no other candidate saw, following prev links among them.
func heads(cands []*Op) []*Op {
	byID := map[OpID]*Op{}
	for _, c := range cands {
		byID[c.ID()] = c
	}
	seen := map[OpID]bool{}
	var walk func(id OpID)
	walk = func(id OpID) {
		if seen[id] {
			return
		}
		seen[id] = true
		if o, ok := byID[id]; ok {
			for _, p := range o.Prev {
				walk(p)
			}
		}
	}
	for _, c := range cands {
		for _, p := range c.Prev {
			walk(p)
		}
	}
	var out []*Op
	for _, c := range cands {
		if !seen[c.ID()] {
			out = append(out, c)
		}
	}
	return out
}

func flagValue(o *Op) FlagValue {
	return FlagValue{Op: o.ID(), At: o.HLC(), Value: o.Value, Actor: o.Actor, Origin: o.Origin}
}

// retypeOK: only task and issue retype, into each other (2.2).
func retypeOK(v json.RawMessage) bool {
	var s string
	return json.Unmarshal(v, &s) == nil && (s == TypeTask || s == TypeIssue)
}

// memberKey is a set member's identity: a tag's text, a link's kind and target.
func memberKey(field string, v json.RawMessage) string {
	if field == "links" {
		var l Link
		if json.Unmarshal(v, &l) == nil && l.Kind != "" {
			return l.key()
		}
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
