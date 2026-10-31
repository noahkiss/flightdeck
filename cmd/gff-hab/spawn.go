package main

import (
	"context"
	"crypto/rand"
	"strings"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/link"
)

// spawner starts programs in panes through the holder link. Each pane's environment gets
// GFF_HAB: the hook socket and the pane's token, so the hooks of an agent in the pane reach
// gff-hab and name the pane (harness-adapters.md, Hook endpoint). The API methods that create
// panes call it; until they exist, only tests do.
type spawner struct {
	lk       *link.Link
	tokens   *hook.Tokens
	hookSock string // absolute
}

// spawn starts sp.Argv in a new pane on the current holder. A zero sp.UUID gets a random one.
// Any GFF_HAB in sp.Env is replaced.
func (s *spawner) spawn(ctx context.Context, sp hp.Spawn) (link.PaneStatus, error) {
	if sp.UUID == (hp.UUID{}) {
		sp.UUID = newUUID()
	}
	sp.Env = paneEnv(sp.Env, hook.Addr(s.hookSock, s.tokens.Token(sp.UUID)))
	return s.lk.Spawn(ctx, sp)
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
// its holder, and not released. With no link it knows no pane.
func livePane(lk *link.Link) func([16]byte) (string, bool) {
	return func(u [16]byte) (string, bool) {
		if lk == nil {
			return "", false
		}
		st, ok := lk.Pane(hp.UUID(u))
		if !ok || st.Lost || st.Gone {
			return "", false
		}
		return st.UUID.String(), true
	}
}

// startupSpawns runs once gff-hab serves, when set. Only the gfftest build sets it
// (spawn_gfftest.go): the end-to-end tests spawn panes through it until the API has methods
// that create panes.
var startupSpawns func(s *spawner, logf func(string, ...any))
