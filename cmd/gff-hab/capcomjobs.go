package main

// Capcom jobs act on events (capcom-service.md 9.6, stage A9). A Capcom job is a capcom round
// with action words a person granted. The round message carries them; gff-hab copies them onto
// the round item, and only from a message whose sender is round:<name> (DeliverRound). Here the
// hab decides each acting call an event turn makes on the engine's route:
//
//   - The words a turn may use are the union of the words on the round items its batches hold,
//     capped by capcom.event_actions: off allows none, ask turns each call into one confirmation
//     that names the job, allow lets it run under its own gates.
//   - At most capcom.event_actions_per_turn calls per turn, counted in the event queue so a
//     restart keeps the count. A call over the limit is refused.
//   - Each allowed call adds a card to the thread that names the job, and makes the outcome of
//     every round message the turn took acted.
//
// Nothing here reads an agent's channel message or the events' text: the words come from the
// round items alone, and the engine's own list in the batch input is only a hint to it.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/settings"
)

// The values of capcom.event_actions (19.1).
const (
	eventActionsOff   = "off"
	eventActionsAsk   = "ask"
	eventActionsAllow = "allow"
)

// cardJob is the card type of a Capcom job's action in the thread (9.6): a record with no
// choices; its ref is the job.
const cardJob = "job"

// eventActing are the route methods an event turn may not call (9.4, E21) unless a Capcom job
// allows them (9.6): a hand-off and any message to an agent (hand_off sends through
// channel.send), a session or project start (confirm.request), a group's acting calls, a
// calendar or reminder card, and round changes, which no job allows.
var eventActing = []string{"channel.send", "confirm.request",
	"group.create", "group.amend", "group.close", "group.retry", "member.compact", "member.handoff", "member.action",
	"personal.calendar_add", "personal.reminder_add",
	"round.create", "round.update", "round.delete", "round.run", "round.pause", "round.unpause"}

// actionWord is the job word that allows an acting call, or "" when no word does.
func actionWord(method string, params json.RawMessage) string {
	switch method {
	case "channel.send":
		return "hand_off"
	case "confirm.request":
		var p struct {
			Kind api.ConfirmKind `json:"kind"`
		}
		json.Unmarshal(params, &p)
		switch p.Kind {
		case "start":
			return "start_session"
		case "setup":
			return "set_up_project"
		}
		return ""
	case "group.create", "group.amend", "group.close", "group.retry", "member.compact", "member.handoff", "member.action":
		return "group"
	case "personal.calendar_add", "personal.reminder_add":
		return "card"
	}
	return ""
}

// routeTurnWait is how long an acting call from the engine's route waits for its turn to be
// known: the call comes from inside a turn, but the turn.started before it travels on the
// engine's own connection and can arrive after it.
const routeTurnWait = 5 * time.Second

// batchWait is how long an event turn's acting call waits for the hab to hear which batch the
// turn took (input.taken), which also travels on the engine's own connection.
const batchWait = 2 * time.Second

// turnState is the engine's running turn, waiting up to wait for one to be known. Tests set
// turnNow in place of a link.
func (e *capcomEngine) turnState(wait time.Duration) capcomengine.Status {
	if e.turnNow != nil {
		return e.turnNow()
	}
	if wait == 0 {
		return e.link.Status()
	}
	return e.link.WaitTurn(wait)
}

// errAskFirst refuses an event turn's call no job allows (9.4).
func errAskFirst(method, why string) error {
	return control.Errorf(api.ErrorKindDenied, "%s is not allowed in a turn Capcom began on its own, from an event or an agent's report%s: ask the user first, and act once they answer", method, why)
}

// eventActionsLevel is capcom.event_actions; an unknown value reads as off.
func (e *capcomEngine) eventActionsLevel() string {
	var v string
	json.Unmarshal(e.settingValue(settings.CapcomEventActions), &v)
	switch v {
	case eventActionsAsk, eventActionsAllow:
		return v
	}
	return eventActionsOff
}

// turnJobs returns the jobs among the round items turn took whose words include word, waiting
// up to batchWait for the turn's batch to be known.
func (e *capcomEngine) turnJobs(turn, word string) []string {
	if e.events == nil || turn == "" {
		return nil
	}
	deadline := time.Now().Add(batchWait)
	bs := e.events.TurnBatches(turn)
	for len(bs) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		bs = e.events.TurnBatches(turn)
	}
	var jobs []string
	for _, b := range bs {
		for _, j := range e.batchJobs(b, word) {
			if !slices.Contains(jobs, j) {
				jobs = append(jobs, j)
			}
		}
	}
	return jobs
}

// batchJobs returns the jobs of a batch's round items whose words include word.
func (e *capcomEngine) batchJobs(b capcomevents.Batch, word string) []string {
	var jobs []string
	for _, id := range b.Items {
		it, ok := e.thread.Get(id)
		if !ok || it.Kind != capcomthread.KindRound || it.Job == "" || !slices.Contains(it.Actions, word) {
			continue
		}
		if !slices.Contains(jobs, it.Job) {
			jobs = append(jobs, it.Job)
		}
	}
	return jobs
}

// batchActions is the words a batch's turn may use, capped by the level and the per-turn
// limit, for the engine's batch input. The hab checks every call again.
func (e *capcomEngine) batchActions(b capcomevents.Batch) []string {
	if e.eventActionsLevel() == eventActionsOff || e.settingInt(settings.CapcomEventActionsPerTurn) <= 0 {
		return nil
	}
	var out []string
	for _, w := range []string{"hand_off", "start_session", "set_up_project", "group", "card", "fetch"} {
		if len(e.batchJobs(b, w)) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// allowCall decides one call of an event turn under the Capcom jobs it took: the jobs that
// allow word, or the refusal. It counts the call when it allows it.
func (e *capcomEngine) allowCall(turn, word, method string) (jobs []string, level string, err error) {
	if word == "" {
		return nil, "", errAskFirst(method, "")
	}
	jobs = e.turnJobs(turn, word)
	if len(jobs) == 0 {
		return nil, "", errAskFirst(method, ", and no Capcom job in this turn allows "+word)
	}
	level = e.eventActionsLevel()
	if level == eventActionsOff {
		return nil, "", errAskFirst(method, ": Capcom jobs may not act (capcom.event_actions is off)")
	}
	limit := e.settingInt(settings.CapcomEventActionsPerTurn)
	if !e.events.Act(turn, limit) {
		return nil, "", errAskFirst(method, fmt.Sprintf(": this turn used its %d action(s) (capcom.event_actions_per_turn)", limit))
	}
	return jobs, level, nil
}

// routeGuard checks each acting call from the engine's route. A user turn's call goes on. An
// event turn's call goes on only under a Capcom job that allows it (9.6); under ask it becomes
// one confirmation and does not run now. Any other is refused, as is one with no turn known,
// since only a turn runs tools.
func (e *capcomEngine) routeGuard(ctx context.Context, pane, method string, params json.RawMessage) (json.RawMessage, error) {
	if pane != capcomRoute || !slices.Contains(eventActing, method) {
		return nil, nil
	}
	st := e.turnState(routeTurnWait)
	if st.Turn != "" && st.UserTurn {
		return nil, nil
	}
	if st.Turn == "" {
		return nil, errAskFirst(method, "")
	}
	word := actionWord(method, params)
	jobs, level, err := e.allowCall(st.Turn, word, method)
	if err != nil {
		return nil, err
	}
	job := strings.Join(jobs, ", ")
	summary := actionSummary(method, params)
	if level == eventActionsAllow || word == "card" {
		// allow: it runs under its own gates. A card's own Add tap is its one confirmation.
		e.jobCard(st.Turn, fmt.Sprintf("For job %s, Capcom used %s: %s", job, word, summary), job)
		return nil, nil
	}
	if e.confirmAction == nil || e.dispatch == nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "%s: this hab cannot ask the user to confirm a Capcom job's action", method)
	}
	dispatch := e.dispatch
	c, err := e.confirmAction(ctx, capcomServiceName, api.ConfirmAction{Job: job, Word: api.RoundAction(word), Summary: summary},
		time.Duration(e.settingInt(settings.CapcomEventActionMaxAge))*time.Minute,
		func(ctx context.Context) (string, error) {
			res, err := dispatch(ctx, pane, method, params)
			if err != nil {
				return "", err
			}
			return resultLine(res), nil
		})
	if err != nil {
		return nil, err
	}
	e.jobCard(st.Turn, fmt.Sprintf("For job %s, Capcom asks to use %s: %s. Nothing runs before you confirm.", job, word, summary), job)
	if method == "confirm.request" {
		// The start tools read a confirmation: this one is the job's, open on the user's devices.
		b, err := json.Marshal(api.ConfirmRequested{Confirmation: c})
		return b, err
	}
	return nil, control.Errorf(api.ErrorKindDenied, "%s waits for the user: confirmation %s for job %s is open on their devices, and nothing ran. Their answer reaches you later as a message from Flightdeck. Do not say it was done", method, c.ID, job)
}

// eventGate decides an approval request of an event turn (5.3, 9.6): a WebFetch of a URL the
// turn did not see is allowed to raise its card only under a job that allows fetch. The card
// is the one confirmation, under ask and allow alike.
func (e *capcomEngine) eventGate(turn, tool string) (string, error) {
	word := ""
	if tool == "WebFetch" {
		word = "fetch"
	}
	jobs, _, err := e.allowCall(turn, word, tool)
	if err != nil {
		return "", err
	}
	job := strings.Join(jobs, ", ")
	return "for job " + job, nil
}

// turnAllows reports whether the running event turn has a Capcom job that allows word while
// the level lets jobs act. The group service's own event-turn check reads it; routeGuard did the
// counting.
func (e *capcomEngine) turnAllows(word string) bool {
	st := e.turnState(0)
	if st.Turn == "" || e.eventActionsLevel() == eventActionsOff || e.events == nil {
		return false
	}
	for _, b := range e.events.TurnBatches(st.Turn) {
		if len(e.batchJobs(b, word)) > 0 {
			return true
		}
	}
	return false
}

// jobCard adds the card of a job's action to the thread (9.6): a record with no choices.
func (e *capcomEngine) jobCard(turn, text, job string) {
	if _, _, err := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindCard, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Turn: turn, Text: text, Card: &capcomthread.Card{Type: cardJob, Ref: job}}); err != nil {
		e.logf("capcom: store a job's card: %v", err)
	}
}

// actionSummary says what an acting call would do, in one line, for its card.
func actionSummary(method string, params json.RawMessage) string {
	var p struct {
		To    string                  `json:"to"`
		Text  string                  `json:"text"`
		Kind  string                  `json:"kind"`
		Start *api.ConfirmStart       `json:"start"`
		Setup *api.ConfirmSetup       `json:"setup"`
		Group string                  `json:"group"`
		Plan  *struct{ Title string } `json:"plan"`
		Title string                  `json:"title"`
	}
	json.Unmarshal(params, &p)
	var s string
	switch {
	case method == "channel.send":
		s = "message " + p.To + ": " + p.Text
	case method == "confirm.request" && p.Start != nil:
		host := p.Start.Host
		if host == "" {
			host = "this host"
		}
		s = fmt.Sprintf("start %s in %s on %s", p.Start.Harness, p.Start.Folder, host)
		if p.Start.Seed != "" {
			s += ": " + p.Start.Seed
		}
	case method == "confirm.request" && p.Setup != nil:
		s = "set up " + p.Setup.Repository + " on " + p.Setup.Host
	case strings.HasPrefix(method, "personal."):
		s = strings.TrimPrefix(method, "personal.") + " " + p.Title
	default:
		s = method
		if p.Group != "" {
			s += " " + p.Group
		}
		if p.Plan != nil && p.Plan.Title != "" {
			s += ": " + p.Plan.Title
		}
	}
	return clipLine(s, 300)
}

// resultLine is a confirmed call's answer in one line.
func resultLine(b json.RawMessage) string {
	var r struct {
		ID           string            `json:"id"`
		To           string            `json:"to"`
		State        string            `json:"state"`
		Confirmation *api.Confirmation `json:"confirmation"`
	}
	json.Unmarshal(b, &r)
	switch {
	case r.Confirmation != nil:
		return fmt.Sprintf("Confirmation %s is %s.", r.Confirmation.ID, r.Confirmation.State)
	case r.To != "":
		return fmt.Sprintf("Message %s to %s: %s.", r.ID, r.To, r.State)
	}
	return clipLine(string(b), 300)
}

// clipLine is s on one line, cut to n bytes.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}
