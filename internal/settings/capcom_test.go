package settings

import (
	"encoding/json"
	"testing"
)

// The Capcom writes-first rows (capcom-service.md 19.1): each range, and quiet hours' form.
func TestCapcomEventRows(t *testing.T) {
	cases := []struct {
		id, v string
		ok    bool
	}{
		{CapcomQuietHours, `"22:00-07:00"`, true}, {CapcomQuietHours, `"00:00-00:00"`, true},
		{CapcomQuietHours, `"24:00-07:00"`, false}, {CapcomQuietHours, `"22:00"`, false}, {CapcomQuietHours, `" 2:00-07:00"`, false},
		{CapcomQuietHours, `"22:60-07:00"`, false}, {CapcomQuietHours, `2200`, false},
		{CapcomQuietHours, `"off"`, true}, {CapcomQuietHours, `"Off"`, false},
		{CapcomDailyTurns, `1`, true}, {CapcomDailyTurns, `1000`, true}, {CapcomDailyTurns, `0`, true}, {CapcomDailyTurns, `-1`, false}, {CapcomDailyTurns, `1001`, false},
		{CapcomEventActions, `"all"`, true}, {CapcomEventActions, `"bypass"`, false},
		{CapcomEventActionsPerTurn, `0`, true}, {CapcomEventActionsPerTurn, `50`, true}, {CapcomEventActionsPerTurn, `51`, false},
		{CapcomBatchWindow, `10`, true}, {CapcomBatchWindow, `3600`, true}, {CapcomBatchWindow, `9`, false}, {CapcomBatchWindow, `3601`, false},
		{CapcomBatchMax, `1`, true}, {CapcomBatchMax, `200`, true}, {CapcomBatchMax, `0`, false}, {CapcomBatchMax, `201`, false},
		{CapcomEventsNotices, `false`, true}, {CapcomEventsRounds, `"no"`, false}, {CapcomEventsRoundMessages, `true`, true},
	}
	for _, c := range cases {
		d, ok := Lookup(c.id)
		if !ok {
			t.Fatalf("no %s", c.id)
		}
		if _, err := d.Check(json.RawMessage(c.v)); (err == nil) != c.ok {
			t.Errorf("%s %s: err %v", c.id, c.v, err)
		}
	}
	if s, e, err := ParseQuietHours("22:00-07:30"); err != nil || s != 22*60 || e != 7*60+30 {
		t.Fatalf("parse %d %d %v", s, e, err)
	}
	if s, e, err := ParseQuietHours("off"); err != nil || s != e {
		t.Fatalf("parse off %d %d %v", s, e, err)
	}
	for id, want := range map[string]string{CapcomDailyTurns: "200", CapcomEventActionsPerTurn: "10"} {
		if d, _ := Lookup(id); string(d.Default) != want {
			t.Errorf("%s default %s, want %s (A10)", id, d.Default, want)
		}
	}
}
