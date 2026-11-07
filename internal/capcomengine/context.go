package capcomengine

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/capcomthread"
)

// ContextLimits are the context manager's [capcom] keys (capcom-service.md 6, 19.2), read at
// each use.
type ContextLimits struct {
	SummaryMaxChars   int
	SummaryInputChars int
	SummaryQuiet      time.Duration
	ReseedAt          float64 // a share of the model's window
	ReseedQuiet       time.Duration
	ReseedQuietAt     float64
	SeedMessages      int
	SeedMessagesChars int
	SeedLogbookBytes  int
}

// DefaultContextLimits are 19.2's defaults.
var DefaultContextLimits = ContextLimits{SummaryMaxChars: 6000, SummaryInputChars: 64000, SummaryQuiet: 10 * time.Minute,
	ReseedAt: 0.6, ReseedQuiet: time.Hour, ReseedQuietAt: 0.25, SeedMessages: 6, SeedMessagesChars: 8000, SeedLogbookBytes: 8 << 10}

// The context manager (6) is the part of the link that keeps the model's context fit: it asks
// the engine for a re-seed at a turn's end above reseed_at, or after a quiet hour above
// reseed_quiet_at, hands it a quiet summary update after summary_quiet, and answers its
// context.request with the summary material and the seed's parts. The engine owns the summary
// call, the fresh SDK session and the seed's order; the hab owns the triggers, the thread and
// Logbook. A re-seed never starts mid-turn or with an approval open: the engine holds a
// reseed until both are clear.

// ctxState is the context manager's state, under Link.mu.
type ctxState struct {
	lastContext float64     // context use at the last turn's end
	asked       bool        // a reseed was sent; the next session.started clears it
	quietT      func() bool // the summary_quiet timer
	reseedT     func() bool // the reseed_quiet timer
}

func (l *Link) limits() ContextLimits {
	if l.cfg.Limits == nil {
		return DefaultContextLimits
	}
	return l.cfg.Limits()
}

// stopQuietLocked stops both quiet timers. The caller holds l.mu.
func (l *Link) stopQuietLocked() {
	if l.cx.quietT != nil {
		l.cx.quietT()
		l.cx.quietT = nil
	}
	if l.cx.reseedT != nil {
		l.cx.reseedT()
		l.cx.reseedT = nil
	}
}

// turnEnded runs the size trigger, else arms the quiet timers.
func (l *Link) turnEnded(m Message) {
	lim := l.limits()
	l.mu.Lock()
	l.stopQuietLocked()
	l.cx.lastContext = m.ContextUsed
	limit := !l.cx.asked && m.ContextUsed > lim.ReseedAt
	if limit {
		l.cx.asked = true
	} else {
		l.cx.quietT = l.cfg.After(lim.SummaryQuiet, l.quietSummary)
		l.cx.reseedT = l.cfg.After(lim.ReseedQuiet, l.quietReseed)
	}
	l.mu.Unlock()
	if limit {
		if err := l.Reseed(ReseedLimit); err != nil {
			l.mu.Lock()
			l.cx.asked = false
			l.mu.Unlock()
		}
	}
}

// quietSummary hands the engine the items since the last summary when the thread has been
// quiet for summary_quiet after a turn (6.1).
func (l *Link) quietSummary() {
	l.mu.Lock()
	l.cx.quietT = nil
	c, busy := l.cur, l.st.Turn != ""
	l.mu.Unlock()
	if c == nil || !c.ready || busy {
		return
	}
	m := l.contextFor(ForQuiet)
	if len(m.Recent) == 0 {
		return
	}
	if err := l.send(c, m); err != nil {
		l.cfg.Logf("capcom: quiet summary: %v", err)
	}
}

// quietReseed asks for a re-seed after reseed_quiet with no turn, when the last turn left the
// context above reseed_quiet_at (6.2).
func (l *Link) quietReseed() {
	lim := l.limits()
	l.mu.Lock()
	l.cx.reseedT = nil
	due := !l.cx.asked && l.st.Turn == "" && l.cx.lastContext > lim.ReseedQuietAt
	if due {
		l.cx.asked = true
	}
	l.mu.Unlock()
	if !due {
		return
	}
	if err := l.Reseed(ReseedQuiet); err != nil {
		l.mu.Lock()
		l.cx.asked = false
		l.mu.Unlock()
	}
}

// sessionStarted clears the re-seed mark: the fresh session's context is the seed alone.
func (l *Link) sessionStarted() {
	l.mu.Lock()
	l.cx.asked = false
	l.cx.lastContext = 0
	l.mu.Unlock()
}

// storeSummary keeps the engine's summary, cut to summary_max_chars, with the item it covers.
func (l *Link) storeSummary(text, through string) {
	text = cut(text, l.limits().SummaryMaxChars)
	l.cfg.Record.Update(func(r *Record) {
		r.Summary = text
		if through != "" {
			r.SummaryThrough = through
		}
	})
}

// summarised reports whether an item is summary material: what was said (6.1).
func summarised(it capcomthread.Item) bool {
	switch it.Kind {
	case capcomthread.KindMessage, capcomthread.KindAgent, capcomthread.KindRound:
		return true
	case capcomthread.KindNote:
		return it.Model
	}
	return false
}

// seedMessage reports whether an item is a message of the seed (6.2): the user's or Capcom's.
func seedMessage(it capcomthread.Item) bool { return it.Kind == capcomthread.KindMessage }

// contextFor builds a context message: the summary at hand, the items since it within
// summary_input_chars (newest kept), and for a re-seed the seed's parts within their caps.
func (l *Link) contextFor(purpose string) Message {
	lim := l.limits()
	rec := l.cfg.Record.Get()
	m := Message{Type: TContext, For: purpose, Summary: rec.Summary, Through: rec.SummaryThrough}
	items := l.cfg.Thread.After(rec.SummaryThrough, summarised)
	budget := lim.SummaryInputChars
	var recent []Recent
	for i := len(items) - 1; i >= 0 && budget > 0; i-- {
		r := recentOf(items[i])
		r.Text = cut(r.Text, budget)
		budget -= utf8.RuneCountInString(r.Text)
		recent = append(recent, r)
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	m.Recent = recent
	if len(items) > 0 {
		m.Through = items[len(items)-1].ID
	}
	if purpose == ForReseed {
		m.Seed = l.seed(lim)
	}
	return m
}

// seed builds the hab's parts of a seed: the Logbook part within seed_logbook_bytes, the last
// seed_messages messages with the oldest dropped first past seed_messages_chars, and what is
// still open.
func (l *Link) seed(lim ContextLimits) *Seed {
	s := &Seed{}
	if l.cfg.Logbook != nil && lim.SeedLogbookBytes > 0 {
		s.Logbook = cutBytes(l.cfg.Logbook(lim.SeedLogbookBytes), lim.SeedLogbookBytes)
	}
	if lim.SeedMessages > 0 && lim.SeedMessagesChars > 0 {
		msgs := l.cfg.Thread.Tail(lim.SeedMessages, seedMessage)
		total := 0
		for _, it := range msgs {
			total += utf8.RuneCountInString(it.Text)
		}
		for len(msgs) > 1 && total > lim.SeedMessagesChars {
			total -= utf8.RuneCountInString(msgs[0].Text)
			msgs = msgs[1:]
		}
		for _, it := range msgs {
			r := recentOf(it)
			r.Text = cut(r.Text, lim.SeedMessagesChars)
			s.Messages = append(s.Messages, r)
		}
	}
	for _, it := range l.cfg.Thread.After("", openCard) {
		line := fmt.Sprintf("%s card %s: %s", it.Card.Type, it.ID, oneLine(it.Text))
		if it.Card.Ref != "" {
			line = fmt.Sprintf("%s card %s (%s): %s", it.Card.Type, it.ID, it.Card.Ref, oneLine(it.Text))
		}
		if len(it.Card.Choices) > 0 {
			line += " [" + strings.Join(it.Card.Choices, ", ") + "]"
		}
		s.Open = append(s.Open, line)
	}
	return s
}

// openCard is a card that waits for an answer. A memory card is no question to the user.
func openCard(it capcomthread.Item) bool {
	return it.Kind == capcomthread.KindCard && it.Card != nil && it.Card.Type != capcomthread.CardMemory && it.Answer == nil &&
		len(it.Card.Choices) > 0
}

func recentOf(it capcomthread.Item) Recent {
	r := Recent{ID: it.ID, Kind: it.Kind, Time: it.Time, Text: it.Text}
	switch it.Author.Kind {
	case capcomthread.AuthorAgent:
		r.Author = it.Author.Address
	default:
		r.Author = it.Author.Kind
	}
	return r
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return cut(s, 200)
}

// cut keeps at most n characters of s.
func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// cutBytes keeps at most n bytes of s, on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
