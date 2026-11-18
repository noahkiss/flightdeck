package main

// The CLI side of a Capcom job's words (rounds.md 13, capcom-service.md 9.6): -actions on add
// and edit, and export then apply keeps them.

import (
	"bytes"
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/noahkiss/flightdeck/api"
)

func TestRoundCLIActions(t *testing.T) {
	t.Parallel()
	parse := func(args ...string) (api.RoundSpec, error) {
		var d roundDef
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		d.flags(fs)
		if err := fs.Parse(args); err != nil {
			return api.RoundSpec{}, err
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		spec := api.RoundSpec{Mode: api.RoundModeCapcom, Actions: []api.RoundAction{"fetch"}}
		err := d.apply(&spec, set, fs.Args())
		return spec, err
	}
	s, err := parse("-actions", "hand_off, card", "-manual", "watch the mail")
	if err != nil || !slices.Equal(s.Actions, []api.RoundAction{"hand_off", "card"}) {
		t.Fatalf("-actions: %v %v", s.Actions, err)
	}
	if s, err := parse("-actions", "", "-manual", "p"); err != nil || len(s.Actions) != 0 {
		t.Fatalf(`-actions "": %v %v`, s.Actions, err)
	}
	// Not given: an edit keeps the stored words.
	if s, err := parse("-manual", "p"); err != nil || !slices.Equal(s.Actions, []api.RoundAction{"fetch"}) {
		t.Fatalf("no -actions: %v %v", s.Actions, err)
	}

	// Export then apply keeps the words.
	r := api.Round{ID: "r1", RoundSpec: api.RoundSpec{Name: "mail", Mode: api.RoundModeCapcom, Prompt: "p",
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}, Actions: []api.RoundAction{"hand_off", "fetch"}}}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(roundsFile{Rounds: []fileRound{toFile(r)}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`actions = ["hand_off", "fetch"]`)) {
		t.Fatalf("export:\n%s", buf.String())
	}
	var back roundsFile
	if _, err := toml.Decode(buf.String(), &back); err != nil || len(back.Rounds) != 1 {
		t.Fatalf("decode: %v", err)
	}
	spec, err := back.Rounds[0].spec()
	if err != nil || !slices.Equal(spec.Actions, r.Actions) {
		t.Fatalf("apply spec: %v %v", spec.Actions, err)
	}
}
