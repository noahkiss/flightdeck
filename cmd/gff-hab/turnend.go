package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// session.turn_end (hab-logic-api.md 3.9): a pane's agent state left working or blocked for
// done or error. idle never ends a turn, because Codex reports idle at its start
// (harness-adapters.md, Codex). The event names the turn's final agent message when the chat
// reader finds it within turnEndRead; else it goes without one.

// turnEndRead bounds the read of a turn's final message: the transcript may trail the state.
const turnEndRead = 3 * time.Second

// turnEnds watches agent states and publishes session.turn_end.
type turnEnds struct {
	session func(pane string) (string, bool) // the Flightdeck session of a pane
	// read returns the session's latest chat messages, oldest first; nil reads nothing.
	read    func(ctx context.Context, session string) ([]api.ChatMessage, error)
	publish func(structure.Event)
	readFor time.Duration // 0: turnEndRead
	poll    time.Duration // 0: 200 ms

	mu      sync.Mutex
	started map[string]uint64 // per pane: the revision that began a turn; absent: no turn runs
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newTurnEnds(session func(string) (string, bool), read func(context.Context, string) ([]api.ChatMessage, error), publish func(structure.Event)) *turnEnds {
	ctx, cancel := context.WithCancel(context.Background())
	return &turnEnds{session: session, read: read, publish: publish, started: map[string]uint64{}, ctx: ctx, cancel: cancel}
}

// Close stops the reads on their way and waits for them.
func (t *turnEnds) Close() {
	t.cancel()
	t.wg.Wait()
}

// agentState takes one agent.state of any pane. The harness store calls it with its lock held,
// so it only takes t.mu and hands the read off its goroutine.
func (t *turnEnds) agentState(st api.AgentState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() != nil {
		return
	}
	switch st.State {
	case api.AgentStatusWorking, api.AgentStatusBlocked:
		if _, ok := t.started[st.Pane]; !ok {
			t.started[st.Pane] = st.Revision
		}
	case api.AgentStatusDone, api.AgentStatusError:
		if _, ok := t.started[st.Pane]; !ok {
			return
		}
		delete(t.started, st.Pane)
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.end(st)
		}()
	default: // idle, exited, none: the turn, if any, is forgotten
		delete(t.started, st.Pane)
	}
}

// end publishes one turn end, after a bounded read of its final message.
func (t *turnEnds) end(st api.AgentState) {
	sess, ok := t.session(st.Pane)
	if !ok {
		return
	}
	te := api.TurnEnd{Session: sess}
	if t.read != nil {
		wait := turnEndRead
		if t.readFor > 0 {
			wait = t.readFor
		}
		poll := 200 * time.Millisecond
		if t.poll > 0 {
			poll = t.poll
		}
		ctx, cancel := context.WithTimeout(t.ctx, wait)
		for {
			if msgs, err := t.read(ctx, sess); err == nil {
				if id, ok := finalMessageID(msgs); ok {
					te.FinalMessage = id
					break
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(poll):
				continue
			}
			break
		}
		cancel()
	}
	if t.ctx.Err() != nil {
		return
	}
	t.publish(structure.Event{Type: api.EventSessionTurnEnd, ID: sess, Data: te})
}

// finalMessageID is the id of the last top-level agent text after the last user message.
func finalMessageID(msgs []api.ChatMessage) (string, bool) {
	from := 0
	for i, m := range msgs {
		if m.Kind == api.ChatKindUser {
			from = i + 1
		}
	}
	for i := len(msgs) - 1; i >= from; i-- {
		m := msgs[i]
		if m.Kind == api.ChatKindAgent && m.Parent == "" && strings.TrimSpace(m.Text) != "" {
			return m.ID, true
		}
	}
	return "", false
}
