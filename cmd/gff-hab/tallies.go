package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/diaglog"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/tallies"
)

// talliesOn reads [diagnostics] tallies from the hab's config: on unless set false (7.3).
func talliesOn(path string, logf func(string, ...any)) bool {
	if path == "" {
		return true
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		logf("tallies: %v; tallies stay on", err)
		return true
	}
	var fc struct {
		Diagnostics struct {
			Tallies *bool `toml:"tallies"`
		} `toml:"diagnostics"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return true // the config's own readers report a file that does not parse
	}
	return fc.Diagnostics.Tallies == nil || *fc.Diagnostics.Tallies
}

// logLevel reads [diagnostics] log_level from the hab's config (diagnostics.md 4.5): debug,
// info, warn or error. Missing, or a word it does not know: info.
func logLevel(path string, logf func(string, ...any)) diaglog.Level {
	if path == "" {
		return diaglog.LevelInfo
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return diaglog.LevelInfo // tallies report a config they cannot read
	}
	var fc struct {
		Diagnostics struct {
			LogLevel string `toml:"log_level"`
		} `toml:"diagnostics"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil || fc.Diagnostics.LogLevel == "" {
		return diaglog.LevelInfo
	}
	l, ok := diaglog.ParseLevel(fc.Diagnostics.LogLevel)
	if !ok {
		logf("diagnostics: log_level %q is not debug, info, warn or error; the floor stays info", fc.Diagnostics.LogLevel)
	}
	return l
}

// openTallies starts gff-hab's tallies (diagnostics.md 7), or returns nil when the config
// turns them off. It counts this start of gff-hab.
func openTallies(state, configPath string, logf func(string, ...any)) *tallies.Tallies {
	if !talliesOn(configPath, logf) {
		logf("tallies: off ([diagnostics] tallies = false)")
		return nil
	}
	t := tallies.New(tallies.Config{Dir: filepath.Join(state, tallies.DirName), Surface: tallies.Current(), Logf: logf})
	t.Health("hab", tallies.KindStart)
	t.Start()
	return t
}

// controlTally is the control server's counter: nil when tallies are off, so the server skips
// the call.
func controlTally(t *tallies.Tallies) control.Tally {
	if t == nil {
		return nil
	}
	return t
}

// settingsInConfig returns the settings the hab's config or a flag locks, for the tallies.
func settingsInConfig(s *settings.Service) func() []string {
	return func() []string {
		var ids []string
		for _, st := range s.List() {
			if st.Source == api.SettingSourceConfig {
				ids = append(ids, st.ID)
			}
		}
		return ids
	}
}

// channelTally counts the channel features of a record this hab sent: a message queued, or
// expired unsent (7.2).
func channelTally(t *tallies.Tallies, r api.ChannelRecord) {
	switch {
	case r.State == api.ChannelStateQueued && r.At == r.Created:
		t.Feature(tallies.FeatureChannelSend)
	case r.State == api.ChannelStateExpired:
		t.Feature(tallies.FeatureChannelExpired)
	}
}

// talliesCmd prints the tallies of this host's hab (diagnostics.md 7.4). It reads files, so it
// works while gff-hab is down.
func talliesCmd(args []string, out io.Writer) error {
	fl := flag.NewFlagSet("tallies", flag.ExitOnError)
	state := fl.String("state", "", "state directory (default: see gff-hab -h)")
	days := fl.Int("days", 7, "window in days, today included: calls and errors per method and client kind, events, hooks, features, settings, health")
	unused := fl.Int("unused", 0, "list the methods, events, hook events, features and settings of this build with no use in the last `n` days")
	asJSON := fl.Bool("json", false, "print the raw figures as JSON")
	clear := fl.Bool("clear", false, "delete the tallies")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), "usage: gff-hab tallies [-state dir] [-days n | -unused n] [-json]\n"+
			"       gff-hab tallies -clear [-state dir]\n\n"+
			"Local counts of API calls, events, hook envelopes, features, settings and health. Names and\n"+
			"numbers only, kept in <state>/tallies, never sent anywhere. [diagnostics] tallies = false in\n"+
			"the hab's config turns them off.\n\n")
		fl.PrintDefaults()
	}
	fl.Parse(args)
	if fl.NArg() != 0 {
		return errors.New("tallies takes no arguments")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	tdir := filepath.Join(dir, tallies.DirName)
	if *clear {
		if err := tallies.Clear(tdir); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "deleted %s\n", tdir)
		return err
	}
	now := time.Now()
	if *unused > 0 {
		seen, err := tallies.ReadSeen(tdir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		w, err := tallies.ReadWindow(tdir, now, *unused)
		if err != nil {
			return err
		}
		us := tallies.FindUnused(tallies.Current(), seen, now, *unused)
		if *asJSON {
			return writeJSONLine(out, struct {
				Days   int              `json:"days"`
				Since  string           `json:"since"`
				Files  int              `json:"files"`
				Unused []tallies.Unused `json:"unused"`
			}{*unused, seen.Since, w.Files, us})
		}
		return tallies.PrintUnused(out, us, seen.Since, *unused, w.Files)
	}
	w, err := tallies.ReadWindow(tdir, now, *days)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSONLine(out, w)
	}
	return tallies.PrintWindow(out, w)
}

func writeJSONLine(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", b)
	return err
}

// routeTally counts a pane route call as a call of client kind route (diagnostics.md 7.1).
func routeTally(t *tallies.Tallies) func(method, errKind string, failed bool) {
	return func(method, errKind string, failed bool) { t.Call(method, tallies.ClientRoute, errKind, failed) }
}
