package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/control"
)

// lbReply reads the stub's "lb: <status> <json>" reply as the write's answer.
func lbReply(t *testing.T, text string) api.LogbookWritten {
	t.Helper()
	_, js, ok := strings.Cut(strings.TrimPrefix(text, "lb: "), " ")
	var w struct {
		Result api.LogbookWritten `json:"result"`
	}
	if !ok || json.Unmarshal([]byte(js), &w) != nil || w.Result.State == "" {
		t.Fatalf("not a write answer: %q", text)
	}
	return w.Result
}

// sayWait sends a message and returns the stub's reply to it.
func (e *serviceEnv) sayWait(t *testing.T, c *control.Client, text string) string {
	t.Helper()
	run(t, "capcom", "say", "-socket", e.sock, text)
	var reply string
	waitUntil(t, "the reply to "+text, func() bool {
		items := threadText(t, c)
		for i, it := range items {
			if it.Author.Kind == "user" && it.Text == text {
				for j := i - 1; j >= 0; j-- {
					if items[j].Kind == "message" && items[j].Author.Kind == "capcom" {
						reply = items[j].Text
						return true
					}
				}
			}
		}
		return false
	})
	return reply
}

// lastItem returns the newest thread item that matches keep.
func lastItem(t *testing.T, c *control.Client, keep func(api.CapcomItem) bool) (api.CapcomItem, bool) {
	t.Helper()
	for _, it := range threadText(t, c) {
		if keep(it) {
			return it, true
		}
	}
	return api.CapcomItem{}, false
}

func noteWith(prefix string) func(api.CapcomItem) bool {
	return func(it api.CapcomItem) bool { return it.Kind == "note" && strings.HasPrefix(it.Text, prefix) }
}

func dividersOf(items []api.CapcomItem) []api.CapcomItem {
	var out []api.CapcomItem
	for _, it := range items {
		if it.Kind == "divider" {
			out = append(out, it)
		}
	}
	return out
}

// The Capcom context manager and memory end to end with the stub engine (capcom-service.md 6,
// 18 A4): a capcom guide lands at once with a "Noted" line whose Undo deletes it, and an Undo
// after an edit elsewhere says so; an event turn's guide lands too; a guide with no capcom tag
// and a task stay proposals with no line; at Safe the guide is a proposal, "Noted, waiting for
// review", whose Undo rejects it; a turn past reseed_at re-seeds once with one divider holding
// the summary, and the seed keeps seed_messages, seed_messages_chars and seed_logbook_bytes; a
// compaction adds a divider; "New conversation" adds one divider with an empty summary and
// counts the proposals that wait.
func TestCapcomServiceContext(t *testing.T) {
	e := newServiceEnv(t, "reseed_at = 50\nseed_messages = 2\nseed_messages_chars = 60\nseed_logbook_bytes = 120\n")
	e.serve()
	c := e.control()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the engine connected", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})

	// A capcom guide lands at once, with a Noted line and Undo.
	w := lbReply(t, e.sayWait(t, c, "remember The user is vegetarian"))
	if w.State != api.LogbookWriteStateApplied || len(w.IDs) != 1 {
		t.Fatalf("the capcom guide: %+v", w)
	}
	veg := w.IDs[0]
	if r, ok := lbGet(t, c, veg); !ok || r.Type != "guide" || r.CreatedBy == nil || r.CreatedBy.Session != st.Session {
		t.Fatalf("record %+v", r)
	}
	noted, ok := lastItem(t, c, noteWith("Noted: The user is vegetarian"))
	if !ok || noted.Card == nil || noted.Card.Type != "memory" || noted.Card.Ref != veg || strings.Join(noted.Card.Choices, ",") != "undo" {
		t.Fatalf("noted line %+v", noted)
	}
	res, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: noted.ID, Choice: "undo"})
	if err != nil || res.Item.Answer == nil || res.Note != "" {
		t.Fatalf("undo: %+v %v", res, err)
	}
	if r, ok := lbGet(t, c, veg); ok && !r.Deleted {
		t.Fatalf("the undone guide is still there: %+v", r)
	}
	if _, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: noted.ID, Choice: "undo"}); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second undo: %v", err)
	}

	// Undo after an edit elsewhere deletes the edited record and says so.
	w = lbReply(t, e.sayWait(t, c, "remember Likes window seats"))
	seat := w.IDs[0]
	noted, _ = lastItem(t, c, noteWith("Noted: Likes window seats"))
	call(t, c, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpSet, ID: seat, Field: "title", Value: api.RawJSON(`"Likes aisle seats"`)}}}, nil)
	res, err = callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: noted.ID, Choice: "undo"})
	if err != nil || !strings.Contains(res.Note, "edited") || !strings.Contains(res.Note, "Likes aisle seats") {
		t.Fatalf("undo after an edit: %+v %v", res, err)
	}
	if r, ok := lbGet(t, c, seat); ok && !r.Deleted {
		t.Fatal("the edited guide is still there")
	}

	// An event turn's guide lands at once too; Capcom's own edit of it lands.
	w = lbReply(t, e.sayWait(t, c, "event:remember Lives in Lisbon"))
	if w.State != api.LogbookWriteStateApplied {
		t.Fatalf("the event turn's guide: %+v", w)
	}
	home := w.IDs[0]
	if _, ok := lastItem(t, c, noteWith("Noted: Lives in Lisbon")); !ok {
		t.Fatal("no Noted line for the event turn's guide")
	}
	if w := lbReply(t, e.sayWait(t, c, "edit "+home+" Lives in Porto")); w.State != api.LogbookWriteStateApplied {
		t.Fatalf("an edit of a capcom guide: %+v", w)
	}

	// A guide with no capcom tag, and a task, stay proposals with no Noted line.
	countNoted := func() int {
		n := 0
		for _, it := range threadText(t, c) {
			if noteWith("Noted")(it) {
				n++
			}
		}
		return n
	}
	before := countNoted()
	if w := lbReply(t, e.sayWait(t, c, "guide Some project rule")); w.State != api.LogbookWriteStateProposed {
		t.Fatalf("an untagged guide: %+v", w)
	}
	if w := lbReply(t, e.sayWait(t, c, "lbwrite")); w.State != api.LogbookWriteStateProposed {
		t.Fatalf("a task: %+v", w)
	}
	if n := countNoted(); n != before {
		t.Fatalf("%d Noted lines for proposals", n-before)
	}

	// At Safe the guide is a proposal; Undo rejects it.
	setApproval(t, c, "agents")
	w = lbReply(t, e.sayWait(t, c, "remember Allergic to nuts"))
	if w.State != api.LogbookWriteStateProposed || w.Proposal == "" {
		t.Fatalf("the guide at Safe: %+v", w)
	}
	waiting, ok := lastItem(t, c, noteWith("Noted, waiting for review: Allergic to nuts"))
	if !ok || waiting.Card == nil || waiting.Card.Ref != w.Proposal {
		t.Fatalf("waiting line %+v", waiting)
	}
	if _, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: waiting.ID, Choice: "undo"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := lbGet(t, c, w.Proposal); r.Status != "rejected" {
		t.Fatalf("the proposal after undo: %+v", r)
	}
	setApproval(t, c, "off")

	// A turn past reseed_at re-seeds once, with one divider that holds the summary.
	e.sayWait(t, c, "a message that is long enough to push the oldest one out of the seed")
	e.sayWait(t, c, "ctx 0.6")
	waitUntil(t, "the re-seed", func() bool { return len(dividersOf(threadText(t, c))) == 1 })
	d := dividersOf(threadText(t, c))[0]
	if d.Text != "Earlier messages summarised" || !strings.HasPrefix(d.Summary, "stub summary of ") {
		t.Fatalf("divider %+v", d)
	}
	if n := strings.Count(e.stubLog(), "reseed limit"); n != 1 {
		t.Fatalf("%d re-seeds", n)
	}
	var cx capcomengine.Message
	files, _ := filepath.Glob(filepath.Join(e.stubDir, "context-*.json"))
	if len(files) != 1 {
		t.Fatalf("context files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if err := json.Unmarshal(b, &cx); err != nil || cx.Seed == nil {
		t.Fatalf("context %s: %v", b, err)
	}
	chars := 0
	for _, m := range cx.Seed.Messages {
		chars += utf8.RuneCountInString(m.Text)
	}
	if len(cx.Seed.Messages) == 0 || len(cx.Seed.Messages) > 2 || chars > 60 {
		t.Fatalf("seed messages %d, %d characters: %+v", len(cx.Seed.Messages), chars, cx.Seed.Messages)
	}
	if len(cx.Seed.Logbook) > 120 || !strings.Contains(cx.Seed.Logbook, "Lives in Porto") {
		t.Fatalf("logbook part (%d bytes) %q", len(cx.Seed.Logbook), cx.Seed.Logbook)
	}
	if strings.Contains(cx.Seed.Logbook, "vegetarian") {
		t.Fatal("an undone guide is in the seed")
	}
	// The next turns run in the fresh session; a second turn past 50% re-seeds again only then.
	e.sayWait(t, c, "after the re-seed")

	// A compaction adds a divider with the summary at that moment.
	e.sayWait(t, c, "compact")
	waitUntil(t, "the compaction divider", func() bool { return len(dividersOf(threadText(t, c))) == 2 })

	// New conversation: one divider, an empty summary, the proposals counted.
	opened, err := callJSON[api.CapcomOpened](t, c, "capcom.open", api.CapcomOpenParams{New: true})
	if err != nil || opened.Proposals < 2 {
		t.Fatalf("capcom.open new: %+v %v", opened, err)
	}
	waitUntil(t, "the new conversation divider", func() bool { return len(dividersOf(threadText(t, c))) == 3 })
	nd := dividersOf(threadText(t, c))[0]
	if nd.Text != "New conversation" || nd.Summary != "" {
		t.Fatalf("new conversation divider %+v", nd)
	}
	rec, _ := capcomengine.OpenStore(filepath.Join(e.state, "capcom"))
	if r := rec.Get(); r.Summary != "" || r.Sessions[len(r.Sessions)-1].Why != "new" {
		t.Fatalf("record after new conversation: %+v", r)
	}
}
