package main

// Capcom writes first (capcom-service.md 9): the events the service looks at on its own. Notices
// raised on this hab and round messages from any hab become thread items; the event queue
// (internal/capcomevents) batches them, holds them through quiet hours and past the daily event
// turn budget, and hands each batch to the engine as one event turn. An event turn may read and
// message the user, never act (9.4): the route refuses its acting calls.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomevents"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// settingValue reads one hab-side setting, or its default before the settings service runs.
func (e *capcomEngine) settingValue(id string) json.RawMessage {
	if e.setting != nil {
		if v := e.setting(id); len(v) > 0 {
			return v
		}
	}
	d, _ := settings.Lookup(id)
	return d.Default
}

func (e *capcomEngine) settingOn(id string) bool {
	var b bool
	return json.Unmarshal(e.settingValue(id), &b) == nil && b
}

func (e *capcomEngine) settingInt(id string) int {
	var n int
	json.Unmarshal(e.settingValue(id), &n)
	return n
}

// eventLimits are the queue's limits from the hab-side settings (19.1).
func (e *capcomEngine) eventLimits() capcomevents.Limits {
	l := capcomevents.DefaultLimits
	var qh string
	if json.Unmarshal(e.settingValue(settings.CapcomQuietHours), &qh) == nil {
		if s, end, err := settings.ParseQuietHours(qh); err == nil {
			l.QuietStart, l.QuietEnd = s, end
		}
	}
	if n := e.settingInt(settings.CapcomDailyTurns); n > 0 {
		l.DailyTurns = n
	}
	if n := e.settingInt(settings.CapcomBatchWindow); n > 0 {
		l.BatchWindow = time.Duration(n) * time.Second
	}
	if n := e.settingInt(settings.CapcomBatchMax); n > 0 {
		l.BatchMax = n
	}
	return l
}

// SettingsChanged checks the queue again: a limit may have moved.
func (e *capcomEngine) SettingsChanged() {
	if e.events != nil {
		e.events.Check()
	}
}

// observe takes each published event: a notice raised on this hab reaches Capcom as an event
// (9.1), unless it is passive, its source is turned off, or it is about the service itself.
func (e *capcomEngine) observe(ev structure.Event) {
	if ev.Type == api.EventSettingsUpdated && strings.HasPrefix(ev.ID, "capcom.") {
		// A limit may have moved. Off this goroutine: the settings service publishes with its
		// lock held, and the check reads the settings.
		go e.SettingsChanged()
		return
	}
	if ev.Type != control.EventNoticeRaised {
		return
	}
	n, ok := ev.Data.(api.Notice)
	if !ok {
		return
	}
	session := e.Session()
	if session == "" || n.Level == api.NoticeLevelPassive || (n.Session != "" && n.Session == session) {
		return
	}
	src := settings.CapcomEventsNotices
	if n.Round != nil {
		src = settings.CapcomEventsRounds
	}
	if !e.settingOn(src) {
		return
	}
	text := strings.TrimSpace(n.Title)
	if b := strings.TrimSpace(n.Body); b != "" {
		text += "\n" + b
	}
	if n.Level != "" && n.Level != api.NoticeLevelActive {
		text += "\n(level " + string(n.Level) + ")"
	}
	it, added, err := e.thread.Add(capcomthread.Item{Kind: capcomthread.KindEvent, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck},
		Key: "notice:" + n.ID, Text: text})
	if err != nil {
		e.logf("capcom: store a notice event: %v", err)
		return
	}
	if !added && (!it.Read.IsZero() || e.events.Holds(it.ID)) {
		return // a repeat; one a crash left out of the queue goes in now
	}
	if err := e.events.Add(capcomevents.Event{Item: it.ID, Urgent: n.Level == api.NoticeLevelTimeSensitive}); err != nil {
		e.logf("capcom events: %v", err)
	}
}

// roundEventData bounds one round event's data in the thread item.
const roundEventData = 2 << 10

// DeliverRound answers a round message to capcom (9.5): accepted with one round item added,
// duplicate for a message id the thread holds, declined off with round messages turned off, and
// refused on a hab that does not run the service. The channels service calls it with its lock
// held: nothing here calls back into channels.
func (e *capcomEngine) DeliverRound(m api.ChannelMessage) (api.ChannelState, string) {
	if !e.holds() || e.Session() == "" {
		if label := e.moved(); label != "" {
			return api.ChannelStateRefused, "Capcom moved to " + label
		}
		return api.ChannelStateRefused, "no Capcom service on this hab"
	}
	key := "round:" + m.ID
	urgent := m.Round.Level == string(api.NoticeLevelTimeSensitive)
	if it, held := e.thread.ByKey(key); held {
		// A crash between the thread and the queue left it in neither batch nor turn: queue it
		// now, so the retry is still seen.
		if it.Read.IsZero() && !e.events.Holds(it.ID) {
			if err := e.events.Add(capcomevents.Event{Item: it.ID, Urgent: urgent, Round: m.ID}); err != nil {
				return api.ChannelStateRefused, "the Capcom event queue did not take it"
			}
		}
		return api.ChannelStateDuplicate, ""
	}
	if !e.settingOn(settings.CapcomEventsRoundMessages) {
		return api.ChannelStateDeclined, "off"
	}
	r := m.Round
	text := m.Text
	if r.Prompt != "" {
		text = r.Prompt
	}
	it := capcomthread.Item{Kind: capcomthread.KindRound, Key: key, Text: text,
		Author: capcomthread.Author{Kind: capcomthread.AuthorAgent, Address: m.From.Host + "/" + m.From.Session}}
	for _, ev := range r.Events {
		c := capcomthread.Context{Kind: "round_event", Title: strings.TrimSpace(ev.Kind + ": " + ev.Title)}
		if ev.Level != "" {
			c.Title += " (" + ev.Level + ")"
		}
		if d := string(ev.Data); d != "" && d != "null" {
			if len(d) > roundEventData {
				d = strings.ToValidUTF8(d[:roundEventData], "") + " [cut]"
			}
			c.Text = d
		}
		it.Context = append(it.Context, c)
	}
	if r.Omitted > 0 {
		it.Context = append(it.Context, capcomthread.Context{Kind: "round_event", Title: fmt.Sprintf("%d more events left out", r.Omitted)})
	}
	stored, added, err := e.thread.Add(it)
	if err != nil {
		e.logf("capcom: store a round message: %v", err)
		return api.ChannelStateRefused, "the Capcom thread did not take it"
	}
	if !added {
		return api.ChannelStateDuplicate, ""
	}
	if err := e.events.Add(capcomevents.Event{Item: stored.ID, Urgent: urgent, Round: m.ID}); err != nil {
		// The runner sends it again; the thread holds it, and the duplicate path queues it.
		e.logf("capcom events: %v", err)
		return api.ChannelStateRefused, "the Capcom event queue did not take it"
	}
	return api.ChannelStateAccepted, ""
}

// eventActing are the route methods an event turn may not call (9.4, E21): a hand-off and
// any message to an agent (hand_off sends through channel.send), a session or project start
// (confirm.request), a group's acting calls, and round changes.
var eventActing = []string{"channel.send", "confirm.request",
	"group.create", "group.amend", "group.close", "group.retry", "member.compact", "member.handoff", "member.action",
	"round.create", "round.update", "round.delete", "round.run", "round.pause", "round.unpause"}

// routeTurnWait is how long an acting call from the engine's route waits for its turn to be
// known: the call comes from inside a turn, but the turn.started before it travels on the
// engine's own connection and can arrive after it.
const routeTurnWait = 5 * time.Second

// routeGuard refuses an acting call from the engine's route unless the running turn is a user
// turn: a user message opened it or joined it (9.4). An event turn's call is refused, and so is
// one with no turn known, since only a turn runs tools.
func (e *capcomEngine) routeGuard(pane, method string) error {
	if pane != capcomRoute || !slices.Contains(eventActing, method) {
		return nil
	}
	st := e.link.WaitTurn(routeTurnWait)
	if st.Turn != "" && st.UserTurn {
		return nil
	}
	return control.Errorf(api.ErrorKindDenied, "%s is not allowed in a turn Capcom began on its own, from an event or an agent's report: ask the user first, and act once they answer", method)
}
