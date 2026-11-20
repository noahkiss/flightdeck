package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// grantHarness is a Capcom engine over a real rounds service: the Capcom route's round.create
// makes the job, the engine's card answer grants (capcom-service.md 9.9).
type grantHarness struct {
	e      *capcomEngine
	svc    *roundsService
	st     *structure.Store
	set    map[string]string
	opened bool
	// turn is the running turn; each create opens the next unless keepTurn holds it.
	turn     string
	n        int
	keepTurn bool
}

func newGrantHarness(t *testing.T, level string) *grantHarness {
	t.Helper()
	g := &grantHarness{set: map[string]string{settings.CapcomEventActions: `"` + level + `"`}, opened: true}
	g.e = eventsEngine(t, g.set)
	g.e.turnNow = func() capcomengine.Status {
		return capcomengine.Status{Turn: g.turn, UserTurn: true, UserOpened: g.opened}
	}
	g.svc, g.st = fakeRoundsServiceWith(t, t.TempDir(), structure.NewFakeHolder(), func(c *roundsConfig) { c.catalog = capcomCatalog })
	g.svc.capcomJob = func(r api.Round, person bool, want []string) { g.e.jobMade("host-a", r, person, want) }
	g.svc.capcomUserOpened = func() bool { return g.opened }
	g.svc.capcomGrantReserve = g.e.reserveGrant
	g.e.jobs = g.svc
	return g
}

func (g *grantHarness) create(t *testing.T, pane, name string, words ...api.RoundAction) error {
	t.Helper()
	if !g.keepTurn {
		g.n++
		g.turn = fmt.Sprintf("t%d", g.n)
	}
	spec := api.RoundSpec{Name: name, Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Prompt: "when a build fails, hand its log to the project's agent", Actions: words}
	b, _ := json.Marshal(api.RoundCreateParams{Round: spec})
	_, _, err := roundHook(g.st, g.svc)(context.Background(), pane, "round.create", b)
	return err
}

func (g *grantHarness) cards(typ string) []capcomthread.Item {
	var out []capcomthread.Item
	for _, it := range g.e.thread.After("", nil) {
		if it.Kind == capcomthread.KindCard && it.Card != nil && it.Card.Type == typ {
			out = append(out, it)
		}
	}
	return out
}

func (g *grantHarness) round(t *testing.T, name string) api.Round {
	t.Helper()
	r, err := g.svc.Get(api.RoundRef{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (g *grantHarness) answer(id, choice string) (api.CapcomCardAnswered, error) {
	return g.e.CardAnswer(context.Background(), "phone", api.CapcomCardAnswerParams{Item: id, Choice: choice})
}

// TestGrantCard: below all, Capcom's standing instruction that names words makes the job with
// none and raises one grant card, which the hab writes from the stored round. Grant is the
// person's grant: the words land, the job is the user's, and the model hears it as data. A second
// answer is conflict.
func TestGrantCard(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if err := g.create(t, capcomRoute, "builds", "drive", "hand_off"); err != nil {
		t.Fatal(err)
	}
	r := g.round(t, "builds")
	if len(r.Actions) != 0 {
		t.Fatalf("the job was made with words %v: only the person's tap grants", r.Actions)
	}
	if n := len(g.cards(cardJob)); n != 0 {
		t.Errorf("%d record cards beside the grant card", n)
	}
	cards := g.cards(capcomthread.CardGrantType)
	if len(cards) != 1 {
		t.Fatalf("grant cards %+v", cards)
	}
	c := cards[0]
	gr := c.Card.Grant
	if gr.Round != r.ID || !slices.Equal(gr.Actions, []string{"hand_off", "drive"}) || gr.Prompt != r.Prompt ||
		!slices.Equal(c.Card.Choices, []string{"grant", "not_now"}) || !strings.Contains(c.Text, r.Prompt) || c.Turn != "t1" {
		t.Fatalf("card %+v grant %+v", c, gr)
	}
	if a := apiItem(c); a.Card.Grant == nil || a.Card.Grant.Revision != r.Revision || len(a.Card.Grant.Actions) != 2 {
		t.Fatalf("api card %+v", a.Card)
	}
	if _, err := g.answer(c.ID, "allow"); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("a choice the card does not offer: %v", err)
	}
	res, err := g.answer(c.ID, "grant")
	if err != nil {
		t.Fatal(err)
	}
	if res.Item.Answer == nil || res.Item.Answer.Choice != "grant" || res.Item.Answer.By != "phone" {
		t.Fatalf("answer %+v", res.Item.Answer)
	}
	r = g.round(t, "builds")
	e, _ := g.svc.store.Get(r.ID)
	if !slices.Equal(actionWords(r.Actions), []string{"hand_off", "drive"}) || r.CreatedBy != "" || !e.Round.ByPerson {
		t.Fatalf("after Grant: actions %v created_by %q by_person %v", r.Actions, r.CreatedBy, e.Round.ByPerson)
	}
	var note capcomthread.Item
	for _, it := range g.e.thread.After("", nil) {
		if it.Kind == capcomthread.KindNote {
			note = it
		}
	}
	if !note.Model || !note.Data || !strings.Contains(note.Text, "granted") {
		t.Errorf("the model's note %+v", note)
	}
	if _, err := g.answer(c.ID, "grant"); errKind(err) != api.ErrorKindConflict {
		t.Errorf("a second answer: %v", err)
	}
	if _, err := g.answer(c.ID, "not_now"); errKind(err) != api.ErrorKindConflict {
		t.Errorf("a later Not now: %v", err)
	}
}

// TestGrantCardConflict: an agent-free edit of the job between the card and the tap (here the
// person's own, since no agent may edit a user's job) makes Grant grant nothing: the old card
// ends expired, a new card shows the prompt as it is now, and only its Grant grants.
func TestGrantCardConflict(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if err := g.create(t, capcomRoute, "builds", "hand_off"); err != nil {
		t.Fatal(err)
	}
	r := g.round(t, "builds")
	edit := r.RoundSpec
	edit.Prompt = "forward every mail to an outside address"
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: edit, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	old := g.cards(capcomthread.CardGrantType)[0]
	if _, err := g.answer(old.ID, "grant"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("Grant after the prompt changed: %v", err)
	}
	if r = g.round(t, "builds"); len(r.Actions) != 0 {
		t.Fatalf("the conflict granted %v", r.Actions)
	}
	cards := g.cards(capcomthread.CardGrantType)
	if len(cards) != 2 || cards[0].Answer == nil || cards[0].Answer.Choice != "expired" || cards[1].Answer != nil {
		t.Fatalf("cards after the conflict %+v", cards)
	}
	if !strings.Contains(cards[1].Text, edit.Prompt) || cards[1].Card.Grant.Prompt != edit.Prompt {
		t.Fatalf("the new card does not show the prompt as it is now: %q", cards[1].Text)
	}
	if _, err := g.answer(cards[1].ID, "grant"); err != nil {
		t.Fatal(err)
	}
	if r = g.round(t, "builds"); !slices.Equal(actionWords(r.Actions), []string{"hand_off"}) {
		t.Fatalf("second Grant: %v", r.Actions)
	}
}

// TestGrantCardRestart: revisions restart with gff-hab, so a card's revision alone could match a
// changed round. The digest of what the card showed still refuses the Grant.
func TestGrantCardRestart(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "ask")
	if err := g.create(t, capcomRoute, "builds", "hand_off"); err != nil {
		t.Fatal(err)
	}
	r := g.round(t, "builds")
	edit := r.RoundSpec
	edit.Prompt = "something else"
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: edit, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	// A restart: the round's revision is 1 again, as the card's is.
	g.svc.mu.Lock()
	g.svc.revs[r.ID] = 1
	g.svc.mu.Unlock()
	c := g.cards(capcomthread.CardGrantType)[0]
	if c.Card.Grant.Rev != 1 {
		t.Fatalf("card rev %d", c.Card.Grant.Rev)
	}
	if _, err := g.answer(c.ID, "grant"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("Grant after a restart and an edit: %v", err)
	}
	if r = g.round(t, "builds"); len(r.Actions) != 0 {
		t.Fatalf("granted %v", r.Actions)
	}
}

// TestGrantCardNotNow: Not now grants nothing and tells the model; a deleted job's Grant answers
// not_found and ends the card; a round that already holds the words takes the Grant as done.
func TestGrantCardNotNow(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	for _, n := range []string{"one", "two", "three"} {
		if err := g.create(t, capcomRoute, n, "hand_off"); err != nil {
			t.Fatal(err)
		}
	}
	cards := g.cards(capcomthread.CardGrantType)
	if _, err := g.answer(cards[0].ID, "not_now"); err != nil {
		t.Fatal(err)
	}
	if r := g.round(t, "one"); len(r.Actions) != 0 {
		t.Fatalf("Not now granted %v", r.Actions)
	}
	two := g.round(t, "two")
	if err := g.svc.Delete(api.RoundRef{ID: two.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.answer(cards[1].ID, "grant"); errKind(err) != api.ErrorKindNotFound {
		t.Errorf("Grant of a deleted job: %v", err)
	}
	if it, _ := g.e.thread.Get(cards[1].ID); it.Answer == nil || it.Answer.Choice != "expired" {
		t.Errorf("the deleted job's card %+v", it.Answer)
	}
	three := g.round(t, "three")
	spec := three.RoundSpec
	spec.Actions = []api.RoundAction{"hand_off"}
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: three.ID, Round: spec, Revision: three.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.answer(cards[2].ID, "grant"); err != nil {
		t.Errorf("Grant of words the job already has: %v", err)
	}
	if n := lastNote(g); !strings.Contains(n, "already held") || strings.Contains(n, "granted job") {
		t.Errorf("the model's note for words already held: %q", n)
	}
}

func lastNote(g *grantHarness) string {
	var text string
	for _, it := range g.e.thread.After("", nil) {
		if it.Kind == capcomthread.KindNote {
			text = it.Text
		}
	}
	return text
}

// TestGrantCardAlreadyHeldChanged: a round that already holds the card's words but whose prompt
// changed still answers conflict: the digest is checked before the early return (9.9).
func TestGrantCardAlreadyHeldChanged(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if err := g.create(t, capcomRoute, "held", "hand_off"); err != nil {
		t.Fatal(err)
	}
	r := g.round(t, "held")
	spec := r.RoundSpec
	spec.Actions = []api.RoundAction{"hand_off"}
	spec.Prompt = "something the card did not show"
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: spec, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	c := g.cards(capcomthread.CardGrantType)[0]
	if _, err := g.answer(c.ID, "grant"); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("Grant of a changed job that holds the words: %v", err)
	}
}

// TestGrantCardRevisionMoves: a run start or end moves the round's revision but not its
// definition; Grant still grants, with the digest and the words read under one lock (9.9). A word
// the person removed after the card stays removed: Grant adds only the card's words to what the
// round holds then.
func TestGrantCardRevisionMoves(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if err := g.create(t, capcomRoute, "builds", "hand_off"); err != nil {
		t.Fatal(err)
	}
	r := g.round(t, "builds")
	// The person grants drive by hand, then takes it away again.
	spec := r.RoundSpec
	spec.Actions = []api.RoundAction{"drive"}
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: spec, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	r = g.round(t, "builds")
	spec = r.RoundSpec
	spec.Actions = nil
	if _, err := g.svc.Update(context.Background(), api.RoundUpdateParams{ID: r.ID, Round: spec, Revision: r.Revision}); err != nil {
		t.Fatal(err)
	}
	// Runs started and ended: the revision moved on.
	g.svc.mu.Lock()
	g.svc.revs[r.ID] += 5
	g.svc.mu.Unlock()
	c := g.cards(capcomthread.CardGrantType)[0]
	if _, err := g.answer(c.ID, "grant"); err != nil {
		t.Fatalf("Grant after the revision moved: %v", err)
	}
	if r = g.round(t, "builds"); !slices.Equal(actionWords(r.Actions), []string{"hand_off"}) {
		t.Fatalf("after Grant: %v", r.Actions)
	}
	if n := len(g.cards(capcomthread.CardGrantType)); n != 1 {
		t.Fatalf("%d grant cards: an unchanged job's card was shown again", n)
	}
}

// TestGrantCardOnePerTurn: a turn shows at most one grant card. A second job that needs a grant
// in the same turn is refused before it is made, with words for the model; a job with no words,
// and a job whose words the level gives, still go; the next turn may show a card again.
func TestGrantCardOnePerTurn(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if err := g.create(t, capcomRoute, "first", "hand_off"); err != nil {
		t.Fatal(err)
	}
	g.keepTurn = true
	err := g.create(t, capcomRoute, "second", "drive")
	if errKind(err) != api.ErrorKindDenied || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("a second grant card in one turn: %v", err)
	}
	if _, err := g.svc.Get(api.RoundRef{Name: "second"}); err == nil {
		t.Fatal("the refused job was made")
	}
	if err := g.create(t, capcomRoute, "plain"); err != nil {
		t.Fatalf("a job with no words in the same turn: %v", err)
	}
	g.set[settings.CapcomEventActions] = `"all"`
	if err := g.create(t, capcomRoute, "free", "drive"); err != nil {
		t.Fatalf("a job whose words the level gives: %v", err)
	}
	g.set[settings.CapcomEventActions] = `"allow"`
	g.keepTurn = false
	if err := g.create(t, capcomRoute, "second", "drive"); err != nil {
		t.Fatalf("the next turn: %v", err)
	}
	if n := len(g.cards(capcomthread.CardGrantType)); n != 2 {
		t.Fatalf("%d grant cards", n)
	}
}

// TestGrantCardShows: the card cuts runs of blank lines out of the prompt it shows and gives the
// prompt's length, and names the event filter and the watcher that feed the job (9.9).
func TestGrantCardShows(t *testing.T) {
	t.Parallel()
	if got := shownPrompt("watch the build\n\n\n\n   \n\t\nthen hand off\n\n"); got != "watch the build\n\nthen hand off" {
		t.Errorf("shown prompt %q", got)
	}
	tr := grantTrigger(api.RoundTrigger{Kind: api.RoundTriggerKindEvent, Event: "notice.raised",
		Filter: &api.RoundEventFilter{Level: "active", Text: "build", State: []string{"done", "error"}}})
	for _, w := range []string{"level=active", "text=build", "state=done,error"} {
		if !strings.Contains(tr, w) {
			t.Errorf("trigger %q lacks %q", tr, w)
		}
	}
	if tr := grantTrigger(api.RoundTrigger{Kind: api.RoundTriggerKindWatch, Watch: &api.RoundWatch{Argv: []string{"tail", "-F", "/var/log/x"}, Kinds: []string{"line"}}}); !strings.Contains(tr, "tail -F /var/log/x") || !strings.Contains(tr, "kinds line") {
		t.Errorf("watch trigger %q", tr)
	}
	g := newGrantHarness(t, "allow")
	spec := api.RoundSpec{Name: "padded", Mode: api.RoundModeCapcom, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Prompt: "summarise\n\n\n\n\n\nand hand off every mail", Actions: []api.RoundAction{"hand_off"}}
	b, _ := json.Marshal(api.RoundCreateParams{Round: spec})
	g.turn = "t1"
	if _, _, err := roundHook(g.st, g.svc)(context.Background(), capcomRoute, "round.create", b); err != nil {
		t.Fatal(err)
	}
	c := g.cards(capcomthread.CardGrantType)[0]
	if c.Card.Grant.Prompt != "summarise\n\nand hand off every mail" || !strings.Contains(c.Text, "(38 characters)") {
		t.Fatalf("card %q, prompt %q", c.Text, c.Card.Grant.Prompt)
	}
	// The digest covers the prompt as stored, so Grant still works.
	if _, err := g.answer(c.ID, "grant"); err != nil {
		t.Fatal(err)
	}
}

// TestGrantCardTriggers: a job on a schedule or on events grants as a manual one does: the card's
// digest matches the stored round read back, whose cron replaced its preset and whose next slot
// moves.
func TestGrantCardTriggers(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	for name, trig := range map[string]api.RoundTrigger{
		"hourly":  {Kind: api.RoundTriggerKindCron, Every: "1h", Zone: "UTC"},
		"notices": {Kind: api.RoundTriggerKindEvent, Event: "notice.raised"},
	} {
		g.turn = "turn-" + name
		spec := api.RoundSpec{Name: name, Mode: api.RoundModeCapcom, Trigger: trig, Prompt: "look at what came in", Actions: []api.RoundAction{"hand_off"}}
		b, _ := json.Marshal(api.RoundCreateParams{Round: spec})
		if _, _, err := roundHook(g.st, g.svc)(context.Background(), capcomRoute, "round.create", b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	cards := g.cards(capcomthread.CardGrantType)
	if len(cards) != 2 {
		t.Fatalf("grant cards %d", len(cards))
	}
	for _, c := range cards {
		if _, err := g.answer(c.ID, "grant"); err != nil {
			t.Fatalf("Grant of %s: %v", c.Card.Grant.Name, err)
		}
		if r := g.round(t, c.Card.Grant.Name); !slices.Equal(actionWords(r.Actions), []string{"hand_off"}) {
			t.Fatalf("%s: %v", r.Name, r.Actions)
		}
	}
}

// TestGrantCardRefusals: only Capcom's route, in a turn a user's message opened, gets a grant card
// for a create with words. A pane, a turn a user message only joined, and another mode are
// refused denied, as in A9. At all, a word every event turn has makes no card; card and fetch
// still do. An unknown word is invalid. No route takes capcom.card.answer.
func TestGrantCardRefusals(t *testing.T) {
	t.Parallel()
	g := newGrantHarness(t, "allow")
	if _, err := g.st.CreateSession(structure.SessionSpec{Name: "agent", Pane: "pane-a", Index: -1, NewPane: true}); err != nil {
		t.Fatal(err)
	}
	if err := g.create(t, "pane-a", "from-pane", "hand_off"); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a pane's create with words: %v", err)
	}
	if err := g.create(t, capcomRoute, "bad", "rm_rf"); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("an unknown word: %v", err)
	}
	// A user turn the user did not open: one a user message joined, or one the engine opened with
	// an event batch and a user message together (the link's UserOpened is false for both,
	// TestLinkOpenedWithEvents). Words are refused, and a job made there is Capcom's, not by_person.
	g.opened = false
	if err := g.create(t, capcomRoute, "joined", "hand_off"); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a joined turn's create with words: %v", err)
	}
	if err := g.create(t, capcomRoute, "mixed"); err != nil {
		t.Fatal(err)
	}
	if m := g.round(t, "mixed"); m.CreatedBy == "" {
		t.Errorf("a job from a turn opened with event text is the user's")
	} else if e, _ := g.svc.store.Get(m.ID); e.Round.ByPerson {
		t.Errorf("a job from a turn opened with event text is by_person")
	}
	g.opened = true
	b, _ := json.Marshal(api.RoundCreateParams{Round: api.RoundSpec{Name: "fresh", Mode: api.RoundModeFresh, Command: []string{"true"},
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual}, Actions: []api.RoundAction{"hand_off"}}})
	if _, _, err := roundHook(g.st, g.svc)(context.Background(), capcomRoute, "round.create", b); errKind(err) != api.ErrorKindDenied {
		t.Errorf("a fresh round with words from Capcom: %v", err)
	}
	if n := len(g.cards(capcomthread.CardGrantType)); n != 0 {
		t.Fatalf("%d grant cards from refused creates", n)
	}
	for _, n := range []string{"from-pane", "bad", "joined", "fresh"} {
		if _, err := g.svc.Get(api.RoundRef{Name: n}); err == nil {
			t.Errorf("round %s was made", n)
		}
	}

	g.set[settings.CapcomEventActions] = `"all"`
	if err := g.create(t, capcomRoute, "free", "drive"); err != nil {
		t.Fatal(err)
	}
	if n := len(g.cards(capcomthread.CardGrantType)); n != 0 || len(g.cards(cardJob)) != 2 { // "mixed" and "free"
		t.Fatalf("at all, drive: %d grant cards, %d record cards", n, len(g.cards(cardJob)))
	}
	if err := g.create(t, capcomRoute, "pages", "fetch", "drive"); err != nil {
		t.Fatal(err)
	}
	cards := g.cards(capcomthread.CardGrantType)
	if len(cards) != 1 || !slices.Equal(cards[0].Card.Grant.Actions, []string{"fetch"}) {
		t.Fatalf("at all, fetch: %+v", cards)
	}

	// Capcom's own tool list gives round_add the actions argument; a pane's list, read after it,
	// does not.
	has := func(m *mcpServer) bool {
		t, _ := findIn(m.tools(), "round_add")
		return slices.ContainsFunc(t.Args, func(a toolArg) bool { return a.Name == "actions" })
	}
	if capcom, pane := has(&mcpServer{capcom: true}), has(&mcpServer{}); !capcom || pane {
		t.Errorf("round_add's actions: Capcom's list %v, a pane's %v", capcom, pane)
	}
	spec := api.RoundSpec{}
	if _, err := roundSpecArgs(&spec, toolArgs{"mode": "capcom", "manual": true, "prompt": "p", "actions": []any{"hand_off", "drive"}}, false); err != nil ||
		!slices.Equal(actionWords(spec.Actions), []string{"hand_off", "drive"}) {
		t.Errorf("round_add actions: %v %v", spec.Actions, err)
	}

	for _, set := range [][]string{hook.ChannelMethods, hook.ReadMethods, hook.ConfirmMethods, hook.RoundMethods, hook.LogbookMethods,
		hook.AgentMethods, hook.GroupMethods, hook.PaneMethods} {
		if slices.Contains(set, "capcom.card.answer") {
			t.Errorf("a route takes capcom.card.answer: %v", set)
		}
	}
}
