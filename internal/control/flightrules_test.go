package control

import (
	"context"
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

type fakeFlightRules struct{}

func (fakeFlightRules) List(_ context.Context, p api.FlightRulesParams) (api.FlightRules, error) {
	if p.Session == "gone" {
		return api.FlightRules{}, Errorf(api.ErrorKindNotFound, "no session gone")
	}
	return api.FlightRules{Session: p.Session, Harness: "claude"}, nil
}

func (fakeFlightRules) Text(_ context.Context, p api.FlightRuleTextParams) (api.FlightRuleText, error) {
	return api.FlightRuleText{ID: p.ID, Text: "hello", Bytes: 5}, nil
}

// TestFlightRulesMethods: both methods reach FlightRules; an empty ledger answers an empty
// list, not null; a missing session or id is invalid; an *Error keeps its kind; with no
// FlightRules both are unavailable.
func TestFlightRulesMethods(t *testing.T) {
	h := startHab(t, "", socketPath(t), func(c *Config) { c.FlightRules = fakeFlightRules{} })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	var raw struct {
		Entries []api.FlightRule `json:"entries"`
		Harness string           `json:"harness"`
	}
	if err := c.Call(ctx(t), "flightrules.list", api.FlightRulesParams{Session: "s1"}, &raw); err != nil || raw.Entries == nil || raw.Harness != "claude" {
		t.Fatalf("list: %+v %v", raw, err)
	}
	if r, err := cl.FlightrulesText(ctx(t), api.FlightRuleTextParams{Session: "s1", ID: "e1"}); err != nil || r.Text != "hello" || r.ID != "e1" {
		t.Fatalf("text: %+v %v", r, err)
	}
	var ce *Error
	if _, err := cl.FlightrulesList(ctx(t), api.FlightRulesParams{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Errorf("no session: %v", err)
	}
	if _, err := cl.FlightrulesText(ctx(t), api.FlightRuleTextParams{Session: "s1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Errorf("no id: %v", err)
	}
	if _, err := cl.FlightrulesList(ctx(t), api.FlightRulesParams{Session: "gone"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("gone session: %v", err)
	}

	bare := startHab(t, "", socketPath(t), nil)
	bc := dial(t, bare.path)
	hello(t, bc, "")
	if _, err := (api.Client{Caller: bc}).FlightrulesList(ctx(t), api.FlightRulesParams{Session: "s1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no flight rules: %v", err)
	}
}
