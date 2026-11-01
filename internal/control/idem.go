package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Idempotency keys on session.create and session.send (spec 3.9, Idempotency keys).
const (
	// MaxIdempotencyKey is the longest key the hab takes, in bytes.
	MaxIdempotencyKey = 128
	// idemKeep is how long the hab remembers a key whose call succeeded.
	idemKeep = 24 * time.Hour
	// idemMax is how many succeeded keys the hab remembers; past it the oldest goes first.
	idemMax = 4096
)

// idemTable remembers the keys of calls that succeeded, and the calls still running. It is
// shared by every connection: a client that reconnects may get a new client id.
type idemTable struct {
	mu    sync.Mutex
	calls map[string]*idemCall
	order []idemDone // succeeded calls, oldest first
}

type idemCall struct {
	fingerprint [32]byte
	done        chan struct{} // closed when the first call ends
	result      any
	err         *Error
}

type idemDone struct {
	id   string
	call *idemCall
	at   time.Time
}

// idem runs call once per method and key. A repeat while the first call runs waits for it and
// gets its answer. A repeat after it succeeded gets its result and runs nothing. A call that
// failed is forgotten, so a retry runs again. A key reused with other params is refused with
// conflict. An empty key runs call every time. params is the request with its key cleared.
func (s *Server) idem(ctx context.Context, method, key string, params any, call func() (any, *Error)) (any, *Error) {
	if key == "" {
		return call()
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, appErr(api.ErrorKindInvalid, "%s: params could not be encoded: %v", method, err)
	}
	fp := sha256.Sum256(b)
	id := method + "\x00" + key
	t := &s.idemKeys
	t.mu.Lock()
	t.pruneLocked(s.cfg.Now())
	if c, ok := t.calls[id]; ok {
		t.mu.Unlock()
		if c.fingerprint != fp {
			return nil, appErr(api.ErrorKindConflict, "idempotency key %q was used for a different %s; make a new key for each request", key, method)
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil, appErr(api.ErrorKindUnavailable, "the first %s with idempotency key %q is still running; try again", method, key)
		}
		return c.result, c.err
	}
	if t.calls == nil {
		t.calls = map[string]*idemCall{}
	}
	c := &idemCall{fingerprint: fp, done: make(chan struct{})}
	t.calls[id] = c
	t.mu.Unlock()

	result, e := call()

	t.mu.Lock()
	c.result, c.err = result, e
	if e != nil {
		delete(t.calls, id)
	} else {
		t.order = append(t.order, idemDone{id: id, call: c, at: s.cfg.Now()})
		t.pruneLocked(s.cfg.Now())
	}
	close(c.done)
	t.mu.Unlock()
	return result, e
}

// pruneLocked forgets succeeded keys older than idemKeep, and the oldest past idemMax.
func (t *idemTable) pruneLocked(now time.Time) {
	n := 0
	for n < len(t.order) && (len(t.order)-n > idemMax || now.Sub(t.order[n].at) >= idemKeep) {
		if d := t.order[n]; t.calls[d.id] == d.call {
			delete(t.calls, d.id)
		}
		n++
	}
	if n > 0 {
		t.order = append(t.order[:0:0], t.order[n:]...)
	}
}

// checkKey refuses a key longer than MaxIdempotencyKey.
func checkKey(method, key string) *Error {
	if len(key) > MaxIdempotencyKey {
		return appErr(api.ErrorKindInvalid, "%s: idempotency_key is %d bytes; at most %d", method, len(key), MaxIdempotencyKey)
	}
	return nil
}
