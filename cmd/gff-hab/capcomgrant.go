package main

// The grant card of a standing instruction (capcom-service.md 9.6, 9.9): below
// capcom.event_actions = all, Capcom sets a user's job up with no words and the hab asks the user
// for them on a card. Grant is the person's round.update from the answering device: the answer
// carries only the choice, and the round id, the words and the digest of what the card showed
// come from the stored card.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
)

// jobGranter is what a grant card needs of the rounds service.
type jobGranter interface {
	GrantCard(ctx context.Context, id, digest string, words []string) (api.Round, []string, error)
	Get(ref api.RoundRef) (api.Round, error)
}

// grantAsk is the words of want a grant card would offer at the level now: those it does not
// give every event turn.
func (e *capcomEngine) grantAsk(want []string) []string {
	level := e.eventActionsLevel()
	return slices.DeleteFunc(slices.Clone(want), func(w string) bool { return freeWord(level, w) })
}

// reserveGrant takes the running turn's one grant card (9.9) for a create that asks for words,
// before the round is made. A turn has at most one: a second job that needs a grant in the same
// turn is refused, with words for the model, and nothing is made. A create whose words the level
// already gives needs no card and is let through.
func (e *capcomEngine) reserveGrant(want []string) error {
	if len(e.grantAsk(want)) == 0 {
		return nil
	}
	turn := e.turnState(0).Turn
	e.grantMu.Lock()
	defer e.grantMu.Unlock()
	if turn != "" && e.grantTurn == turn {
		return control.Errorf(api.ErrorKindDenied, "round.create: this turn already showed the user a grant card, and a turn shows at most one. Nothing was made. Tell the user, and set this job up after they answer the card, in a later turn")
	}
	e.grantTurn = turn
	return nil
}

// blankRuns is two or more blank lines in a row.
var blankRuns = regexp.MustCompile(`\n[ \t]*(\n[ \t]*){2,}`)

// shownPrompt is a prompt as a card shows it: runs of blank lines cut to one, so no padding puts
// the instruction below the card's buttons (9.9). The digest still covers the prompt as stored.
func shownPrompt(p string) string {
	return strings.TrimSpace(blankRuns.ReplaceAllString(p, "\n\n"))
}

// grantTrigger is a job's trigger as a grant card shows it: the plain words, then each event
// filter field and the watcher's program, so the card names what feeds the job (9.9).
func grantTrigger(t api.RoundTrigger) string {
	s := triggerWords(t)
	if f := t.Filter; f != nil {
		var m map[string]any
		if b, err := json.Marshal(f); err == nil && json.Unmarshal(b, &m) == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			var parts []string
			for _, k := range keys {
				v := m[k]
				if l, ok := v.([]any); ok {
					var ws []string
					for _, x := range l {
						ws = append(ws, fmt.Sprint(x))
					}
					v = strings.Join(ws, ",")
				}
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
			if len(parts) > 0 {
				s += ", where " + strings.Join(parts, " ")
			}
		}
	}
	if w := t.Watch; w != nil {
		s += ", watcher: " + strings.Join(w.Argv, " ")
		if w.Cwd != "" {
			s += " (in " + w.Cwd + ")"
		}
		if len(w.Kinds) > 0 {
			s += ", kinds " + strings.Join(w.Kinds, ",")
		}
	}
	return s
}

// grantCard adds a grant card for round r and the words it lacks. The hab writes the text from
// the stored round: the prompt Capcom follows, the trigger and the words. lead, when set, opens
// the text (a card shown again after the round changed).
func (e *capcomEngine) grantCard(turn string, r api.Round, words []string, lead string) {
	var b strings.Builder
	if lead != "" {
		b.WriteString(lead + "\n\n")
	}
	prompt, trigger := shownPrompt(r.Prompt), grantTrigger(r.Trigger)
	fmt.Fprintf(&b, "Capcom set up job %s. Let it act with: %s?\n\nIt follows this prompt (%d characters) when the job's events come in:\n\n%s\n\nTrigger: %s",
		r.Name, strings.Join(words, ", "), len([]rune(r.Prompt)), prompt, trigger)
	g := &capcomthread.CardGrant{Round: r.ID, Name: r.Name, Rev: r.Revision, Digest: roundDigest(r),
		Actions: words, Prompt: prompt, Trigger: trigger}
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
	var added []string
	if choice == capcomthread.ChoiceGrant {
		if e.jobs == nil {
			return api.CapcomCardAnswered{}, control.Errorf(api.ErrorKindUnavailable, "this hab has no rounds: nothing to grant")
		}
		var err error
		_, added, err = e.jobs.GrantCard(ctx, g.Round, g.Digest, g.Actions)
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
	var text string
	switch {
	case choice != capcomthread.ChoiceGrant:
		text = fmt.Sprintf("The user chose Not now on grant card %s: job %q has no new words.", it.ID, g.Name)
	case len(added) == 0:
		text = fmt.Sprintf("The user tapped Grant on card %s on %s: job %q, unchanged since the card, already held the words %s, so nothing changed.",
			it.ID, client, g.Name, strings.Join(g.Actions, ", "))
	default:
		text = fmt.Sprintf("The user granted job %q the words %s on %s (card %s).", g.Name, strings.Join(added, ", "), client, it.ID)
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
