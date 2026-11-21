package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// Capcom's pace rows (capcom-service.md 10.1, 19.1): their defaults and ranges; an app write
// takes; the [capcom] pace_* keys lock them as a [settings] entry does, in the forms 19.1 names,
// and a [settings] entry for the same id wins; pace_wait_at locks nothing.
func TestCapcomPaceRows(t *testing.T) {
	for id, want := range map[string]string{CapcomPaceDailyShare: "15", CapcomPaceSmallAt: "100", CapcomPaceEvents: "true", CapcomPaceEventStop: "80"} {
		if d, ok := Lookup(id); !ok || string(d.Default) != want {
			t.Errorf("%s default %s, want %s", id, d.Default, want)
		}
	}
	for _, c := range []struct {
		id, v string
		ok    bool
	}{
		{CapcomPaceDailyShare, `1`, true}, {CapcomPaceDailyShare, `100`, true}, {CapcomPaceDailyShare, `0`, false}, {CapcomPaceDailyShare, `101`, false},
		{CapcomPaceSmallAt, `10`, true}, {CapcomPaceSmallAt, `500`, true}, {CapcomPaceSmallAt, `9`, false}, {CapcomPaceSmallAt, `501`, false},
		{CapcomPaceEventStop, `10`, true}, {CapcomPaceEventStop, `500`, true}, {CapcomPaceEventStop, `0`, false},
		{CapcomPaceEvents, `false`, true}, {CapcomPaceEvents, `0`, false}, {CapcomPaceDailyShare, `15.5`, false},
	} {
		d, _ := Lookup(c.id)
		if _, err := d.Check(json.RawMessage(c.v)); (err == nil) != c.ok {
			t.Errorf("%s %s: err %v", c.id, c.v, err)
		}
	}

	// No config: the app writes, and the write is applied.
	s, r := open(t, t.TempDir(), "", nil)
	if _, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: CapcomPaceEvents, Value: json.RawMessage(`false`), Revision: 1},
		{ID: CapcomPaceDailyShare, Value: json.RawMessage(`25`), Revision: 1}}}); err != nil {
		t.Fatal(err)
	}
	if r.applied[CapcomPaceEvents] != "false" || r.applied[CapcomPaceDailyShare] != "25" {
		t.Fatalf("applied %v", r.applied)
	}

	// The [capcom] keys lock: a number, a fraction rounded, a string with %; a bad value is left
	// out; [settings] wins over [capcom] for the same id; pace_wait_at locks nothing.
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	body := "[capcom]\npace_daily_share = 12.6\npace_small_at = \"150%\"\npace_event_stop = 9\npace_wait_at = 120\n\n" +
		"[settings]\n\"capcom.pace.small_at\" = 200\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ = open(t, t.TempDir(), cfg, nil)
	for id, want := range map[string]struct {
		v   string
		src api.SettingSource
	}{
		CapcomPaceDailyShare: {"13", api.SettingSourceConfig},
		CapcomPaceSmallAt:    {"200", api.SettingSourceConfig},
		CapcomPaceEventStop:  {"80", api.SettingSourceDefault},
		CapcomPaceEvents:     {"true", api.SettingSourceDefault},
	} {
		if st := find(t, s.List(), id); string(st.Value) != want.v || st.Source != want.src {
			t.Errorf("%s: %+v, want %s from %s", id, st, want.v, want.src)
		}
	}
	_, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: CapcomPaceDailyShare, Value: json.RawMessage(`20`), Revision: 2}}})
	if k, id := refusal(err); k != api.ErrorKindLocked || id != CapcomPaceDailyShare {
		t.Fatalf("write to a row [capcom] locks: %v", err)
	}
	// Read, for a caller while gff-hab is down, sees the same lock.
	if v := Read(t.TempDir(), cfg)(CapcomPaceDailyShare); string(v) != "13" {
		t.Fatalf("Read: %s", v)
	}
}

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
