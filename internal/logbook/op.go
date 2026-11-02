package logbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// OpVersion is the op format version this build writes and folds. An op with a higher version is
// stored and forwarded as it came, and folded only by a build that knows it (4.4).
const OpVersion = 1

// Kind is an op kind (4.2).
type Kind string

const (
	KindCreate Kind = "create"
	KindSet    Kind = "set"
	KindAdd    Kind = "add"
	KindRemove Kind = "remove"
	KindAppend Kind = "append"
	KindForget Kind = "forget"
)

var kinds = []Kind{KindCreate, KindSet, KindAdd, KindRemove, KindAppend, KindForget}

// Op is one change to one record, made on one hab. The JSON names are the wire and file form;
// the envelope (v, o, s, w, c, bl, k, r, root) never changes between versions, so an older build
// can order, apply and forward an op whose body it does not understand.
type Op struct {
	V       int    `json:"v"`
	Origin  string `json:"o"`
	Seq     uint64 `json:"s"`
	Wall    int64  `json:"w"`
	Counter uint32 `json:"c"`
	// BatchLast is the seq of the last op of this op's batch. A replica folds a batch only when
	// it holds all of it (4.2).
	BatchLast uint64 `json:"bl"`
	Kind      Kind   `json:"k"`
	// Record is the record id the writer used: for a create, the id it claims.
	Record string `json:"r"`
	// Root is the record's create op. Ops name it so that two creates of one id stay apart (2.3).
	Root OpID `json:"root"`
	// Field names the field of a set, add, remove or append.
	Field string `json:"f,omitempty"`
	// Value: a create's initial fields (an object), a set's value, an add's or remove's member,
	// an append's note text.
	Value json.RawMessage `json:"val,omitempty"`
	// Prev: for a set, the ops whose value the writer saw for the field (every value it showed,
	// so two when it saw a conflict); for a remove, the adds of the member it saw.
	Prev []OpID `json:"prev,omitempty"`
	// Target is the op a forget redacts.
	Target *OpID  `json:"t,omitempty"`
	Actor  *Actor `json:"a,omitempty"`

	raw []byte
}

// ID is the op's id.
func (o *Op) ID() OpID { return OpID{Origin: o.Origin, Seq: o.Seq} }

// HLC is the op's stamp. Every op of a batch shares it.
func (o *Op) HLC() HLC { return HLC{Wall: o.Wall, Counter: o.Counter, Origin: o.Origin} }

// Raw is the op's stored bytes: what sync forwards.
func (o *Op) Raw() []byte { return o.raw }

// before orders ops by stamp, then by seq within one batch.
func (o *Op) before(p *Op) bool {
	if c := o.HLC().Compare(p.HLC()); c != 0 {
		return c < 0
	}
	return o.Seq < p.Seq
}

func opCmp(a, b *Op) int {
	if c := a.HLC().Compare(b.HLC()); c != 0 {
		return c
	}
	return cmpInt(int64(a.Seq), int64(b.Seq))
}

// errBadEnvelope means an op cannot be ordered or applied at all; it is refused, not stored.
var errBadEnvelope = errors.New("logbook: op envelope incomplete")

// decodeOp reads stored or received bytes. It fails only when the envelope is unusable; an op of
// a later version or an unknown kind decodes with known false.
func decodeOp(raw []byte) (*Op, error) {
	var o Op
	if err := json.Unmarshal(raw, &o); err != nil {
		// A later version may change a body field's shape. Read the envelope alone.
		var env struct {
			V         int    `json:"v"`
			Origin    string `json:"o"`
			Seq       uint64 `json:"s"`
			Wall      int64  `json:"w"`
			Counter   uint32 `json:"c"`
			BatchLast uint64 `json:"bl"`
			Kind      Kind   `json:"k"`
			Record    string `json:"r"`
			Root      string `json:"root"`
		}
		if err2 := json.Unmarshal(raw, &env); err2 != nil {
			return nil, fmt.Errorf("%w: %v", errBadEnvelope, err)
		}
		o = Op{V: env.V, Origin: env.Origin, Seq: env.Seq, Wall: env.Wall, Counter: env.Counter,
			BatchLast: env.BatchLast, Kind: env.Kind, Record: env.Record}
		if r, err := ParseOpID(env.Root); err == nil {
			o.Root = r
		}
		if o.V <= OpVersion {
			return nil, fmt.Errorf("%w: %v", errBadEnvelope, err)
		}
	}
	if o.Origin == "" || o.Seq == 0 || o.V < 1 || o.BatchLast < o.Seq || o.Kind == "" ||
		o.Record == "" || o.Root.IsZero() {
		return nil, errBadEnvelope
	}
	// SQLite stores integers as int64: a seq above that cannot be stored or looked up. Op ids in
	// the root, the target and prev are held to the same bound by ParseOpID.
	if o.Seq > math.MaxInt64 || o.BatchLast > math.MaxInt64 {
		return nil, fmt.Errorf("%w: seq above 2^63-1", errBadEnvelope)
	}
	// A create claims an id of the shape 2.3 allows. That shape has one dash; a collision id
	// (base-2, base-3) has two, so a peer can never claim an id a fold gives another record.
	if o.Kind == KindCreate && !ValidID(o.Record) {
		return nil, fmt.Errorf("%w: create of id %q", errBadEnvelope, o.Record)
	}
	o.raw = slices.Clone(raw)
	return &o, nil
}

// known reports whether this build can fold the op: its version, kind and field are known.
func (o *Op) known() bool {
	if o.V > OpVersion || !slices.Contains(kinds, o.Kind) {
		return false
	}
	switch o.Kind {
	case KindCreate:
		if o.Root != o.ID() {
			return false
		}
		var c createValue
		if json.Unmarshal(o.Value, &c) != nil || !slices.Contains(Types, c.Type) {
			return false
		}
		for name := range c.Fields {
			f, ok := fieldFor(c.Type, name)
			if !ok || f.kind == setField || f.kind == logField {
				return false
			}
		}
		return true
	case KindForget:
		return o.Target != nil
	case KindSet:
		f, ok := fields[o.Field]
		return ok && f.kind != setField && f.kind != logField
	case KindAdd, KindRemove:
		f, ok := fields[o.Field]
		return ok && f.kind == setField
	case KindAppend:
		f, ok := fields[o.Field]
		return ok && f.kind == logField
	}
	return false
}

// createValue is a create op's value: the record's type, its initial scalar fields, and the
// optional source.
type createValue struct {
	Type   string                     `json:"type"`
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
	Source json.RawMessage            `json:"source,omitempty"` // a Source, or a forgotten marker
}

// encode sets the op's stored bytes from its fields.
func (o *Op) encode() error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	o.raw = b
	return nil
}

// forgottenPrefix starts a redacted value (5.7).
const forgottenPrefix = "forgotten sha256:"

func forgottenMarker(v []byte) json.RawMessage {
	sum := sha256.Sum256(v)
	return jsonString(forgottenPrefix + hex.EncodeToString(sum[:]))
}

func isForgotten(v json.RawMessage) bool {
	var s string
	return json.Unmarshal(v, &s) == nil && strings.HasPrefix(s, forgottenPrefix)
}

// forgettable reports whether a forget redacts an op of this kind and field: a create's text, a
// set of a text field, an add, an append (5.7). A set of a field with no text, a remove and a
// forget hold nothing to forget, so a forget of one, which a local write refuses, changes nothing
// when a peer sends it. An op of a kind this build does not know is redacted whole: the newer
// build that wrote the forget knows what it holds.
func forgettable(kind Kind, field string) bool {
	switch kind {
	case KindRemove, KindForget:
		return false
	case KindSet:
		spec, ok := fields[field]
		return !ok || spec.text
	}
	return true
}

// redact returns raw with its value replaced by a hash and the word "forgotten". For a create,
// each text field and the source are replaced and the rest kept, so the record still folds. It
// is idempotent, so replicas that redact the same op in any order hold the same bytes.
func redact(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	val, ok := m["val"]
	if !ok {
		return raw, nil
	}
	var kind Kind
	_ = json.Unmarshal(m["k"], &kind)
	var field string
	_ = json.Unmarshal(m["f"], &field)
	if !forgettable(kind, field) {
		return raw, nil
	}
	if kind == KindCreate {
		var c map[string]json.RawMessage
		if err := json.Unmarshal(val, &c); err != nil {
			return nil, err
		}
		if src, ok := c["source"]; ok && !isForgotten(src) {
			c["source"] = forgottenMarker(src)
		}
		var fs map[string]json.RawMessage
		if f, ok := c["fields"]; ok && json.Unmarshal(f, &fs) == nil {
			for name, v := range fs {
				if spec, ok := fields[name]; ok && spec.text && !isForgotten(v) {
					fs[name] = forgottenMarker(v)
				}
			}
			c["fields"], _ = json.Marshal(fs)
		}
		m["val"], _ = json.Marshal(c)
	} else if !isForgotten(val) {
		m["val"] = forgottenMarker(val)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(out, raw) {
		return raw, nil
	}
	return out, nil
}
