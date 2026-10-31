package api

import (
	"encoding/json"
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
	e := Event{Seq: 1, Type: EventPaneOwner, ID: "p1", Revision: 2,
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
