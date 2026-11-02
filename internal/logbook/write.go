package logbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Step is one change in a batch.
type Step struct {
	Kind Kind
	// Record is the record the step changes. For a create it is the new id; empty mints one.
	Record string
	// Type, Fields and Source make a create: the record type, its initial scalar fields, and
	// where it came from.
	Type   string
	Fields map[string]any
	Source *Source
	// Field and Value: a set's field and value; an add's or remove's set field and member (a tag
	// string or a Link); an append's field ("notes") and text.
	Field string
	Value any
	// Target is the op a forget redacts.
	Target OpID
}

// Batch is one change the user or an agent makes in one step: its ops share one stamp and land
// all or nothing on every replica (4.2).
type Batch struct {
	Actor *Actor
	Steps []Step
	// Key, when set, is the write's idempotency key (keys.go), and KeyHash a hash of the request
	// it came with. A key the file holds applies nothing and returns the first result.
	Key, KeyHash string
}

// WriteResult names what a write made.
type WriteResult struct {
	Ops []OpID
	// Records lists the ids the batch touched, in step order, each once.
	Records []string
	// Repeated is true when the batch's key was used before: nothing was applied, and Ops and
	// Records are the first write's.
	Repeated bool `json:"-"`
}

// Create returns a create step for a new record with a minted id.
func Create(typ string, fields map[string]any) Step {
	return Step{Kind: KindCreate, Record: NewID(typ), Type: typ, Fields: fields}
}

// Write makes a batch into ops on this replica and folds them, in one transaction under SQLite's
// write lock (4.3): the next seq and the last stamp come from the file, so two writers on one
// file never share a seq. A step that breaks a rule refuses the whole batch with an
// *InvalidError.
func (s *Store) Write(ctx context.Context, b Batch) (WriteResult, error) {
	var res WriteResult
	if len(b.Steps) == 0 {
		return res, invalid("", "an empty batch")
	}
	if len(b.Key) > MaxKey {
		return res, invalid("", "an idempotency key is at most %d bytes", MaxKey)
	}
	if err := s.waitHold(ctx); err != nil {
		return res, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	m, err := readMeta(ctx, tx)
	if err != nil {
		return res, err
	}
	if b.Key != "" {
		if r, found, err := keyed(ctx, tx, b.Key, b.KeyHash); found || err != nil {
			return r, err
		}
	}
	if g, err := originRow(ctx, tx, m.origin); err != nil {
		return res, err
	} else if g.shared {
		return res, ErrSharedOrigin
	} else if m.ownSeen > g.held {
		return res, fmt.Errorf("%w (a peer holds up to seq %d, this file up to %d)", ErrRestoring, m.ownSeen, g.held)
	}
	stamp := tick(m.last, s.now().UnixMilli(), m.origin)
	w := &writer{ctx: ctx, tx: tx, state: map[string]*folded{}, setInBatch: map[string]bool{}}
	n := uint64(len(b.Steps))
	var ops []*Op
	for i, st := range b.Steps {
		o := &Op{V: OpVersion, Origin: m.origin, Seq: m.nextSeq + uint64(i), Wall: stamp.Wall,
			Counter: stamp.Counter, BatchLast: m.nextSeq + n - 1, Kind: st.Kind, Actor: b.Actor}
		if err := w.step(o, st); err != nil {
			return res, err
		}
		if err := o.encode(); err != nil {
			return res, err
		}
		ops = append(ops, o)
		res.Ops = append(res.Ops, o.ID())
		if !slices.Contains(res.Records, o.Record) {
			res.Records = append(res.Records, o.Record)
		}
	}
	affected := map[string]bool{}
	forgot := false
	for _, o := range ops {
		red, err := s.insertOp(ctx, tx, o)
		if err != nil {
			return res, err
		}
		for r := range red {
			affected[r] = true
			forgot = true
		}
	}
	m.nextSeq += n
	m.last = stamp
	if err := m.write(ctx, tx); err != nil {
		return res, err
	}
	if _, _, err := advance(ctx, tx, m.origin); err != nil {
		return res, err
	}
	if err := s.affectedBy(ctx, tx, ops, affected); err != nil {
		return res, err
	}
	if _, err := s.refold(ctx, tx, affected); err != nil {
		return res, err
	}
	if forgot {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fts(fts) VALUES('optimize')`); err != nil {
			return res, err
		}
	}
	if b.Key != "" {
		if err := keep(ctx, tx, b.Key, b.KeyHash, res, s.now().UnixMilli()); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if forgot {
		s.scrub(ctx)
	}
	return res, nil
}

// writer checks a batch's steps against the folded state, with the batch's own earlier steps
// applied.
type writer struct {
	ctx        context.Context
	tx         txn
	state      map[string]*folded
	setInBatch map[string]bool
}

func (w *writer) record(id string) (*folded, error) {
	if f, ok := w.state[id]; ok {
		return f, nil
	}
	var data string
	err := w.tx.QueryRowContext(w.ctx, `SELECT data FROM records WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var f folded
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil, err
	}
	w.state[id] = &f
	return &f, nil
}

func (w *writer) step(o *Op, st Step) error {
	switch st.Kind {
	case KindCreate:
		return w.create(o, st)
	case KindForget:
		return w.forget(o, st)
	case KindSet, KindAdd, KindRemove, KindAppend:
	default:
		return invalid("", "unknown step kind %q", st.Kind)
	}
	f, err := w.record(st.Record)
	if err != nil {
		return err
	}
	o.Record, o.Root, o.Field = f.ID, f.Root, st.Field
	// Findings and proposals never change type, and a task and an issue share their rules, so
	// the current type sets the rules.
	createType := f.Type
	if f.boolean("deleted") && !(st.Kind == KindSet && st.Field == "deleted") {
		return invalid("restore it first", "%s is deleted", f.ID)
	}
	spec, ok := fieldFor(f.Type, st.Field)
	if !ok {
		return invalid("", "a %s has no field %q", f.Type, st.Field)
	}
	val, err := json.Marshal(st.Value)
	if err != nil {
		return invalid("", "value of %s: %v", st.Field, err)
	}
	switch st.Kind {
	case KindSet:
		if spec.kind == setField || spec.kind == logField {
			return invalid("use add, remove or append", "%s is not a scalar field", st.Field)
		}
		if !mutable(createType, st.Field) {
			hint := ""
			if createType == TypeFinding && (st.Field == "title" || st.Field == "body") {
				hint = "a finding is fixed at create: supersede it with a new finding"
			}
			return invalid(hint, "a %s's %s cannot change", createType, st.Field)
		}
		if err := checkScalar(f.Type, st.Field, spec, val); err != nil {
			return err
		}
		if f.Type == TypeProposal && (st.Field == "status" || st.Field == "decided_by") &&
			slices.Contains(decisions, f.str("status")) && !w.setInBatch[f.ID+" status"] {
			// The first decision stands (5.6); a writer that sees it cannot add a second. Only
			// decisions made apart raise "decided twice".
			return invalid("a decided proposal does not change", "%s is %s", f.ID, f.str("status"))
		}
		key := f.ID + " " + st.Field
		if w.setInBatch[key] {
			return invalid("", "%s of %s set twice in one batch", st.Field, f.ID)
		}
		w.setInBatch[key] = true
		o.Value = val
		o.Prev = slices.Clone(f.Fields[st.Field].Heads)
		f.Fields[st.Field] = FieldValue{Value: val, Op: o.ID(), Heads: []OpID{o.ID()}}
		if st.Field == "type" {
			var t string
			_ = json.Unmarshal(val, &t)
			f.Type = t
		}
	case KindAdd, KindRemove:
		if spec.kind != setField {
			return invalid("", "%s is not a set field", st.Field)
		}
		if err := w.checkMember(f, st.Field, val); err != nil {
			return err
		}
		o.Value = val
		key := memberKey(st.Field, val)
		if st.Kind == KindAdd {
			f.Sets[st.Field] = append(f.Sets[st.Field], Member{Key: key, Value: val, Op: o.ID(), Adds: []OpID{o.ID()}})
			break
		}
		var seen []OpID
		kept := f.Sets[st.Field][:0:0]
		for _, mem := range f.Sets[st.Field] {
			if mem.Key == key {
				seen = append(seen, mem.Adds...)
			} else {
				kept = append(kept, mem)
			}
		}
		if len(seen) == 0 {
			return invalid("", "%s has no %s member %s", f.ID, st.Field, key)
		}
		f.Sets[st.Field] = kept
		o.Prev = seen
	case KindAppend:
		if spec.kind != logField {
			return invalid("", "%s is not a note log", st.Field)
		}
		text, ok := st.Value.(string)
		if !ok || strings.TrimSpace(text) == "" || len(text) > MaxBody || !utf8.ValidString(text) {
			return invalid("", "a note is non-empty text of at most 256 KiB")
		}
		o.Value = val
	}
	return nil
}

var tagRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._:/+-]*$`)
var linkTargetRe = regexp.MustCompile(`^[a-z]+-[a-z0-9]{3,16}(-[0-9]+)?$`)

func (w *writer) create(o *Op, st Step) error {
	if !slices.Contains(Types, st.Type) {
		return invalid("", "unknown record type %q", st.Type)
	}
	id := st.Record
	if id == "" {
		id = NewID(st.Type)
	}
	if !ValidID(id) {
		return invalid("", "record id %q: want <letters>-<3 to 16 letters or digits>", id)
	}
	if _, err := w.record(id); err == nil {
		return invalid("", "record %s exists", id)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	cv := createValue{Type: st.Type, Fields: map[string]json.RawMessage{}}
	for name, v := range st.Fields {
		spec, ok := fieldFor(st.Type, name)
		if !ok || name == "type" || spec.kind == setField || spec.kind == logField {
			return invalid("add tags and links, and append notes, as their own steps", "a %s has no scalar field %q", st.Type, name)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return invalid("", "value of %s: %v", name, err)
		}
		if err := checkScalar(st.Type, name, spec, b); err != nil {
			return err
		}
		cv.Fields[name] = b
	}
	if _, ok := cv.Fields["title"]; !ok {
		return invalid("", "a new record needs a title")
	}
	if list := statuses[st.Type]; len(list) > 0 {
		if _, ok := cv.Fields["status"]; !ok {
			cv.Fields["status"] = jsonString(list[0])
		}
	}
	if st.Type == TypeProposal {
		var s string
		_ = json.Unmarshal(cv.Fields["status"], &s)
		if s != "pending" {
			return invalid("decide it after", "a new proposal is pending")
		}
	}
	if st.Source != nil {
		cv.Source, _ = json.Marshal(st.Source)
	}
	val, err := json.Marshal(cv)
	if err != nil {
		return err
	}
	o.Record, o.Root, o.Value = id, o.ID(), val
	f := &folded{Root: o.ID(), ID: id, Type: st.Type, Fields: map[string]FieldValue{}, Sets: map[string][]Member{}}
	f.Fields["type"] = FieldValue{Value: jsonString(st.Type), Op: o.ID(), Heads: []OpID{o.ID()}}
	for name, v := range cv.Fields {
		f.Fields[name] = FieldValue{Value: v, Op: o.ID(), Heads: []OpID{o.ID()}}
		w.setInBatch[id+" "+name] = true
	}
	w.state[id] = f
	return nil
}

func (w *writer) forget(o *Op, st Step) error {
	if st.Target.Seq > math.MaxInt64 {
		return ErrNotFound
	}
	var raw []byte
	var root string
	err := w.tx.QueryRowContext(w.ctx, `SELECT raw, root FROM ops WHERE origin = ? AND seq = ?`, st.Target.Origin, st.Target.Seq).
		Scan(&raw, &root)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	t, err := decodeOp(raw)
	if err != nil {
		return err
	}
	switch t.Kind {
	case KindCreate, KindAppend, KindAdd:
	case KindSet:
		if spec, ok := fields[t.Field]; !ok || !spec.text {
			return invalid("forget a text value: a title, body, note, tag or the like", "%s holds no text", t.Field)
		}
	default:
		return invalid("", "a %s op holds nothing to forget", t.Kind)
	}
	target := st.Target
	o.Target = &target
	o.Root, _ = ParseOpID(root)
	// Name the record by the id it folds to, which differs from the create's claim after an id
	// collision (2.3), as every other step does.
	o.Record = t.Record
	var id string
	err = w.tx.QueryRowContext(w.ctx, `SELECT id FROM records WHERE root = ?`, root).Scan(&id)
	switch {
	case err == nil:
		o.Record = id
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return nil
}

func checkScalar(typ, name string, spec fieldSpec, v json.RawMessage) error {
	switch spec.kind {
	case scalarBool:
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return invalid("", "%s is true or false", name)
		}
		return nil
	case scalarInt:
		var n int
		if json.Unmarshal(v, &n) != nil || n < 0 || n > 3 {
			return invalid("", "%s is 1 to 3, or 0 for none", name)
		}
		return nil
	case scalarJSON:
		if len(v) > MaxBody {
			return invalid("", "%s is over 256 KiB", name)
		}
		return nil
	}
	var s string
	if json.Unmarshal(v, &s) != nil || !utf8.ValidString(s) {
		return invalid("", "%s is text", name)
	}
	switch name {
	case "title":
		if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > MaxTitle || strings.ContainsAny(s, "\r\n") {
			return invalid("", "a title is one line of 1 to 200 characters")
		}
	case "body":
		if len(s) > MaxBody {
			return invalid("put the text in a file in the repository and link it", "the body is over 256 KiB")
		}
	case "status":
		if !ValidStatus(typ, s) {
			return invalid(strings.Join(statuses[typ], ", "), "%q is not a %s status", s, typ)
		}
	case "type":
		if !retypeOK(v) {
			return invalid("", "only a task and an issue change type, into each other")
		}
	case "triage":
		if s != "" && s != "new" {
			return invalid("", "triage is new or empty")
		}
	default:
		if len(s) > 4096 {
			return invalid("", "%s is over 4 KiB", name)
		}
	}
	return nil
}

func (w *writer) checkMember(f *folded, field string, v json.RawMessage) error {
	if field == "tags" {
		var s string
		if json.Unmarshal(v, &s) != nil || utf8.RuneCountInString(s) > MaxTag || !tagRe.MatchString(s) ||
			strings.IndexFunc(s, unicode.IsSpace) >= 0 {
			return invalid("", "a tag is one short word")
		}
		return nil
	}
	if field == "identities" {
		var s string
		if json.Unmarshal(v, &s) != nil || strings.TrimSpace(s) == "" || len(s) > 1024 || strings.ContainsAny(s, "\r\n") {
			return invalid("", "an identity is one line of text")
		}
		return nil
	}
	var l Link
	if json.Unmarshal(v, &l) != nil || !slices.Contains(linkKinds, l.Kind) || !linkTargetRe.MatchString(l.To) {
		return invalid(strings.Join(linkKinds, ", "), "a link is a kind and a record id")
	}
	if l.To == f.ID {
		return invalid("", "a record cannot link to itself")
	}
	if l.Pos != 0 && l.Kind != LinkStage {
		return invalid("", "only a stage link has a position")
	}
	from := []string{}
	to := []string{}
	switch l.Kind {
	case LinkSupersedes:
		from, to = []string{TypeFinding}, []string{TypeFinding}
	case LinkStage:
		from, to = []string{TypePlan}, []string{TypeTask, TypeIssue}
	case LinkPartOf:
		to = []string{TypePlan}
	case LinkBlocks:
		from, to = []string{TypeTask, TypeIssue}, []string{TypeTask, TypeIssue}
	}
	if len(from) > 0 && !slices.Contains(from, f.Type) {
		return invalid("", "a %s link starts at a %s", l.Kind, strings.Join(from, " or "))
	}
	if len(to) > 0 {
		t, err := w.record(l.To)
		switch {
		case errors.Is(err, ErrNotFound):
			// Not here yet: kept, and resolved when its ops arrive (2.4).
		case err != nil:
			return err
		case !slices.Contains(to, t.Type):
			return invalid("", "a %s link points at a %s", l.Kind, strings.Join(to, " or "))
		}
	}
	return nil
}
