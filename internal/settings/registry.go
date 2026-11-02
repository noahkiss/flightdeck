// Package settings keeps the hab-side settings (app.md 20.5): each setting's value, default,
// revision and source; client writes in <state>/settings.json; locks from the [settings] table
// of the hab's config file and from flags or the environment. It serves settings.get,
// settings.set and the settings.updated event. Pure Go.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Capability is the capability string of the hab-side settings (hab-logic-api.md 3.4).
const Capability = "settings"

// The hab-side setting ids (app.md 20.3, 20.4).
const (
	StartElsewhere  = "confirm.start_elsewhere"
	ApprovalMaxAge  = "approval.max_age"
	LogbookApproval = "logbook.approval"
	PermissionWait  = "hab.permission_wait"
)

// Def is one hab-side setting: its id, its built-in value (the Balanced column, app.md 20.1)
// and the check of its type.
type Def struct {
	ID      string
	Default json.RawMessage
	// check returns the canonical JSON of v, or an error saying why v is outside the type.
	check func(v any) (json.RawMessage, error)
}

// Defs is every hab-side setting, in the order settings.get lists them.
var Defs = []Def{
	{ID: StartElsewhere, Default: json.RawMessage(`true`), check: checkBool},
	{ID: ApprovalMaxAge, Default: json.RawMessage(`60`), check: checkInt(0, 1440)},
	{ID: LogbookApproval, Default: json.RawMessage(`"off"`), check: checkWord("off", "agents")},
	{ID: PermissionWait, Default: json.RawMessage(`0`), check: checkInt(0, 120)},
}

// Lookup returns the setting with the id.
func Lookup(id string) (Def, bool) {
	for _, d := range Defs {
		if d.ID == id {
			return d, true
		}
	}
	return Def{}, false
}

// Check returns the canonical JSON of raw when it is a value of the setting's type.
func (d Def) Check(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: not a JSON value", d.ID)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", d.ID)
	}
	out, err := d.check(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", d.ID, err)
	}
	return out, nil
}

// CheckGo checks a value decoded from TOML (bool, int64, float64, string).
func (d Def) CheckGo(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", d.ID, err)
	}
	return d.Check(b)
}

func checkBool(v any) (json.RawMessage, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("want true or false")
	}
	return json.Marshal(b)
}

func checkInt(lo, hi int64) func(any) (json.RawMessage, error) {
	return func(v any) (json.RawMessage, error) {
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("want a whole number from %d to %d", lo, hi)
		}
		i, err := n.Int64()
		if err != nil || i < lo || i > hi {
			return nil, fmt.Errorf("want a whole number from %d to %d", lo, hi)
		}
		return json.Marshal(i)
	}
}

func checkWord(words ...string) func(any) (json.RawMessage, error) {
	return func(v any) (json.RawMessage, error) {
		s, ok := v.(string)
		if !ok || !slices.Contains(words, s) {
			return nil, fmt.Errorf("want one of %q", words)
		}
		return json.Marshal(s)
	}
}
