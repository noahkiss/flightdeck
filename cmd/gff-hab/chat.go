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
	logf    func(string, ...any)
	every   time.Duration // poll interval; default chatPoll
}

type chatReader struct {
	native string
	r      *transcript.Reader
}

func newChatService(cfg chatConfig) *chatService {
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	if cfg.every <= 0 {
		cfg.every = chatPoll
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

// poll reads every open transcript once per interval. A reader whose session record is gone is
// dropped.
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
		c.mu.Lock()
		var rs []*transcript.Reader
		for id, cr := range c.readers {
			if _, ok := c.session(id); !ok {
				delete(c.readers, id)
				continue
			}
			rs = append(rs, cr.r)
		}
		c.mu.Unlock()
		for _, r := range rs {
			if err := r.Refresh(); err != nil {
				c.logf("transcript: %v", err)
			}
		}
	}
}

// reader returns the transcript reader of a gff-hab session, opening it on first use. A nil
// reader with no error means the transcript is not written yet.
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
	c.mu.Lock()
	defer c.mu.Unlock()
	cr := c.readers[id]
	if native == "" {
		if cr != nil { // the agent left, or the pane is gone: keep showing the last conversation
			return cr.r, nil
		}
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s has no agent session yet: its harness has not reported one", id)
	}
	if cr != nil && cr.native == native {
		return cr.r, nil
	}
	path, err := c.find(native)
	if errors.Is(err, transcript.ErrNoTranscript) {
		return nil, nil
	}
	if err != nil {
		return nil, control.Errorf(api.ErrorKindUnavailable, "session %s: %v", id, err)
	}
	r := transcript.NewClaude(transcript.Config{Path: path, Session: id, Logf: c.logf, OnChange: c.changed})
	c.readers[id] = &chatReader{native: native, r: r}
	return r, nil
}

// changed sends one chat.message event per added or changed message.
func (c *chatService) changed(msgs []api.ChatMessage) {
	for _, m := range msgs {
		c.publish(structure.Event{Type: control.EventChatMessage, ID: m.ID, Data: api.NonNil(m)})
	}
}

// ChatPage implements control.Chat.
func (c *chatService) ChatPage(_ context.Context, p api.ChatPageParams) (api.ChatMessages, error) {
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
		out.Messages, out.More, out.Cursor, err = r.Since(p.After, p.Limit)
	} else {
		out.Messages, out.More, out.Cursor, err = r.Page(p.Before, p.Limit)
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
