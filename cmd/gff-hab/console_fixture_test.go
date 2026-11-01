package main

// The console fixtures the app's card tests read (app.md 6): a hand-written byte stream with
// prompt marks goes through the hab's tracker and command API shape, and the result is what
// command.list and command.output would answer for it.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/cmdmarks"
)

var updateConsole = flag.Bool("update-console", false, "write the expected command files of the console fixtures")

type consoleFixture struct {
	Start  uint64 `json:"start"`
	Chunks []struct {
		At   time.Time `json:"at"`
		Data string    `json:"data"`
	} `json:"chunks"`
}

// TestConsoleFixtures: each testdata/console/<name>.marks.json, fed chunk by chunk at its
// times, gives the commands and outputs in <name>.commands.json: one command.output answer per
// command, oldest first, holding the command's whole output. -update-console rewrites them.
func TestConsoleFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "console", "*.marks.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no console fixtures: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx consoleFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		tr := cmdmarks.New(fx.Start)
		var stream []byte
		off := fx.Start
		for _, c := range fx.Chunks {
			tr.Feed(off, []byte(c.Data), c.At)
			stream = append(stream, c.Data...)
			off += uint64(len(c.Data))
		}
		cs, _ := tr.List(nil, cmdmarks.MaxCommands)
		out := make([]api.CommandOutput, 0, len(cs))
		for _, c := range cs {
			end := off
			if c.End != nil {
				end = *c.End
			}
			out = append(out, api.CommandOutput{Command: apiCommand(c), Start: c.Output, End: end,
				Data: stream[c.Output-fx.Start : end-fx.Start], More: c.End == nil})
		}
		got, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		want := strings.TrimSuffix(f, ".marks.json") + ".commands.json"
		if *updateConsole {
			if err := os.WriteFile(want, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := os.ReadFile(want)
		if err != nil {
			t.Fatalf("%v (run go test ./cmd/gff-hab -run TestConsoleFixtures -update-console)", err)
		}
		if !bytes.Equal(got, w) {
			t.Errorf("%s: commands differ from %s:\n%s", f, want, got)
		}
	}
}
