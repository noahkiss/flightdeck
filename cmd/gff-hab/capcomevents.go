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
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/rounds"
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
	// The mark alone decides: right after a start the engine and its session come up in the
	// background, and a message a peer pushed meanwhile waits in the queue, which holds every
	// batch until the engine's start, rather than being refused for good.
	if !e.holds() {
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
			if err := e.events.Add(capcomevents.Event{Item: it.ID, Urgent: urgent, Round: m.ID, Solo: len(it.Actions) > 0}); err != nil {
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
	if m.From.Session == channels.RoundSender(r.Name) {
		// A Capcom job's words (9.6) come only from a round runner's message: no session sends
		// under a round: sender. Words outside the closed set are dropped, not trusted.
		it.Job = m.From.Host + "/" + r.Name // two hosts may each have a round of one name
		for _, w := range r.Actions {
			if slices.Contains(rounds.Actions, string(w)) && !slices.Contains(it.Actions, string(w)) {
				it.Actions = append(it.Actions, string(w))
			}
		}
	}
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
	// A job with words goes in a batch of its own (9.6): no other job's prompt or event shares
	// the turn its words allow.
	if err := e.events.Add(capcomevents.Event{Item: stored.ID, Urgent: urgent, Round: m.ID, Solo: len(stored.Actions) > 0}); err != nil {
		// The runner sends it again; the thread holds it, and the duplicate path queues it.
		e.logf("capcom events: %v", err)
		return api.ChannelStateRefused, "the Capcom event queue did not take it"
	}
	return api.ChannelStateAccepted, ""
}
