package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaEmbedded(t *testing.T) {
	var doc struct {
		Revision int `json:"x-revision"`
	}
	if err := json.Unmarshal(SchemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Revision != Revision {
		t.Fatalf("embedded schema revision %d, want %d", doc.Revision, Revision)
	}
}

func TestEventDecodeData(t *testing.T) {
	e := Event{Seq: 1, Type: EventPaneOwner, ID: "p1",
		Data: RawJSON(`{"reason":"typed","cols":80,"rows":24}`)}
	v, err := e.DecodeData()
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.(PaneOwner)
	if !ok || o.Reason != OwnerReasonTyped || o.Cols != 80 {
		t.Fatalf("got %#v", v)
	}
	e.Type = "later.thing"
	if v, _ := e.DecodeData(); string(v.(RawJSON)) != string(e.Data) {
		t.Fatalf("unknown type should return raw data")
	}
}

func TestResultsCarrySeq(t *testing.T) {
	for _, m := range Methods {
		if m.Name == "rpc.discover" {
			continue
		}
		b, _ := json.Marshal(m.Result)
		var fields map[string]any
		_ = json.Unmarshal(b, &fields)
		if _, ok := fields["seq"]; !ok {
			t.Errorf("%s: result has no seq (spec 3.5)", m.Name)
		}
	}
}

// TestNoNullArrays marshals every zero-value message through NonNil and fails on a null, which a
// Swift client would refuse for a required array.
func TestNoNullArrays(t *testing.T) {
	var values []any
	for _, m := range Methods {
		values = append(values, m.Result)
	}
	for _, n := range Notifications {
		values = append(values, n.Params)
	}
	for _, e := range Events {
		values = append(values, e.Data)
	}
	for _, f := range Frames {
		if f.Payload != nil {
			values = append(values, f.Payload)
		}
	}
	// A nested list: a structure with one project whose session list is nil.
	values = append(values, StructureResult{Structure: Structure{Projects: []Project{{ID: "p"}}}})
	for _, v := range values {
		if _, raw := v.(RawJSON); raw {
			continue
		}
		b, err := json.Marshal(NonNil(v))
		if err != nil {
			t.Fatal(err)
		}
		// Event.data is any JSON value, so only its own null is allowed.
		s := strings.ReplaceAll(string(b), `"data":null`, "")
		if strings.Contains(s, "null") {
			t.Errorf("%T marshals a null: %s", v, b)
		}
	}
}
