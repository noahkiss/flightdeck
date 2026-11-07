package main

import (
	"context"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// engineChat answers chat.page for a Capcom service session from the thread (capcom-service.md
// 7.4), so a client built for sessions shows it with no change. Every other session goes on to
// the transcript readers.
type engineChat struct {
	next control.Chat // nil: no transcript readers
	eng  *capcomEngine
}

func (c engineChat) ChatPage(ctx context.Context, p api.ChatPageParams) (api.ChatMessages, error) {
	if c.eng.serviceRecord(p.Session) {
		return c.eng.chatPage(p)
	}
	if c.next == nil {
		return api.ChatMessages{}, control.Errorf(api.ErrorKindUnavailable, "chat.page needs the holder link, which this hab is running without")
	}
	return c.next.ChatPage(ctx, p)
}

// serviceRecord reports whether id is a service session's record: the one that runs now, or an
// earlier one this hab kept after it lost the mark (kind capcom, no pane, the engine folder).
func (e *capcomEngine) serviceRecord(id string) bool {
	if id == "" {
		return false
	}
	if id == e.Session() {
		return true
	}
	ss, ok := e.store.Session(id)
	return ok && ss.Kind == api.SessionKindCapcom && ss.Pane == "" && ss.Dir == capcomengine.EngineDir(e.dir)
}

// chatPage reads the thread as chat messages: a page before p.Before, newest at the end, or the
// items after the cursor p.After. The cursor is the newest item id a page returned; a changed
// item (a read mark, an answer) reaches clients as a chat.message event with the same id.
func (e *capcomEngine) chatPage(p api.ChatPageParams) (api.ChatMessages, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, capcomthread.PageMax)
	out := api.ChatMessages{Messages: []api.ChatMessage{}}
	if p.After != "" {
		items := e.thread.After(p.After, nil)
		if len(items) > limit {
			items, out.More = items[:limit], true
		}
		for _, it := range items {
			out.Messages = append(out.Messages, chatMessage(p.Session, it))
		}
		out.Cursor = p.After
		if n := len(items); n > 0 {
			out.Cursor = items[n-1].ID
		}
		return out, nil
	}
	if p.Before != "" {
		if _, ok := e.thread.Get(p.Before); !ok {
			return api.ChatMessages{}, control.Errorf(api.ErrorKindNotFound, "no message %s in session %s", p.Before, p.Session)
		}
	}
	items, more := e.thread.Page(p.Before, limit)
	out.More = more
	for i := len(items) - 1; i >= 0; i-- {
		out.Messages = append(out.Messages, chatMessage(p.Session, items[i]))
	}
	if p.Before == "" {
		out.Cursor = e.thread.Last()
	}
	return out, nil
}

// chatMessage is a thread item as a chat message: the user's messages and agents' reports as
// user lines (an agent's with its address as sender), Capcom's replies as agent lines, and
// every other item as an event line.
func chatMessage(session string, it capcomthread.Item) api.ChatMessage {
	m := api.ChatMessage{ID: it.ID, Session: session, At: it.Time.UTC().Format(time.RFC3339Nano), Text: it.Text}
	switch it.Kind {
	case capcomthread.KindMessage:
		m.Kind = api.ChatKindUser
		if it.Author.Kind == capcomthread.AuthorCapcom {
			m.Kind = api.ChatKindAgent
		}
	case capcomthread.KindAgent:
		m.Kind, m.Sender = api.ChatKindUser, it.Author.Address
	case capcomthread.KindTools:
		var calls []string
		for _, t := range it.Tools {
			calls = append(calls, strings.TrimSpace(t.Name+" "+t.Summary))
		}
		m.Kind, m.Event, m.Text = api.ChatKindEvent, api.ChatEventCommand, strings.Join(calls, "\n")
	case capcomthread.KindCard:
		m.Kind, m.Event = api.ChatKindEvent, api.ChatEventNotice
		if it.Card != nil && it.Card.Type == "approval" {
			m.Event = api.ChatEventPermission
		}
		if it.Answer != nil {
			m.Text += " (" + it.Answer.Choice + ")"
		}
	case capcomthread.KindDivider:
		m.Kind, m.Event = api.ChatKindEvent, api.ChatEventCompaction
	default: // note, round
		m.Kind, m.Event = api.ChatKindEvent, api.ChatEventNotice
	}
	return m
}

// publishChat sends a thread item as a chat.message event of the running service session.
func (e *capcomEngine) publishChat(it capcomthread.Item) {
	if id := e.Session(); id != "" {
		e.publish(structure.Event{Type: control.EventChatMessage, ID: it.ID, Data: api.NonNil(chatMessage(id, it))})
	}
}
