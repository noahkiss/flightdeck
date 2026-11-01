package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/internal/holderclient"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
)

// spawner starts programs in panes through the holder link. Each pane's environment gets
// GFF_HAB: the hook socket and the pane's token, so the hooks of an agent in the pane reach
// gff-hab and name the pane (harness-adapters.md, Hook endpoint). session.create calls it
// (actions.go).
type spawner struct {
	lk       *link.Link
	tokens   *hook.Tokens
	hookSock string    // absolute
	spawning *spawning // the tokens' known set includes it (livePane)
	// run starts the pane; nil is lk.Spawn. has reports whether a pane with the uuid exists;
	// nil asks the link and its holders (paneExists). Unit tests replace both.
	run func(context.Context, hp.Spawn) (link.PaneStatus, error)
	has func(hp.UUID) bool
}

// spawn starts sp.Argv in a new pane on the current holder. A zero sp.UUID gets a random one.
// Any GFF_HAB in sp.Env is replaced. While the spawn runs, the pane's token is known, so a
// program that calls back before the link has the holder's answer is not refused.
func (s *spawner) spawn(ctx context.Context, sp hp.Spawn) (link.PaneStatus, error) {
	if sp.UUID == (hp.UUID{}) {
		sp.UUID = newUUID()
	}
	sp.Env = paneEnv(sp.Env, hook.Addr(s.hookSock, s.tokens.Token(sp.UUID)))
	if s.spawning != nil {
		s.spawning.add(sp.UUID)
		defer s.spawning.done(sp.UUID)
	}
	if s.run != nil {
		return s.run(ctx, sp)
	}
	return s.lk.Spawn(ctx, sp)
}

// exists reports whether a pane with uuid u exists after a spawn error: the link follows it,
// or a holder lists it.
func (s *spawner) exists(u hp.UUID) bool {
	if s.has != nil {
		return s.has(u)
	}
	if s.lk == nil {
		return false
	}
	if _, ok := s.lk.Pane(u); ok {
		return true
	}
	return s.lk.HolderHas(u)
}

// spawnRefused reports whether a spawn error is a definite refusal, after which no pane runs: the
// holder answered spawn-failed, no holder runs, or the uuid is taken. Any other error (a
// timeout, a closed link or connection) leaves open whether the holder started the program.
func spawnRefused(err error) bool {
	var re *holderclient.RequestError
	return errors.As(err, &re) || errors.Is(err, holderclient.ErrNoHolder) || errors.Is(err, link.ErrUUIDUsed)
}

// spawning holds the uuids of panes whose spawn is in flight. The holder starts the program
// before it answers the spawn, and the link follows the pane only once it has that answer; in
// between the program may already call back with its token.
type spawning struct {
	mu sync.Mutex
	m  map[hp.UUID]int
}

func newSpawning() *spawning { return &spawning{m: map[hp.UUID]int{}} }

func (s *spawning) add(u hp.UUID) {
	s.mu.Lock()
	s.m[u]++
	s.mu.Unlock()
}

func (s *spawning) done(u hp.UUID) {
	s.mu.Lock()
	if s.m[u]--; s.m[u] <= 0 {
		delete(s.m, u)
	}
	s.mu.Unlock()
}

func (s *spawning) has(u hp.UUID) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[u] > 0
}

// paneEnv returns env without any GFF_HAB entry, plus GFF_HAB=addr.
func paneEnv(env []string, addr string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, hook.Env+"=") {
			out = append(out, kv)
		}
	}
	return append(out, hook.Env+"="+addr)
}

// newUUID returns a random (version 4) uuid.
func newUUID() hp.UUID {
	var u hp.UUID
	rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// livePane resolves a token's uuid to a pane id while the link follows the pane: not lost with
// its holder, and not released. A pane the link does not know yet is live while its spawn is in
// flight (st, which may be nil). With no link it knows no pane.
func livePane(lk *link.Link, st *spawning) func([16]byte) (string, bool) {
	if lk == nil {
		return func([16]byte) (string, bool) { return "", false }
	}
	return paneLive(lk.Pane, st)
}

// paneLive is livePane over a pane lookup. The link's answer wins: a pane it reports lost or
// gone is refused even while its uuid is in st.
func paneLive(pane func(hp.UUID) (link.PaneStatus, bool), st *spawning) func([16]byte) (string, bool) {
	return func(u [16]byte) (string, bool) {
		ps, ok := pane(hp.UUID(u))
		if !ok {
			if st.has(hp.UUID(u)) {
				return hp.UUID(u).String(), true
			}
			return "", false
		}
		if ps.Lost || ps.Gone {
			return "", false
		}
		return ps.UUID.String(), true
	}
}

// livePanes counts the panes the link follows now: not lost and not gone.
func livePanes(lk *link.Link) int {
	if lk == nil {
		return 0
	}
	n := 0
	for _, st := range lk.Panes() {
		if !st.Lost && !st.Gone {
			n++
		}
	}
	return n
}

// newHookKeyWarning returns the line gff-hab logs when it made a new hook key while live panes
// exist, or "". Those panes got their tokens under the old key, which is gone, so their hooks are
// refused from now on and they show no agent state until they are respawned.
func newHookKeyWarning(created bool, live int) string {
	if !created || live == 0 {
		return ""
	}
	return fmt.Sprintf("WARNING: made a new %s while %d live pane(s) exist: their hook tokens no longer match, so their hooks are refused and they show no agent state until respawned", hook.KeyFile, live)
}
