package gffclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// threadState is the Capcom service's thread as gff shows it (capcom-service.md 7.7): text,
// one line per item, cards with their ids. The service session has no pane, so the thread
// fills the pane area in its own mode.
type threadState struct {
	host, session string
	items         []api.CapcomItem // oldest first
	typing        bool
	top           int // lines scrolled up from the end
}

// capcomService reports whether a row is a Capcom service session: kind capcom, no pane.
func (u *workspaceUI) capcomService(r board.Row) bool {
	if r.Pane != "" || r.Session == "" {
		return false
	}
	h, ok := u.c.cfg.Hosts.Host(r.Host)
	if !ok {
		return false
	}
	for _, s := range h.Structure.Sessions {
		if s.ID == r.Session {
			return s.Kind == api.SessionKindCapcom && s.Pane == ""
		}
	}
	return false
}

// openThread shows the thread of the service session in r and reads its newest page.
func (u *workspaceUI) openThread(r board.Row) {
	u.thread = &threadState{host: r.Host, session: r.Session}
	u.mode = modeThread
	u.readThread()
}

func (u *workspaceUI) readThread() {
	t := u.thread
	if t == nil {
		return
	}
	host := t.host
	u.call(host, "capcom thread", func(ctx context.Context, cl api.Client) error {
		page, err := cl.CapcomThreadPage(ctx, api.CapcomThreadPageParams{Limit: 200})
		if err != nil {
			return err
		}
		u.c.run(func() {
			if u.thread != nil && u.thread.host == host {
				for _, it := range page.Items {
					u.thread.put(it)
				}
				u.c.layoutChanged()
			}
		})
		return nil
	})
}

// put adds or replaces an item, in id order.
func (t *threadState) put(it api.CapcomItem) {
	for i := range t.items {
		if t.items[i].ID == it.ID {
			t.items[i] = it
			return
		}
	}
	t.items = append(t.items, it)
	sort.Slice(t.items, func(i, j int) bool { return t.items[i].ID < t.items[j].ID })
}

// threadEvent applies capcom.item and capcom.typing from the thread's host.
func (u *workspaceUI) threadEvent(hs *hostState, e api.Event) {
	t := u.thread
	if t == nil || hs.label != t.host {
		return
	}
	switch e.Type {
	case api.EventCapcomItem:
		var it api.CapcomItem
		if json.Unmarshal(e.Data, &it) == nil && it.ID != "" {
			t.put(it)
		}
	case api.EventCapcomTyping:
		var ty api.CapcomTyping
		if e.ID == t.session && json.Unmarshal(e.Data, &ty) == nil {
			t.typing = ty.On
		}
	}
	u.c.layoutChanged()
}

// threadKey handles a key in the thread: Esc or q returns, Enter or m writes a message, s
// stops the turn, the arrows and PageUp/PageDown scroll.
func (u *workspaceUI) threadKey(k key) {
	t := u.thread
	name := k.name
	if name == "" {
		name = string(k.r)
	}
	switch name {
	case "esc", "q":
		u.thread = nil
		u.mode = modePanes
	case "enter", "m":
		host, session := t.host, t.session
		u.ask(&prompt{label: "message Capcom: ", done: func(text, _ string) {
			if strings.TrimSpace(text) == "" {
				return
			}
			u.call(host, "send", func(ctx context.Context, cl api.Client) error {
				_, err := cl.SessionSend(ctx, api.SessionSendParams{Session: session, Text: text, Origin: api.SendOriginTyped, IdempotencyKey: newKey()})
				return err
			})
		}})
	case "s":
		u.call(t.host, "stop", func(ctx context.Context, cl api.Client) error {
			_, err := cl.CapcomInterrupt(ctx, api.CapcomServiceParams{})
			return err
		})
	case "up", "k":
		t.top++
	case "down", "j":
		t.top = max(t.top-1, 0)
	case "pgup":
		t.top += 10
	case "pgdn":
		t.top = max(t.top-10, 0)
	}
	u.c.layoutChanged()
}

func newKey() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ThreadLines is the thread as text, one line per item (capcom-service.md 7.7).
func ThreadLines(items []api.CapcomItem, typing bool) []string {
	var out []string
	lastUser := -1
	for i, it := range items {
		if it.Kind == "message" && it.Author.Kind == "user" {
			lastUser = i
		}
	}
	for i, it := range items {
		text := strings.Join(strings.Fields(it.Text), " ")
		var l string
		switch it.Kind {
		case "message":
			if it.Author.Kind == "user" {
				l = "you: " + text
				if n := len(it.Attachments); n > 0 {
					l += fmt.Sprintf(" [%d image%s]", n, plural(n))
				}
				if i == lastUser {
					if it.Read != "" {
						l += " · read"
					} else {
						l += " · delivered"
					}
				}
			} else {
				l = "capcom: " + text
				if it.Event {
					l = "capcom (wrote first): " + text
				}
			}
		case "tools":
			var names []string
			for _, c := range it.Tools {
				names = append(names, strings.TrimPrefix(c.Name, "mcp__flightdeck__"))
			}
			l = "  · tools: " + strings.Join(names, ", ")
		case "agent":
			l = "  · report from " + it.Author.Address + ": " + text
		case "round":
			l = "  · round: " + text
		case "card":
			typ, ref := "card", ""
			if it.Card != nil {
				typ, ref = it.Card.Type, it.Card.Ref
			}
			l = fmt.Sprintf("[%s %s] %s", typ, ref, text)
			if it.Answer != nil {
				l += " · " + it.Answer.Choice
			} else if it.Card != nil && len(it.Card.Choices) > 0 {
				l += " · " + strings.Join(it.Card.Choices, "/")
			}
		case "divider":
			l = "──── " + text + " ────"
		default:
			l = "  · " + text
		}
		out = append(out, l)
	}
	if typing {
		out = append(out, "capcom is typing…")
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// drawThread draws the thread in a, newest at the bottom.
func (u *workspaceUI) drawThread(a rect) {
	c, t := u.c, u.thread
	if a.h <= 0 || t == nil {
		return
	}
	c.canvas.text(a.x, a.y, a.w, " capcom on "+t.host+" · Enter writes · s stops · Esc returns", boldStyle)
	lines := ThreadLines(t.items, t.typing)
	avail := a.h - 1
	end := max(len(lines)-t.top, 0)
	start := max(end-avail, 0)
	for i, l := range lines[start:end] {
		st := vt.Style{}
		if strings.HasPrefix(l, "  · ") || strings.HasPrefix(l, "────") {
			st = dimStyle
		}
		c.canvas.text(a.x, a.y+1+i, a.w, l, st)
	}
}
