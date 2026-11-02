package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type recorder struct {
	mu      sync.Mutex
	events  []structure.Event
	applied map[string]string
}

func (r *recorder) publish(e structure.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) apply(id string, v json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applied == nil {
		r.applied = map[string]string{}
	}
	r.applied[id] = string(v)
}

func (r *recorder) take() []structure.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func open(t *testing.T, state, config string, overrides map[string]json.RawMessage) (*Service, *recorder) {
	t.Helper()
	r := &recorder{}
	s, err := Open(Config{State: state, ConfigPath: config, Overrides: overrides, Publish: r.publish, Apply: r.apply,
		Poll: time.Hour, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, r
}

func find(t *testing.T, list []api.Setting, id string) api.Setting {
	t.Helper()
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%s not listed in %+v", id, list)
	return api.Setting{}
}

func refusal(err error) (api.ErrorKind, string) {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Data.Kind, e.Data.ID
	}
	return "", ""
}

// TestStartElsewhereOffAtBalanced: the hab default is the Balanced column, and
// confirm.start_elsewhere is off there (app.md 20.3; owner, 2026-11-01).
func TestStartElsewhereOffAtBalanced(t *testing.T) {
	d, ok := Lookup(StartElsewhere)
	if !ok || string(d.Default) != "false" {
		t.Fatalf("confirm.start_elsewhere default %s, want false", d.Default)
	}
}

func TestDefaults(t *testing.T) {
	s, r := open(t, t.TempDir(), "", nil)
	list := s.List()
	if len(list) != len(Defs) {
		t.Fatalf("listed %d, want %d", len(list), len(Defs))
	}
	want := map[string]string{StartElsewhere: "false", ApprovalMaxAge: "60", LogbookApproval: `"off"`, PermissionWait: "0"}
	for id, v := range want {
		st := find(t, list, id)
		if string(st.Value) != v || string(st.Default) != v || st.Revision != 1 || st.Source != api.SettingSourceDefault {
			t.Errorf("%s: %+v", id, st)
		}
		if r.applied[id] != v {
			t.Errorf("%s applied %q, want %q", id, r.applied[id], v)
		}
	}
}

func TestSetAllOrNothing(t *testing.T) {
	state := t.TempDir()
	s, r := open(t, state, "", nil)
	res, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: StartElsewhere, Value: json.RawMessage(`false`), Revision: 1},
		{ID: ApprovalMaxAge, Value: json.RawMessage(` 0 `), Revision: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Settings) != 2 || string(res.Settings[1].Value) != "0" || res.Settings[1].Revision != 2 || res.Settings[1].Source != api.SettingSourceClient {
		t.Fatalf("set: %+v", res.Settings)
	}
	ev := r.take()
	if len(ev) != 2 || ev[0].Type != api.EventSettingsUpdated || ev[0].ID != StartElsewhere || ev[0].Revision != 2 {
		t.Fatalf("events: %+v", ev)
	}
	if r.applied[StartElsewhere] != "false" {
		t.Fatalf("applied: %v", r.applied)
	}

	cases := []struct {
		name string
		rows []api.SettingWrite
		kind api.ErrorKind
		id   string
	}{
		{"stale revision", []api.SettingWrite{{ID: LogbookApproval, Value: json.RawMessage(`"agents"`), Revision: 1}, {ID: StartElsewhere, Value: json.RawMessage(`true`), Revision: 1}}, api.ErrorKindConflict, StartElsewhere},
		{"unknown id", []api.SettingWrite{{ID: "no.such", Value: json.RawMessage(`1`), Revision: 1}}, api.ErrorKindNotFound, "no.such"},
		{"bad word", []api.SettingWrite{{ID: LogbookApproval, Value: json.RawMessage(`"all"`), Revision: 1}}, api.ErrorKindInvalid, LogbookApproval},
		{"out of range", []api.SettingWrite{{ID: ApprovalMaxAge, Value: json.RawMessage(`1441`), Revision: 2}}, api.ErrorKindInvalid, ApprovalMaxAge},
		{"not whole", []api.SettingWrite{{ID: PermissionWait, Value: json.RawMessage(`1.5`), Revision: 1}}, api.ErrorKindInvalid, PermissionWait},
		{"wrong type", []api.SettingWrite{{ID: StartElsewhere, Value: json.RawMessage(`"no"`), Revision: 2}}, api.ErrorKindInvalid, StartElsewhere},
		{"twice", []api.SettingWrite{{ID: PermissionWait, Value: json.RawMessage(`30`), Revision: 1}, {ID: PermissionWait, Value: json.RawMessage(`31`), Revision: 1}}, api.ErrorKindInvalid, PermissionWait},
	}
	for _, c := range cases {
		_, err := s.Set(api.SettingsSetParams{Values: c.rows})
		if k, id := refusal(err); k != c.kind || id != c.id {
			t.Errorf("%s: got %s %q (%v), want %s %q", c.name, k, id, err, c.kind, c.id)
		}
	}
	// Nothing of a refused call took: logbook.approval is still the default at revision 1.
	if st := find(t, s.List(), LogbookApproval); st.Revision != 1 || st.Source != api.SettingSourceDefault {
		t.Fatalf("a refused call changed a row: %+v", st)
	}
	if ev := r.take(); len(ev) != 0 {
		t.Fatalf("a refused call sent events: %+v", ev)
	}

	// The value after a restart.
	s.Close()
	s2, _ := open(t, state, "", nil)
	if st := find(t, s2.List(), ApprovalMaxAge); string(st.Value) != "0" || st.Revision != 2 || st.Source != api.SettingSourceClient {
		t.Fatalf("after a restart: %+v", st)
	}
}

func TestGetNamed(t *testing.T) {
	s, _ := open(t, t.TempDir(), "", nil)
	res := s.Get(api.SettingsGetParams{IDs: []string{PermissionWait, "newer.setting", PermissionWait}})
	if len(res.Settings) != 1 || res.Settings[0].ID != PermissionWait {
		t.Fatalf("get: %+v", res.Settings)
	}
}

func TestConfigLocks(t *testing.T) {
	state := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	write := func(body string) {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[catalog]\nalways_on = true\n\n[settings]\n\"confirm.start_elsewhere\" = true\n\"approval.max_age\" = 5000\n\"no.such\" = 1\n")
	s, r := open(t, state, cfg, nil)
	st := find(t, s.List(), StartElsewhere)
	if string(st.Value) != "true" || st.Source != api.SettingSourceConfig || st.Revision != 2 {
		t.Fatalf("locked row: %+v", st)
	}
	// A value outside its type is left out, not a lock.
	if st := find(t, s.List(), ApprovalMaxAge); st.Source != api.SettingSourceDefault {
		t.Fatalf("bad config value locked the row: %+v", st)
	}
	if r.applied[StartElsewhere] != "true" {
		t.Fatalf("lock not applied: %v", r.applied)
	}
	_, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: StartElsewhere, Value: json.RawMessage(`false`), Revision: 2}}})
	if k, id := refusal(err); k != api.ErrorKindLocked || id != StartElsewhere {
		t.Fatalf("write to a locked row: %v", err)
	}

	// The lock goes: the revision rises, the event says default.
	write("[settings]\n")
	s.Reload()
	ev := r.take()
	if len(ev) != 1 || ev[0].ID != StartElsewhere || ev[0].Revision != 3 {
		t.Fatalf("events: %+v", ev)
	}
	if d := ev[0].Data.(api.SettingUpdated); string(d.Value) != "false" || d.Source != api.SettingSourceDefault {
		t.Fatalf("event data: %+v", d)
	}

	// A lock added while stopped raises the revision at the next start.
	s.Close()
	write("[settings]\n\"logbook.approval\" = \"agents\"\n")
	s2, _ := open(t, state, cfg, nil)
	if st := find(t, s2.List(), LogbookApproval); st.Revision != 2 || st.Source != api.SettingSourceConfig || string(st.Value) != `"agents"` {
		t.Fatalf("lock across a restart: %+v", st)
	}
	if st := find(t, s2.List(), StartElsewhere); st.Revision != 3 {
		t.Fatalf("an unchanged row moved across a restart: %+v", st)
	}
}

// TestBrokenConfigKeepsTheLocks: a config file that stops parsing while a lock is in place
// keeps that lock, refuses writes to it, sends no event, and raises one notice; the fixed file
// is read at the next pair of checks.
func TestBrokenConfigKeepsTheLocks(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	write := func(body string, at time.Time) {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(cfg, at, at)
	}
	write("[settings]\n\"confirm.start_elsewhere\" = false\n", time.Now())
	r := &recorder{}
	var notices []string
	s, err := Open(Config{State: t.TempDir(), ConfigPath: cfg, Publish: r.publish, Apply: r.apply, Poll: time.Hour, Logf: t.Logf,
		Notify: func(title, body string) { notices = append(notices, title) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rev := find(t, s.List(), StartElsewhere).Revision

	write("[settings\n", time.Now().Add(time.Minute))
	s.check()
	s.check()
	st := find(t, s.List(), StartElsewhere)
	if st.Source != api.SettingSourceConfig || string(st.Value) != "false" || st.Revision != rev {
		t.Fatalf("a broken file dropped the lock: %+v", st)
	}
	_, err = s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: StartElsewhere, Value: json.RawMessage(`true`), Revision: rev}}})
	if k, _ := refusal(err); k != api.ErrorKindLocked {
		t.Fatalf("write under a kept lock: %v", err)
	}
	if ev := r.take(); len(ev) != 0 {
		t.Fatalf("a broken file sent events: %+v", ev)
	}
	// Still broken at a later read: no second notice.
	write("[settings\n\n", time.Now().Add(2*time.Minute))
	s.check()
	s.check()
	if len(notices) != 1 {
		t.Fatalf("notices: %q", notices)
	}

	write("[settings]\n", time.Now().Add(3*time.Minute))
	s.check()
	s.check()
	if st := find(t, s.List(), StartElsewhere); st.Source != api.SettingSourceDefault || st.Revision != rev+1 {
		t.Fatalf("the fixed file was not read: %+v", st)
	}
	if ev := r.take(); len(ev) != 1 {
		t.Fatalf("events after the fix: %+v", ev)
	}
}

// TestConfigFloatForIntegerRefused: a TOML float for a whole-number setting is left out, as a
// JSON 30.0 is refused.
func TestConfigFloatForIntegerRefused(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(cfg, []byte("[settings]\n\"approval.max_age\" = 60.0\n\"hab.permission_wait\" = 30\n"), 0o600)
	s, _ := open(t, t.TempDir(), cfg, nil)
	if st := find(t, s.List(), ApprovalMaxAge); st.Source != api.SettingSourceDefault {
		t.Fatalf("a float locked an integer setting: %+v", st)
	}
	if st := find(t, s.List(), PermissionWait); st.Source != api.SettingSourceConfig || string(st.Value) != "30" {
		t.Fatalf("a whole number did not lock: %+v", st)
	}
	_, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: ApprovalMaxAge, Value: json.RawMessage(`30.0`), Revision: 1}}})
	if k, _ := refusal(err); k != api.ErrorKindInvalid {
		t.Fatalf("JSON 30.0: %v", err)
	}
}

func TestConfigReloadWaitsForTheFileToHold(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	if err := os.WriteFile(cfg, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	s, r := open(t, t.TempDir(), cfg, nil)
	if err := os.WriteFile(cfg, []byte("[settings]\n\"hab.permission_wait\" = 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(cfg, future, future)
	s.check() // the change is seen, not yet read
	if ev := r.take(); len(ev) != 0 {
		t.Fatalf("read before the file held: %+v", ev)
	}
	s.check() // it held for one check
	if ev := r.take(); len(ev) != 1 || ev[0].ID != PermissionWait {
		t.Fatalf("events: %+v", ev)
	}
	if r.applied[PermissionWait] != "30" {
		t.Fatalf("applied: %v", r.applied)
	}
}

func TestOverridesWinOverConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(cfg, []byte("[settings]\n\"hab.permission_wait\" = 30\n"), 0o600)
	s, _ := open(t, t.TempDir(), cfg, map[string]json.RawMessage{PermissionWait: json.RawMessage(`45`)})
	if st := find(t, s.List(), PermissionWait); string(st.Value) != "45" || st.Source != api.SettingSourceConfig {
		t.Fatalf("override: %+v", st)
	}
	if _, err := Open(Config{State: t.TempDir(), Overrides: map[string]json.RawMessage{PermissionWait: json.RawMessage(`500`)}}); err == nil {
		t.Fatal("an override outside its type was accepted")
	}
}

// TestExampleConfig: the [settings] table of docs/examples/hab.toml parses and locks only the
// setting it names.
func TestExampleConfig(t *testing.T) {
	s, _ := open(t, t.TempDir(), filepath.Join("..", "..", "docs", "examples", "hab.toml"), nil)
	for _, st := range s.List() {
		if locked := st.Source == api.SettingSourceConfig; locked != (st.ID == StartElsewhere) {
			t.Errorf("%s: %+v", st.ID, st)
		}
	}
}

func TestBrokenConfigAtStart(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(cfg, []byte("[settings\n"), 0o600)
	if _, err := Open(Config{State: t.TempDir(), ConfigPath: cfg}); err == nil {
		t.Fatal("a config that does not parse was accepted at start")
	}
}

func TestBrokenStateFile(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, StateFile), []byte("{"), 0o600)
	s, _ := open(t, state, "", nil)
	if st := find(t, s.List(), StartElsewhere); st.Revision != 1 || st.Source != api.SettingSourceDefault {
		t.Fatalf("after a broken state file: %+v", st)
	}
}

func TestSaveFailureChangesNothing(t *testing.T) {
	state := t.TempDir()
	s, r := open(t, state, "", nil)
	os.Chmod(state, 0o500)
	defer os.Chmod(state, 0o700)
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	_, err := s.Set(api.SettingsSetParams{Values: []api.SettingWrite{{ID: StartElsewhere, Value: json.RawMessage(`true`), Revision: 1}}})
	if k, id := refusal(err); k != api.ErrorKindUnavailable || id != StartElsewhere {
		t.Fatalf("save failure: %v", err)
	}
	if st := find(t, s.List(), StartElsewhere); st.Revision != 1 || string(st.Value) != "false" {
		t.Fatalf("a failed save changed the row: %+v", st)
	}
	if ev := r.take(); len(ev) != 0 {
		t.Fatalf("a failed save sent events: %+v", ev)
	}
}
