package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Capcom's memory in Logbook (capcom-service.md 6.4): the seed's Logbook part, the capcom
// guides that land at once with a "Noted" line, and that line's Undo.

// capcomTag is the tag of Capcom's facts and preferences about the user.
const capcomTag = "capcom"

// capcomMemory is the Logbook half of the Capcom service.
type capcomMemory struct {
	svc  *lbsvc.Service
	host string
	mu   sync.Mutex // one card answer at a time
}

// isCapcomGuide reports whether a record is a capcom guide: a guide with no project and the
// tag capcom.
func isCapcomGuide(r api.LogbookRecord) bool {
	return r.Type == logbook.TypeGuide && r.Project == "" && !r.Deleted && slices.Contains(r.Tags, capcomTag)
}

// guideWrite reports whether every change of a write creates or edits a capcom guide (6.4):
// a create of a guide with no project and the one tag capcom, or a title, body, status or note
// change on such a guide. A tag change is reviewed: it changes who reads the guide. Anything else, in any change,
// leaves the write a proposal.
func (m *capcomMemory) guideWrite(ctx context.Context, changes []api.LogbookChange) bool {
	if m == nil || m.svc == nil || len(changes) == 0 {
		return false
	}
	made := map[string]bool{}
	known := map[string]bool{}
	guide := func(id string) bool {
		if id == "" {
			return false
		}
		if made[id] {
			return true
		}
		if ok, seen := known[id]; seen {
			return ok
		}
		r, err := m.svc.Get(ctx, api.LogbookGetParams{ID: id})
		ok := err == nil && isCapcomGuide(r.Record)
		known[id] = ok
		return ok
	}
	for _, c := range changes {
		switch c.Op {
		case api.LogbookChangeOpCreate:
			// Only the tag capcom: another tag, such as everywhere, would put the guide in other
			// agents' briefs, which no unreviewed write may do.
			if c.Type != logbook.TypeGuide || c.Project != "" || !slices.Equal(c.Tags, []string{capcomTag}) || len(c.Links) > 0 {
				return false
			}
			if c.ID != "" {
				made[c.ID] = true
			}
		case api.LogbookChangeOpSet:
			if (c.Field != "title" && c.Field != "body" && c.Field != "status") || !guide(c.ID) {
				return false
			}
		case api.LogbookChangeOpNote:
			if !guide(c.ID) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// noted adds the thread's "Noted" line for a capcom guide write (6.4): one per guide it
// touched once it landed, or one for the proposal that holds it at Safe. A repeated write adds
// no second line.
func (e *capcomEngine) noted(ctx context.Context, p api.LogbookWriteParams, res api.LogbookWritten) {
	m := e.memory
	if m == nil || m.svc == nil || len(res.Ops) == 0 {
		return
	}
	flightdeck := capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}
	if res.State == api.LogbookWriteStateProposed && res.Proposal != "" {
		title := ""
		for _, c := range p.Changes {
			if c.Op == api.LogbookChangeOpCreate && c.Title != "" {
				title = c.Title
				break
			}
		}
		if title == "" && len(p.Changes) > 0 {
			if r, err := m.svc.Get(ctx, api.LogbookGetParams{ID: p.Changes[0].ID}); err == nil {
				title = r.Record.Title
			}
		}
		e.addNote(capcomthread.Item{Kind: capcomthread.KindNote, Author: flightdeck, Key: "memory:" + res.Proposal,
			Text: notedLine("Noted, waiting for review", title),
			Card: &capcomthread.Card{Type: capcomthread.CardMemory, Ref: res.Proposal, Choices: []string{capcomthread.ChoiceUndo}, Proposal: true}})
		return
	}
	if res.State != api.LogbookWriteStateApplied {
		return
	}
	// Undo is for a guide this write created (6.4); an edit's line has none, so Undo never deletes
	// a guide the user had before.
	created := map[string]bool{}
	for i, c := range p.Changes {
		if c.Op == api.LogbookChangeOpCreate && i < len(res.IDs) {
			created[res.IDs[i]] = true
		}
	}
	seen := map[string]bool{}
	for _, id := range res.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		r, err := m.svc.Get(ctx, api.LogbookGetParams{ID: id})
		if err != nil || !isCapcomGuide(r.Record) {
			continue
		}
		it := capcomthread.Item{Kind: capcomthread.KindNote, Author: flightdeck, Key: "memory:" + res.Ops[0] + ":" + id,
			Text: notedLine("Noted", r.Record.Title)}
		if created[id] {
			it.Card = &capcomthread.Card{Type: capcomthread.CardMemory, Ref: id, Choices: []string{capcomthread.ChoiceUndo}, Rev: r.Record.Revision}
		}
		e.addNote(it)
	}
}

func notedLine(lead, title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return lead
	}
	return lead + ": " + title
}

func (e *capcomEngine) addNote(it capcomthread.Item) {
	if _, _, err := e.thread.Add(it); err != nil {
		e.logf("capcom: store a noted line: %v", err)
	}
}

// CardAnswer serves capcom.card.answer (7.4). A memory line's undo deletes the guide, or rejects
// its proposal; the user's device is the person who decides. The card keeps the first answer.
func (e *capcomEngine) CardAnswer(ctx context.Context, client string, p api.CapcomCardAnswerParams) (api.CapcomCardAnswered, error) {
	it, ok := e.thread.Get(p.Item)
	if !ok || it.Card == nil {
		return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindNotFound, "no card %s in the Capcom thread", p.Item)
	}
	if !slices.Contains(it.Card.Choices, p.Choice) {
		return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindInvalid, "card %s offers %s, not %q", p.Item, strings.Join(it.Card.Choices, ", "), p.Choice)
	}
	if it.Card.Type != capcomthread.CardMemory {
		// Calendar and reminder cards come with A5; approvals answer through approval.answer.
		return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindInvalid, "a %s card is not answered with capcom.card.answer", it.Card.Type)
	}
	m := e.memory
	if m == nil || m.svc == nil {
		return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindUnavailable, "this hab has no Logbook")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, _ = e.thread.Get(p.Item); it.Answer != nil {
		return api.CapcomCardAnswered{Item: apiItem(it)}, control.Errorf(api.ErrorKindConflict, "card %s was answered already (%s)", p.Item, it.Answer.Choice)
	}
	person := api.LogbookActor{Kind: "person", Device: client}
	note := ""
	if it.Card.Proposal {
		_, err := m.svc.Decide(ctx, person, api.LogbookDecideParams{ID: it.Card.Ref, Decision: api.LogbookDecisionReject,
			IdempotencyKey: "capcom-undo:" + it.ID})
		if err != nil {
			return api.CapcomCardAnswered{}, err
		}
	} else {
		r, err := m.svc.Get(ctx, api.LogbookGetParams{ID: it.Card.Ref})
		var ce *control.Error
		switch {
		case errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound:
			return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindNotFound, "the record %s is gone: nothing to undo", it.Card.Ref)
		case err != nil:
			return api.CapcomCardAnswered{}, err
		}
		// An undo whose delete landed but whose answer did not is retried: the write below, under
		// the same key, applies nothing again, and the card gets its answer.
		if r.Record.Revision != it.Card.Rev && !r.Record.Deleted {
			note = fmt.Sprintf("Deleted %q; it was edited after Capcom noted it", r.Record.Title)
		}
		if _, err := m.svc.Write(ctx, person, api.LogbookWriteParams{IdempotencyKey: "capcom-undo:" + it.ID,
			Changes: []api.LogbookChange{{Op: api.LogbookChangeOpSet, ID: it.Card.Ref, Field: "deleted", Value: api.RawJSON("true")}}}); err != nil {
			return api.CapcomCardAnswered{}, err
		}
	}
	done, err := e.thread.AnswerCard(it.ID, p.Choice, client)
	if err != nil && !errors.Is(err, capcomthread.ErrConflict) {
		return api.CapcomCardAnswered{}, err
	}
	if note != "" {
		e.addNote(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: note})
	}
	return api.CapcomCardAnswered{Item: apiItem(done), Note: note}, nil
}

// pendingProposals counts the service session's Logbook proposals that wait for review: the
// review list a new conversation opens (6.4).
func (e *capcomEngine) pendingProposals(ctx context.Context) int {
	m, session := e.memory, e.Session()
	if m == nil || m.svc == nil || session == "" {
		return 0
	}
	res, err := m.svc.List(ctx, api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: logbook.TypeProposal,
		Status: "pending", Session: session, All: true, Limit: 500}})
	if err != nil {
		return 0
	}
	return len(res.Records)
}

// logbookPart builds the seed's Logbook part (6.4) as the Capcom session reads Logbook, in at
// most max bytes, whole lines only: the capcom guides, then the general brief of the records
// tagged everywhere, then the open tasks Capcom filed.
func (e *capcomEngine) logbookPart(max int) string {
	m, session := e.memory, e.Session()
	if m == nil || m.svc == nil || max <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pn := lbsvc.Pane{ID: capcomRoute, Session: session, Host: m.host, Propose: true}
	call := func(method string, params any, out any) bool {
		b, _ := json.Marshal(params)
		res, err := m.svc.Dispatch(ctx, lbsvc.PaneActor(pn), &pn, method, b)
		if err != nil {
			return false
		}
		rb, _ := json.Marshal(res)
		return json.Unmarshal(rb, out) == nil
	}
	var b strings.Builder
	full := false
	add := func(line string) {
		if full || b.Len()+len(line)+1 > max {
			full = true
			return
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	section := func(head string, lines []string) {
		if len(lines) == 0 {
			return
		}
		if b.Len() > 0 {
			add("")
		}
		add(head)
		for _, l := range lines {
			add(l)
		}
	}
	var guides api.LogbookRecords
	if call("logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: logbook.TypeGuide, Tag: capcomTag, General: true, Limit: 500}}, &guides) {
		var lines []string
		for _, r := range guides.Records {
			lines = append(lines, fmt.Sprintf("- %s (%s)", oneLineTitle(r.Title), r.ID))
		}
		section("What you noted about the user (Logbook guides tagged capcom):", lines)
	}
	var brief api.LogbookBrief
	if call("logbook.brief", api.LogbookBriefParams{}, &brief) && strings.TrimSpace(brief.Text) != "" && brief.Records > 0 {
		section("The general Logbook brief (records tagged everywhere):", strings.Split(strings.TrimRight(brief.Text, "\n"), "\n"))
	}
	var tasks api.LogbookRecords
	if session != "" && call("logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Type: logbook.TypeTask, Author: session, All: true, Limit: 500}}, &tasks) {
		var lines []string
		for _, r := range tasks.Records {
			switch r.Status {
			case "open", "doing", "blocked":
				lines = append(lines, fmt.Sprintf("- [%s] %s (%s)", r.Status, oneLineTitle(r.Title), r.ID))
			}
		}
		section("Open tasks you filed:", lines)
	}
	return b.String()
}

func oneLineTitle(s string) string { return strings.Join(strings.Fields(s), " ") }
