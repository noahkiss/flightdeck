package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
	"github.com/noahkiss/flightdeck/internal/transcript"
)

// chatPoll is how often open transcripts are read for new lines, to send chat.message events.
const chatPoll = time.Second

// chatIdle is how long a reader stays open with no chat.page before the poll may close it.
const chatIdle = 10 * time.Minute

// chatService serves chat.page from each session's harness transcript, read in place
// (harness-adapters.md, Claude Code: conversation), and sends chat.message events for what the
// harness appends. A session's transcript is found through its pane's agent state: the native
// session id the hooks reported is the resume id, and names the file.
type chatService struct {
	chatConfig

	mu      sync.Mutex
	readers map[string]*chatReader // by gff-hab session id
	stop    chan struct{}
	done    chan struct{}
}

// chatConfig is what newChatService needs.
type chatConfig struct {
	session func(id string) (api.Session, bool)
	agent   func(pane string) (api.AgentState, bool)
	find    func(native string) (string, error) // the Claude Code transcript of a native session
	publish func(structure.Event)
	watched func(session string) bool // a subscription gets this session's events; nil: none does
	logf    func(string, ...any)
	every   time.Duration    // poll interval; default chatPoll
	idle    time.Duration    // default chatIdle
	now     func() time.Time // default time.Now
}

// chatReader is one session's transcript reader. Its fields change under chatService.mu.
type chatReader struct {
	native string
	r      *transcript.Reader // nil while the file is not written: the poll opens it when it is
	paged  time.Time          // the last chat.page
	size   int64              // bytes read, at the last poll
	grew   time.Time          // when size last changed, or when the reader was made
}

func newChatService(cfg chatConfig) *chatService {
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	if cfg.every <= 0 {
		cfg.every = chatPoll
	}
	if cfg.idle <= 0 {
		cfg.idle = chatIdle
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	c := &chatService{chatConfig: cfg, readers: map[string]*chatReader{}, stop: make(chan struct{}), done: make(chan struct{})}
	go c.poll()
	return c
}

// Close stops the poll.
func (c *chatService) Close() {
	close(c.stop)
	<-c.done
}

// poll runs one pass per interval.
func (c *chatService) poll() {
	defer close(c.done)
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		c.pass()
	}
}

// pass closes the readers no one needs, opens the transcripts that appeared, and reads every
// open transcript once.
func (c *chatService) pass() {
	type entry struct {
		id string
		cr *chatReader
		chatReader
	}
	c.mu.Lock()
	entries := make([]entry, 0, len(c.readers))
	for id, cr := range c.readers {
		entries = append(entries, entry{id, cr, *cr})
	}
	c.mu.Unlock()
	now := c.now()
	for _, e := range entries {
		if c.unneeded(e.id, e.chatReader, now) {
			c.mu.Lock()
			if c.readers[e.id] == e.cr {
				delete(c.readers, e.id)
			}
			c.mu.Unlock()
			continue
		}
		r := e.r
		if r == nil {
			path, err := c.find(e.native)
			if err != nil {
				continue // not written yet, or unreadable: the next pass tries again
			}
			c.mu.Lock()
			if c.readers[e.id] == e.cr && e.cr.r == nil {
				// The client saw an empty conversation: report everything the file holds.
				e.cr.r = c.open(e.id, path, true)
			}
			r = e.cr.r
			c.mu.Unlock()
		}
		if err := r.Refresh(); err != nil {
			c.logf("transcript: %v", err)
		}
		size := r.Size()
		c.mu.Lock()
		if size != e.cr.size {
			e.cr.size, e.cr.grew = size, now
		}
		c.mu.Unlock()
	}
}

// unneeded says the poll should close a reader: its session record is gone, or no chat.page
// came for the idle time and either no subscription gets the session's events, or the
// session's agent is gone and the file has not grown for the idle time. A client that pages
// again opens a new reader; its cursor still works, since cursors name file offsets.
func (c *chatService) unneeded(id string, cr chatReader, now time.Time) bool {
	sess, ok := c.session(id)
	if !ok {
		return true
	}
	if now.Sub(cr.paged) < c.idle {
		return false
	}
	if c.watched == nil || !c.watched(id) {
		return true
	}
	if sess.Pane != "" {
		if st, ok := c.agent(sess.Pane); ok && st.Session == cr.native {
			return false // the agent still runs: its next turn sends events to the subscriber
		}
	}
	return now.Sub(cr.grew) >= c.idle
}

func (c *chatService) open(id, path string, fromEmpty bool) *transcript.Reader {
	return transcript.NewClaude(transcript.Config{Path: path, Session: id, Logf: c.logf, OnChange: c.changed, FromEmpty: fromEmpty})
}

// reader returns the transcript reader of a gff-hab session, opening it on first use. A nil
// reader with no error means the transcript is not written yet; the poll opens it when it
// appears and sends its messages as events.
func (c *chatService) reader(id string) (*transcript.Reader, error) {
	sess, ok := c.session(id)
	if !ok {
		return nil, control.Errorf(api.ErrorKindNotFound, "no session %s", id)
	}
	var native string
	if sess.Pane != "" {
		if st, ok := c.agent(sess.Pane); ok && st.Session != "" {
			if st.Harness != "claude" {
				return nil, control.Errorf(api.ErrorKindUnavailable, "session %s runs %s, which has no transcript reader yet", id, st.Harness)
			}
			native = st.Session
		}
	}
	now := c.now()
	c.mu.Lock()
	cr := c.readers[id]
	if cr != nil && cr.r != nil && (native == "" || cr.native == native) {
		// With no agent (it left, or the pane is gone) the last conversation stays readable.
		cr.paged = now
		c.mu.Unlock()
		return cr.r, nil
	}
	c.mu.Unlock()
	if native == "" {
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s has no agent session yet: its harness has not reported one", id)
	}
	path, err := c.find(native)
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.readers[id]
	if cur != nil && cur.native == native && cur.r != nil { // the poll opened it meanwhile
		cur.paged = now
		return cur.r, nil
	}
	if errors.Is(err, transcript.ErrNoTranscript) {
		if cur == nil || cur.native != native {
			c.readers[id] = &chatReader{native: native, grew: now}
		}
		c.readers[id].paged = now
		return nil, nil
	}
	if err != nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", id, err)
	}
	fromEmpty := cur != nil && cur.native == native // a client paged it while it was not written
	r := c.open(id, path, fromEmpty)
	c.readers[id] = &chatReader{native: native, r: r, paged: now, grew: now}
	return r, nil
}

// changed sends one chat.message event per added or changed message.
func (c *chatService) changed(msgs []api.ChatMessage) {
	for _, m := range msgs {
		c.publish(structure.Event{Type: control.EventChatMessage, ID: m.ID, Data: api.NonNil(m)})
	}
}

// ChatPage implements control.Chat.
// The read stops when ctx ends; a later page goes on from where it stopped.
func (c *chatService) ChatPage(ctx context.Context, p api.ChatPageParams) (api.ChatMessages, error) {
	if err := ctx.Err(); err != nil {
		return api.ChatMessages{}, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", p.Session, err)
	}
	r, err := c.reader(p.Session)
	if err != nil {
		return api.ChatMessages{}, err
	}
	if r == nil {
		if p.Before != "" {
			return api.ChatMessages{}, control.Errorf(api.ErrorKindNotFound, "no message %s in session %s", p.Before, p.Session)
		}
		if p.After != "" {
			return api.ChatMessages{}, control.Errorf(api.ErrorKindConflict, "%v", transcript.ErrStaleCursor)
		}
		return api.ChatMessages{Messages: []api.ChatMessage{}}, nil
	}
	var out api.ChatMessages
	if p.After != "" {
		out.Messages, out.More, out.Cursor, err = r.Since(ctx, p.After, p.Limit)
	} else {
		out.Messages, out.More, out.Cursor, err = r.Page(ctx, p.Before, p.Limit)
	}
	switch {
	case errors.Is(err, transcript.ErrUnknownMessage):
		return api.ChatMessages{}, control.Errorf(api.ErrorKindNotFound, "no message %s in session %s", p.Before, p.Session)
	case errors.Is(err, transcript.ErrStaleCursor):
		return api.ChatMessages{}, control.Errorf(api.ErrorKindConflict, "%v", err)
	case err != nil:
		return api.ChatMessages{}, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", p.Session, err)
	}
	return out, nil
}
