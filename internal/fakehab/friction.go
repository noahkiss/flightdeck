package fakehab

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
)

// Friction returns the diag.friction calls the hab took, in order, with each caller's kind.
func (h *Hab) Friction() []FrictionCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]FrictionCall(nil), h.friction...)
}

// FrictionCall is one diag.friction the hab took.
type FrictionCall struct {
	Client api.ClientKind
	Params api.FrictionParams
}

type frictionAPI struct{ h *Hab }

func (f frictionAPI) Note(_ context.Context, client api.ClientKind, p api.FrictionParams) (api.FrictionNoted, error) {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	f.h.friction = append(f.h.friction, FrictionCall{Client: client, Params: p})
	return api.FrictionNoted{Trace: "hFAKETRACE01"}, nil
}
