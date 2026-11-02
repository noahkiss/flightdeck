package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// settingsClient dials the socket and says hello as an app.
func settingsClient(t *testing.T, sock string) *control.Client {
	t.Helper()
	c, err := control.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func settingOf(t *testing.T, list []api.Setting, id string) api.Setting {
	t.Helper()
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%s not in %+v", id, list)
	return api.Setting{}
}

// setErr calls settings.set and returns the refusal's kind and data.id.
func setErr(t *testing.T, c *control.Client, rows ...api.SettingWrite) (api.ErrorKind, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	err := c.Call(ctx, "settings.set", api.SettingsSetParams{Values: rows}, nil)
	var e *control.Error
	if !errors.As(err, &e) {
		t.Fatalf("settings.set %+v: want a refusal, got %v", rows, err)
	}
	return e.Data.Kind, e.Data.ID
}

// TestSettingsServe: the hab-side settings over a real control socket (app.md 20.5): the
// capability, the host snapshot, settings.get, an all-or-nothing settings.set, a locked row and
// a stale revision refused with data.id, the event to a host subscriber only, the value after a
// restart, and -permission-wait as a lock.
func TestSettingsServe(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	state := filepath.Join(d, "state")
	cfg := filepath.Join(d, "hab.toml")
	if err := os.WriteFile(cfg, []byte("[settings]\n\"logbook.approval\" = \"agents\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"GFF_HAB_CONFIG=" + cfg, "GFF_HAB_PERMISSION_WAIT="}
	s := startServeWith(t, sock, state, env)
	if !slices.Contains(welcome(t, sock).Capabilities, settings.Capability) {
		t.Fatal("no settings capability")
	}

	// A host subscriber gets the settings in its snapshot; a sessions subscriber does not.
	host := settingsClient(t, sock)
	var snap api.StructureResult
	call(t, host, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, &snap)
	if len(snap.Settings) != len(settings.Defs) {
		t.Fatalf("snapshot settings: %+v", snap.Settings)
	}
	if st := settingOf(t, snap.Settings, settings.LogbookApproval); st.Source != api.SettingSourceConfig || string(st.Value) != `"agents"` {
		t.Fatalf("locked row in the snapshot: %+v", st)
	}
	narrow := settingsClient(t, sock)
	var nsnap api.StructureResult
	call(t, narrow, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"none"}}}, &nsnap)
	if nsnap.Settings != nil {
		t.Fatalf("a sessions scope got settings: %+v", nsnap.Settings)
	}

	c := settingsClient(t, sock)
	var got api.SettingsResult
	call(t, c, "settings.get", api.SettingsGetParams{}, &got)
	se := settingOf(t, got.Settings, settings.StartElsewhere)
	if se.Revision != 1 || se.Source != api.SettingSourceDefault || string(se.Value) != "true" {
		t.Fatalf("get: %+v", se)
	}
	var set api.SettingsResult
	call(t, c, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: settings.StartElsewhere, Value: json.RawMessage(`false`), Revision: 1},
		{ID: settings.PermissionWait, Value: json.RawMessage(`30`), Revision: 1},
	}}, &set)
	if len(set.Settings) != 2 || set.Settings[0].Revision != 2 || set.Settings[0].Source != api.SettingSourceClient {
		t.Fatalf("set: %+v", set.Settings)
	}

	// The event reaches the host subscriber.
	seen := map[string]api.Event{}
	deadline := time.After(scaleTimeout(10 * time.Second))
	for len(seen) < 2 {
		select {
		case n := <-host.Notifications():
			if n.Method != "event" {
				continue
			}
			e, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			if e.Type == api.EventSettingsUpdated {
				seen[e.ID] = e
			}
		case <-deadline:
			t.Fatalf("settings.updated events: %+v", seen)
		}
	}
	e := seen[settings.StartElsewhere]
	var data api.SettingUpdated
	if err := json.Unmarshal(e.Data, &data); err != nil || e.Revision == nil || *e.Revision != 2 || string(data.Value) != "false" || data.Source != api.SettingSourceClient {
		t.Fatalf("event: %+v %+v %v", e, data, err)
	}

	// A locked row and a stale revision are refused, and name their row; nothing else takes.
	if k, id := setErr(t, c, api.SettingWrite{ID: settings.ApprovalMaxAge, Value: json.RawMessage(`0`), Revision: 1},
		api.SettingWrite{ID: settings.LogbookApproval, Value: json.RawMessage(`"off"`), Revision: 1}); k != api.ErrorKindLocked || id != settings.LogbookApproval {
		t.Fatalf("locked: %s %q", k, id)
	}
	if k, id := setErr(t, c, api.SettingWrite{ID: settings.ApprovalMaxAge, Value: json.RawMessage(`0`), Revision: 1},
		api.SettingWrite{ID: settings.StartElsewhere, Value: json.RawMessage(`true`), Revision: 1}); k != api.ErrorKindConflict || id != settings.StartElsewhere {
		t.Fatalf("conflict: %s %q", k, id)
	}
	call(t, c, "settings.get", api.SettingsGetParams{IDs: []string{settings.ApprovalMaxAge}}, &got)
	if len(got.Settings) != 1 || got.Settings[0].Revision != 1 || string(got.Settings[0].Value) != "60" {
		t.Fatalf("a refused call took effect: %+v", got.Settings)
	}

	// The narrow subscriber never got a settings event.
	select {
	case n := <-narrow.Notifications():
		if ev, err := n.Event(); err == nil && ev.Type == api.EventSettingsUpdated {
			t.Fatalf("a sessions scope got %+v", ev)
		}
	default:
	}

	// The value after a restart.
	s.stop(t)
	s = startServeWith(t, sock, state, env)
	c = settingsClient(t, sock)
	call(t, c, "settings.get", api.SettingsGetParams{}, &got)
	if st := settingOf(t, got.Settings, settings.StartElsewhere); st.Revision != 2 || string(st.Value) != "false" || st.Source != api.SettingSourceClient {
		t.Fatalf("after a restart: %+v", st)
	}
	if st := settingOf(t, got.Settings, settings.PermissionWait); string(st.Value) != "30" {
		t.Fatalf("permission wait after a restart: %+v", st)
	}

	// -permission-wait locks hab.permission_wait.
	s.stop(t)
	startServeWith(t, sock, state, env, "-permission-wait", "45s")
	c = settingsClient(t, sock)
	call(t, c, "settings.get", api.SettingsGetParams{IDs: []string{settings.PermissionWait}}, &got)
	if st := got.Settings[0]; string(st.Value) != "45" || st.Source != api.SettingSourceConfig {
		t.Fatalf("flag lock: %+v", st)
	}
	if k, id := setErr(t, c, api.SettingWrite{ID: settings.PermissionWait, Value: json.RawMessage(`0`), Revision: got.Settings[0].Revision}); k != api.ErrorKindLocked || id != settings.PermissionWait {
		t.Fatalf("write to the flag lock: %s %q", k, id)
	}
}

// TestPermissionWaitLock: the flag or the environment locks hab.permission_wait in whole
// seconds; neither leaves it to the app.
func TestPermissionWaitLock(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	cases := []struct {
		flag  time.Duration
		given bool
		env   string
		want  string
		bad   bool
	}{
		{0, false, "", "", false},
		{0, true, "", "0", false},
		{30 * time.Second, true, "10", "30", false},
		{0, false, "15", "15", false},
		{0, false, "1500ms", "2", false},
		{0, false, "0", "0", false},
		{0, false, "500ms", "", true},
		{0, false, "3m", "", true},
		{0, false, "soon", "", true},
	}
	for _, c := range cases {
		got, err := permissionWaitLock(c.flag, c.given, env(c.env))
		if (err != nil) != c.bad || string(got) != c.want {
			t.Errorf("%v %v %q: got %q %v", c.flag, c.given, c.env, got, err)
		}
	}
}
