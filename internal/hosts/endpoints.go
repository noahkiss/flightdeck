package hosts

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/noahkiss/flightdeck/internal/habconn"
)

// endpoints is the transport of a host with one or more endpoints, tried in the owner's order
// (mesh.md 3.1, 4.1). Every stream goes to the current endpoint. After a failed attempt the
// Conn calls Failed, and the next attempt tries the next endpoint, wrapping round; the Conn's
// backoff spaces the attempts. A working endpoint stays current until an attempt on it fails.
type endpoints struct {
	eps []Endpoint
	trs []habconn.Transport

	mu  sync.Mutex
	cur int
}

func newEndpoints(eps []Endpoint) (*endpoints, error) {
	if len(eps) == 0 {
		return nil, errors.New("no endpoint")
	}
	t := &endpoints{eps: eps}
	for _, ep := range eps {
		tr, err := ep.Transport()
		if err != nil {
			return nil, err
		}
		t.trs = append(t.trs, tr)
	}
	return t, nil
}

func (t *endpoints) at() (Endpoint, habconn.Transport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eps[t.cur], t.trs[t.cur]
}

func (t *endpoints) current() Endpoint {
	ep, _ := t.at()
	return ep
}

// Dial opens a stream to the current endpoint.
func (t *endpoints) Dial(ctx context.Context, kind habconn.Kind) (io.ReadWriteCloser, error) {
	_, tr := t.at()
	return tr.Dial(ctx, kind)
}

// Failed moves on to the next endpoint.
func (t *endpoints) Failed() {
	t.mu.Lock()
	t.cur = (t.cur + 1) % len(t.eps)
	t.mu.Unlock()
}

// Reset drops the current endpoint's shared connection, when it has one.
func (t *endpoints) Reset(ctx context.Context) error {
	_, tr := t.at()
	if r, ok := tr.(habconn.Resetter); ok {
		return r.Reset(ctx)
	}
	return nil
}
