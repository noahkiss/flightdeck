package gffclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
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

// hostModule is a ModuleSource that runs one module in a module host process (the built
// gff-hab module-host, through modhost.Host), as the hab will: an action runs the module's
// act, then its render, and the new tree arrives through Changed (modules.md 4, 5, 7.4).
type hostModule struct {
	t       *testing.T
	host    string
	m       modhost.Runner
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
	mh := modhost.NewHost(modhost.HostOptions{Command: func() *exec.Cmd { return exec.Command(habBin, "module-host") }})
	t.Cleanup(func() { mh.Close() })
	m, err := mh.New(b, modhost.Options{Backends: modhost.Backends{State: &modhost.MemStore{}}})
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

// liveModules is a ModuleSource whose cards a test sets, and which records the actions.
type liveModules struct {
	mu      sync.Mutex
	cards   []ModuleCard
	acts    []string
	changed chan struct{}
}

func newLiveModules(cards ...ModuleCard) *liveModules {
	return &liveModules{cards: cards, changed: make(chan struct{}, 1)}
}

func (s *liveModules) Cards(string) []ModuleCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ModuleCard(nil), s.cards...)
}

func (s *liveModules) Act(_ context.Context, host, module string, a modules.Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acts = append(s.acts, host+"/"+module+" "+a.Name)
	return nil
}

func (s *liveModules) Changed() <-chan struct{} { return s.changed }

func (s *liveModules) setTree(i int, tree string) {
	s.mu.Lock()
	s.cards[i].Tree, s.cards[i].At = []byte(tree), time.Now()
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *liveModules) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acts...)
}

// TestModuleTextNeverControls: a manifest name, a field placeholder, a confirm question and the
// text of client.copy, each holding ESC and U+009B (CSI), never reach the host terminal as
// controls: the bytes gff writes hold none of the module's sequences.
func TestModuleTextNeverControls(t *testing.T) {
	const evil = "\x1b]0;pwn\x07\u009b6n\x1b[99X"
	tree := `{"type":"stack","children":[
		{"type":"field","id":"f","placeholder":` + jsonString(evil+"ph") + `,"action":{"name":"set"}},
		{"type":"button","id":"b","title":"Go","confirm":` + jsonString(evil+"sure") + `,"action":{"name":"go"}},
		{"type":"button","id":"c","title":"Copy","action":{"name":"client.copy","args":{"text":` + jsonString(evil+"clip") + `}}},
		{"type":"button","id":"u","title":"Url","action":{"name":"client.open_url","args":{"url":` + jsonString("https://example.com/"+evil) + `}}},
		{"type":"button","id":"v","title":"Visit","action":{"name":"client.open_url","args":{"url":"https://example.com/\u009b6n\u202e"}}}]}`
	src := newLiveModules(ModuleCard{Host: "one", Module: "m", Name: evil + "Name", Tree: []byte(tree), At: time.Now()})
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = src }, moduleHost(t, ModulesCapability))
	hst.typ(cmdKey + "g")
	hst.waitScreen("the card", func(s string) bool { return strings.Contains(s, "�]0;pwn��6n�[99XName · one") })
	hst.typ("\r")
	hst.waitScreen("the field's prompt", func(s string) bool { return strings.Contains(s, "�]0;pwn��6n�[99Xph: ") })
	hst.typ("\x1b")
	hst.typ("g")
	hst.waitScreen("the confirm", func(s string) bool { return strings.Contains(s, "�]0;pwn��6n�[99Xsure (y/n)") })
	hst.typ("n")
	hst.typ("c")
	hst.waitScreen("the copy note", func(s string) bool { return strings.Contains(s, "copied 20 chars: �]0;pwn��6n�[99Xclip") })
	// url.Parse refuses a control character; U+009B and a bidi override go percent-encoded.
	hst.typ("u")
	hst.waitScreen("the refused link", func(s string) bool { return strings.Contains(s, "not an https link") })
	hst.typ("v")
	hst.waitScreen("the link", func(s string) bool {
		return strings.Contains(s, "example.com · https://example.com/%C2%9B6n%E2%80%AE")
	})
	out := hst.output()
	for _, bad := range []string{"\x1b]0;", "\u009b", "\x9b6n", "\x1b[99X"} {
		if bytes.Contains(out, []byte(bad)) {
			t.Errorf("gff wrote the module's %q", bad)
		}
	}
	if n := bytes.Count(out, []byte("\x1b]52;")); n != 1 {
		t.Errorf("%d OSC 52 writes, want 1", n)
	}
	if len(src.actions()) != 0 {
		t.Errorf("actions sent: %q", src.actions())
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestModuleHintsOnSelectedCard: only the card that holds the selection draws its key hints,
// and a hint acts on that card; the other card shows "›".
func TestModuleHintsOnSelectedCard(t *testing.T) {
	btn := func(title string) string {
		return `{"type":"button","id":"x","title":"` + title + `","action":{"name":"` + strings.ToLower(title) + `"}}`
	}
	src := newLiveModules(
		ModuleCard{Host: "one", Module: "a", Name: "First", Tree: []byte(btn("Erase")), At: time.Now()},
		ModuleCard{Host: "one", Module: "b", Name: "Second", Tree: []byte(btn("Edit")), At: time.Now()})
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = src }, moduleHost(t, ModulesCapability))
	hst.typ(cmdKey + "g")
	hst.waitScreen("hints on the first card only", func(s string) bool {
		return strings.Contains(s, "e Erase") && strings.Contains(s, "› Edit")
	})
	hst.typ("j")
	hst.waitScreen("hints on the second card only", func(s string) bool {
		return strings.Contains(s, "› Erase") && strings.Contains(s, "e Edit")
	})
	hst.typ("e")
	waitUntil(t, "the second card's action", func() bool { return len(src.actions()) == 1 })
	if got := src.actions(); got[0] != "one/b edit" {
		t.Errorf("actions %q", got)
	}
}

// TestModuleEnterAfterChange: Enter right after the selected target changed under the owner is
// ignored once; the next Enter presses what is drawn.
func TestModuleEnterAfterChange(t *testing.T) {
	btn := func(title string) string {
		return `{"type":"button","id":"x","title":"` + title + `","action":{"name":"` + strings.ToLower(title) + `"}}`
	}
	src := newLiveModules(ModuleCard{Host: "one", Module: "a", Name: "First", Tree: []byte(btn("Start")), At: time.Now()})
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = src }, moduleHost(t, ModulesCapability))
	hst.typ(cmdKey + "g")
	hst.waitScreen("the button", func(s string) bool { return strings.Contains(s, "s Start") })
	src.setTree(0, btn("Remove"))
	hst.waitScreen("the new button", func(s string) bool { return strings.Contains(s, "e Remove") })
	hst.typ("\r")
	hst.waitScreen("the note", func(s string) bool { return strings.Contains(s, "the selection changed; press again") })
	if got := src.actions(); len(got) != 0 {
		t.Fatalf("Enter after a change sent %q", got)
	}
	hst.typ("\r")
	waitUntil(t, "the action", func() bool { return len(src.actions()) == 1 })
	if got := src.actions(); got[0] != "one/a remove" {
		t.Errorf("actions %q", got)
	}
}

// TestModuleCardKeepsSessionKeys: with the selection on a module target, the rename and close
// keys do nothing to the session row under it.
func TestModuleCardKeepsSessionKeys(t *testing.T) {
	a := fakeHab(t, shortDir(t), "a")
	a.Capabilities = []string{ModulesCapability}
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AddSession("sh"); err != nil {
		t.Fatal(err)
	}
	src := newLiveModules(ModuleCard{Host: "one", Module: "a", Name: "First", At: time.Now(),
		Tree: []byte(`{"type":"button","id":"x","title":"Go","action":{"name":"go"}}`)})
	hst := newHost(t, 100, 30, false)
	// The default board binds no rename or close key; a keymap may.
	km := DefaultKeymap()
	km.bind(ModeBoard, "N", actRename)
	km.bind(ModeBoard, "X", actClose)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules, c.Keymap = src, km },
		hosts.Spec{Label: "one", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: a.Sock}}})
	hst.typ(cmdKey + "g")
	hst.waitScreen("the session and the card", func(s string) bool {
		return line(s, " sh ") > 0 && line(s, "First · one") > 0
	})
	// On the session row the keys work.
	hst.typ("kkN")
	hst.waitScreen("the rename prompt", func(s string) bool { return strings.Contains(s, "rename sh on one") })
	hst.typ("\x1b")
	hst.waitScreen("the prompt gone", func(s string) bool { return !strings.Contains(s, "rename sh") })
	// Move the selection onto the card: down past the session row.
	hst.typ("jj")
	hst.waitScreen("the selection on the card", func(s string) bool { return strings.Contains(s, "g Go") })
	hst.typ("NX")
	time.Sleep(100 * time.Millisecond)
	if s := hst.screen(); strings.Contains(s, "rename") || strings.Contains(s, "end sh") {
		t.Errorf("a session key acted under a module target:\n%s", s)
	}
	if len(src.actions()) != 0 || len(a.Sessions()) != 1 {
		t.Errorf("actions %q, sessions %d", src.actions(), len(a.Sessions()))
	}
}

// TestModuleClientActions: client.copy sends nothing for empty or too long text, and
// client.open_url shows only an https link.
func TestModuleClientActions(t *testing.T) {
	tree := `{"type":"stack","children":[
		{"type":"button","id":"e","title":"Empty","action":{"name":"client.copy","args":{"text":""}}},
		{"type":"button","id":"n","title":"Number","action":{"name":"client.copy","args":{"text":5}}},
		{"type":"button","id":"h","title":"Http","action":{"name":"client.open_url","args":{"url":"http://example.com/x"}}},
		{"type":"button","id":"s","title":"Secure","action":{"name":"client.open_url","args":{"url":"https://example.com/a/very/long/path"}}}]}`
	src := newLiveModules(ModuleCard{Host: "one", Module: "m", Name: "M", Tree: []byte(tree), At: time.Now()})
	hst := newHost(t, 100, 30, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Modules = src }, moduleHost(t, ModulesCapability))
	hst.typ(cmdKey + "g")
	hst.waitScreen("the card", func(s string) bool { return strings.Contains(s, "e Empty") })
	hst.typ("e")
	hst.waitScreen("nothing to copy", func(s string) bool { return strings.Contains(s, "nothing to copy") })
	hst.typ("h")
	hst.waitScreen("not https", func(s string) bool { return strings.Contains(s, "not an https link") })
	hst.typ("n")
	hst.waitScreen("a number copies nothing", func(s string) bool { return strings.Contains(s, "nothing to copy") })
	hst.typ("s")
	hst.waitScreen("the host first", func(s string) bool {
		return strings.Contains(s, "example.com · https://example.com/a/very/long/path")
	})
	if bytes.Contains(hst.output(), []byte("\x1b]52;")) {
		t.Error("an OSC 52 was written")
	}
	// A tree cannot hold text past the cap (its own limit is smaller); a source that bypasses
	// the tree still cannot.
	u := &workspaceUI{c: &client{}}
	u.clientAction(ModuleCard{}, modules.Action{Name: "client.copy", Args: map[string]any{"text": strings.Repeat("x", copyMax+1)}})
	if u.c.out.Len() != 0 || u.msg != "not copied: 75777 bytes; the limit is 75776" {
		t.Errorf("too long: wrote %d bytes, note %q", u.c.out.Len(), u.msg)
	}
}

// TestCanvasTextControls: the canvas draws any control character as U+FFFD, whoever passes it.
func TestCanvasTextControls(t *testing.T) {
	var c canvas
	c.resize(10, 1)
	c.text(0, 0, 10, "a\x1b[\u009b\x7f\x00b", vt.Style{})
	var got string
	for _, hc := range c.next[:7] {
		got += hc.text
	}
	if got != "a�[���b" {
		t.Errorf("cells %q", got)
	}
}
