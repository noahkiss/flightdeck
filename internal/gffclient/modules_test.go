package gffclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/modhost"
	"github.com/noahkiss/flightdeck/internal/modview"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// hostModule is a ModuleSource that runs one module in-process with internal/modhost, as the
// hab's module host will: an action runs the module's act, then its render, and the new tree
// arrives through Changed (modules.md 4, 5).
type hostModule struct {
	t       *testing.T
	host    string
	m       *modhost.Module
	changed chan struct{}
	callMu  sync.Mutex

	mu   sync.Mutex
	card ModuleCard
	acts []string
}

func newHostModule(t *testing.T, host, dir string) *hostModule {
	t.Helper()
	b, err := modhost.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := modhost.New(b, modhost.Options{Backends: modhost.Backends{State: &modhost.MemStore{}}})
	if err != nil {
		t.Fatal(err)
	}
	s := &hostModule{t: t, host: host, m: m, changed: make(chan struct{}, 1),
		card: ModuleCard{Host: host, Module: b.Manifest.ID, Name: b.Manifest.Name}}
	s.render(context.Background())
	return s
}

func (s *hostModule) render(ctx context.Context) {
	tree, err := s.m.Render(ctx, modhost.Call{Host: s.host, OS: "linux", Place: "board", Vocabulary: modules.Version})
	s.mu.Lock()
	if err != nil {
		s.card.Err = err.Error() // the last good tree stays (modules.md 8.3)
	} else {
		s.card.Tree, s.card.Err, s.card.At = tree, "", time.Now()
	}
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *hostModule) Cards(place string) []ModuleCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if place != "board" {
		return nil
	}
	return []ModuleCard{s.card}
}

func (s *hostModule) Act(ctx context.Context, host, module string, a modules.Action) error {
	if modview.IsClient(a) {
		s.t.Errorf("a client action reached the module: %s", a.Name)
	}
	args, err := json.Marshal(a.Args)
	if err != nil {
		return err
	}
	// One action and its render at a time, so a slower render never publishes an older tree
	// over a newer one; the hab orders trees by revision for the same reason.
	s.callMu.Lock()
	defer s.callMu.Unlock()
	s.mu.Lock()
	s.acts = append(s.acts, a.Name+" "+string(args))
	s.mu.Unlock()
	if err := s.m.Act(ctx, a.Name, args, modhost.Call{Host: host, Place: "board"}); err != nil {
		return err
	}
	s.render(ctx)
	return nil
}

func (s *hostModule) Changed() <-chan struct{} { return s.changed }

func (s *hostModule) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acts...)
}

func moduleHost(t *testing.T, caps ...string) hosts.Spec {
	t.Helper()
	a := fakeHab(t, shortDir(t), "a")
	if len(caps) > 0 {
		a.Capabilities = caps
		a.Stop()
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
	}
	return hosts.Spec{Label: "one", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: a.Sock}}}
}

// TestModuleCard drives a sample module through its card on the status board: the card draws,
// the button's key hint runs the module's act, the state changes and the card draws again. The
// toggle, the field, a confirm, a client action and a failed render go the same way.
func TestModuleCard(t *testing.T) {
	src := newHostModule(t, "one", "testdata/modules/counter")
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = src }, moduleHost(t, ModulesCapability))
	hst.typ(cmdKey + "g")
	hst.waitScreen("the card under Modules", func(s string) bool {
		return line(s, " Modules") >= 0 && line(s, "Counter · one") > line(s, " Modules") &&
			strings.Contains(s, "Count 0") && strings.Contains(s, "a Add one  z Zero  c Copy  b Break")
	})

	hst.typ("a")
	hst.waitScreen("the count after the key hint", func(s string) bool { return strings.Contains(s, "Count 1") })
	hst.typ("aa")
	hst.waitScreen("two more", func(s string) bool { return strings.Contains(s, "Count 3") })

	// A key hint moves the selection to its button; two up is the toggle.
	hst.typ("kk\r")
	hst.waitScreen("the toggle on", func(s string) bool { return strings.Contains(s, "[x] Watch") })
	hst.typ(" ")
	hst.waitScreen("Space turns it off", func(s string) bool { return strings.Contains(s, "[ ] Watch") })

	hst.typ("j\r")
	hst.waitScreen("the field's prompt", func(s string) bool { return strings.Contains(s, "Note: ") })
	hst.typ("ship it\r")
	hst.waitScreen("the note the module kept", func(s string) bool { return strings.Contains(s, "Note: ship it") })

	hst.typ("z")
	hst.waitScreen("the button's question", func(s string) bool { return strings.Contains(s, "Set the count to zero? (y/n)") })
	// A no sends nothing: one more add makes 4, not 1.
	hst.typ("na")
	hst.waitScreen("the count after a no and an add", func(s string) bool { return strings.Contains(s, "Count 4") })
	hst.typ("zy")
	hst.waitScreen("zero after a yes", func(s string) bool { return strings.Contains(s, "Count 0") })

	hst.typ("c")
	hst.waitScreen("copied", func(s string) bool { return strings.Contains(s, "copied") })
	osc := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("count 0")) + "\x07"
	if !bytes.Contains(hst.output(), []byte(osc)) {
		t.Error("client.copy wrote no OSC 52 with the text")
	}

	want := []string{`counter.add {"by":1}`, `counter.add {"by":1}`, `counter.add {"by":1}`, `watch.set {"value":true}`,
		`watch.set {"value":false}`, `note.set {"text":"ship it"}`, `counter.add {"by":1}`, `counter.zero null`}
	if got := src.actions(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("actions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A render that fails keeps the last good tree, with the error under it.
	hst.typ("b")
	hst.waitScreen("the error under the last tree", func(s string) bool {
		return strings.Contains(s, "module error: ") && strings.Contains(s, "Count 0")
	})
}

// staticModules is a ModuleSource with fixed cards.
type staticModules []ModuleCard

func (s staticModules) Cards(string) []ModuleCard { return s }
func (s staticModules) Act(context.Context, string, string, modules.Action) error {
	return nil
}
func (s staticModules) Changed() <-chan struct{} { return nil }

// TestModuleCardHidden: a hab whose welcome lacks the modules capability shows no Modules
// section, even with a source; a tree that is not a block tree draws one error line.
func TestModuleCardHidden(t *testing.T) {
	cards := staticModules{{Host: "one", Module: "m", Name: "Broken", Tree: []byte(`{"type":`)}}
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = cards }, moduleHost(t))
	hst.typ(cmdKey + "g")
	// The frame that shows the host online is drawn from its welcome: no capability, no section.
	var board string
	hst.waitScreen("the board with the host online", func(s string) bool {
		board = s
		return strings.Contains(s, "status board") && strings.Contains(s, "1/1 hosts")
	})
	if strings.Contains(board, "Modules") || strings.Contains(board, "Broken") {
		t.Fatalf("a module section without the capability:\n%s", board)
	}

	hst2 := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst2, func(c *Config) { c.Modules = cards }, moduleHost(t, ModulesCapability))
	hst2.typ(cmdKey + "g")
	hst2.waitScreen("one error line for the broken tree", func(s string) bool {
		l := line(s, "Broken · one")
		return l > 0 && strings.Contains(strings.Split(s, "\n")[l+1], "module error: not a block tree")
	})
}

func TestModStyle(t *testing.T) {
	if st := modStyle(modview.Style{Tint: "blocked", Focus: true}); !st.Bold || !st.Inverse || st.Fg != palette(1) {
		t.Errorf("blocked, focused: %+v", st)
	}
	if st := modStyle(modview.Style{Tint: "muted"}); !st.Faint {
		t.Errorf("muted: %+v", st)
	}
	for hex, want := range map[string]uint8{"#3A7BD5": 4, "#FF0000": 1, "#00ff00": 2, "#ffffff": 7, "#000000": 0, "#F0C020": 3} {
		if got := modStyle(modview.Style{Tint: hex}).Fg; got != palette(want) {
			t.Errorf("%s: %+v, want palette %d", hex, got, want)
		}
	}
	if st := modStyle(modview.Style{Tint: "ultraviolet"}); st != (vt.Style{}) {
		t.Errorf("unknown tint: %+v", st)
	}
}
