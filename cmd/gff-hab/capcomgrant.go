package main

// The grant card of a standing instruction (capcom-service.md 9.6, 9.9): below
// capcom.event_actions = all, Capcom sets a user's job up with no words and the hab asks the user
// for them on a card. Grant is the person's round.update from the answering device: the answer
// carries only the choice, and the round id, the words and the digest of what the card showed
// come from the stored card.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
)

// jobGranter is what a grant card needs of the rounds service.
type jobGranter interface {
	GrantCard(ctx context.Context, id, digest string, words []string) (api.Round, error)
	Get(ref api.RoundRef) (api.Round, error)
}

// grantCard adds a grant card for round r and the words it lacks. The hab writes the text from
// the stored round, whole: the prompt Capcom follows, the trigger and the words. lead, when set,
// opens the text (a card shown again after the round changed).
func (e *capcomEngine) grantCard(turn string, r api.Round, words []string, lead string) {
	var b strings.Builder
	if lead != "" {
		b.WriteString(lead + "\n\n")
	}
	fmt.Fprintf(&b, "Capcom set up job %s. Let it act with: %s?\n\nIt follows this prompt when the job's events come in:\n\n%s\n\nTrigger: %s",
		r.Name, strings.Join(words, ", "), r.Prompt, triggerWords(r.Trigger))
	g := &capcomthread.CardGrant{Round: r.ID, Name: r.Name, Rev: r.Revision, Digest: roundDigest(r),
		Actions: words, Prompt: r.Prompt, Trigger: triggerWords(r.Trigger)}
	if _, _, err := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindCard, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Turn: turn, Text: b.String(), Card: &capcomthread.Card{Type: capcomthread.CardGrantType, Ref: r.ID,
			Choices: []string{capcomthread.ChoiceGrant, capcomthread.ChoiceNotNow}, Grant: g}}); err != nil {
		e.logf("capcom: store a grant card: %v", err)
	}
}

// answerGrant answers a grant card (9.9). The first answer wins. Grant adds the card's words to
// the round while its definition is as the card showed it; a changed round grants nothing: the
// card ends expired, a new card shows the round as it is now, and the answer is conflict. The
// model hears the outcome as a note marked data.
func (e *capcomEngine) answerGrant(ctx context.Context, id, client, choice string) (api.CapcomCardAnswered, error) {
	e.grantMu.Lock()
	defer e.grantMu.Unlock()
	it, ok := e.thread.Get(id)
	if !ok || it.Card == nil || it.Card.Grant == nil {
		return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindNotFound, "no grant card %s in the Capcom thread", id)
	}
	if it.Answer != nil {
		return api.CapcomCardAnswered{Item: apiItem(it)}, control.Errorf(api.ErrorKindConflict, "card %s was answered already (%s)", id, it.Answer.Choice)
	}
	g := it.Card.Grant
	words := strings.Join(g.Actions, ", ")
	if choice == capcomthread.ChoiceGrant {
		if e.jobs == nil {
			return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindUnavailable, "this hab has no rounds: nothing to grant")
		}
		_, err := e.jobs.GrantCard(ctx, g.Round, g.Digest, g.Actions)
		var ce *control.Error
		switch {
		case errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindConflict:
			e.endGrantCard(it)
			if r, gerr := e.jobs.Get(api.RoundRef{ID: g.Round}); gerr == nil && r.Mode == api.RoundModeCapcom {
				e.grantCard(it.Turn, r, g.Actions, "The job changed since the last card. Read its prompt again.")
			}
			return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindConflict, "job %s changed since the card, and nothing was granted: a new card shows it as it is now", g.Name)
		case errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound:
			e.endGrantCard(it)
			return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindNotFound, "job %s is gone: nothing to grant", g.Name)
		case err != nil:
			return api.CapcomCardAnswered{}, err
		}
	}
	done, err := e.thread.AnswerCard(it.ID, choice, client)
	if errors.Is(err, capcomthread.ErrConflict) {
		return api.CapcomCardAnswered{Item: apiItem(done)}, control.Errorf(api.ErrorKindConflict, "card %s was answered already", id)
	}
	if err != nil {
		return api.CapcomCardAnswered{}, err
	}
	// The job's name is the round's, which Capcom wrote: it is quoted as data, and the note
	// neither opens nor joins a user turn (capcom-service.md 9.6).
	text := fmt.Sprintf("The user chose Not now on grant card %s: job %q has no new words.", it.ID, g.Name)
	if choice == capcomthread.ChoiceGrant {
		text = fmt.Sprintf("The user granted job %q the words %s on %s (card %s).", g.Name, words, client, it.ID)
	}
	e.addNote(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: text, Model: true, Data: true})
	return api.CapcomCardAnswered{Item: apiItem(done)}, nil
}

// endGrantCard closes a grant card that can no longer grant: the round changed or went away.
func (e *capcomEngine) endGrantCard(it capcomthread.Item) {
	if _, err := e.thread.AnswerCard(it.ID, "expired", ""); err != nil && !errors.Is(err, capcomthread.ErrConflict) {
		e.logf("capcom: end a grant card: %v", err)
	}
}
