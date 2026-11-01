package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// MaxBody bounds one posted envelope. Tool inputs (a file being written) can be large.
const MaxBody = 8 << 20

// Sink receives each valid envelope for a known pane. It returns the id of the permission
// request the envelope opened, or "" when it opened none (*harness.Store's Hook gives it).
type Sink func(pane string, env harness.Envelope) (opened string)

// Holder decides whether a hook posted with ?wait=1 waits for an answer. It must call sink once,
// which hands the envelope to the server's Sink and returns what that returned, and register its
// wait in the same step, before the state the sink published can reach a client: an answer that
// comes at once then finds the wait. It returns nil to answer at once; else a Wait.
type Holder func(pane string, env harness.Envelope, sink func() (opened string)) Wait

// Wait waits, within ctx, and returns the answer body (a JSON object), or nil for no answer.
// With a body, the server calls written once with the result of sending it to the hook.
type Wait func(ctx context.Context) (answer []byte, written func(error))

// Resolver names the pane a token belongs to. *Tokens is one.
type Resolver interface {
	Pane(token string) (pane string, ok bool)
}

// Server answers POST /hooks/<token>. It validates the envelope, resolves the pane from the
// token and hands the envelope to the sink. It answers 204 on success, 400 on a bad envelope,
// 404 on an unknown token and 413 on a body over MaxBody. The hook command ignores the answer.
type Server struct {
	reg  Resolver
	sink Sink
	now  func() time.Time
	logf func(string, ...any)

	mu      sync.Mutex
	unknown map[string]bool // tokens already logged as unknown
	holder  Holder
	http    *http.Server
}

// SetHolder sets the holder for posts with ?wait=1. With none, they answer at once like any post.
func (s *Server) SetHolder(h Holder) {
	s.mu.Lock()
	s.holder = h
	s.mu.Unlock()
}

// NewServer returns a server. logf may be nil.
func NewServer(reg Resolver, sink Sink, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{reg: reg, sink: sink, now: time.Now, logf: logf, unknown: map[string]bool{}}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	return s
}

// Serve accepts on l, a Unix listener from control.Listen, until Close. Callers running as
// another user are refused. It returns nil after Close.
func (s *Server) Serve(l net.Listener) error {
	err := s.http.Serve(peerListener{l})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the server and closes its listener.
func (s *Server) Close() error { return s.http.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, "/hooks/")
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "POST /hooks/<token>", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > MaxBody {
		s.logf("hook: dropped an envelope over %d bytes", MaxBody)
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	pane, ok := s.reg.Pane(token)
	if !ok {
		s.mu.Lock()
		first := !s.unknown[token]
		if len(s.unknown) >= 1024 {
			clear(s.unknown) // bound the memory; a token may then be logged twice
		}
		s.unknown[token] = true
		s.mu.Unlock()
		if first {
			s.logf("hook: unknown pane token %s…", token[:min(8, len(token))])
		}
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	var env harness.Envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Harness == "" || env.Event == "" || !json.Valid(env.Payload) {
		s.logf("hook: bad envelope for pane %s: %v", pane, err)
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	if env.T <= 0 {
		env.T = s.now().UnixMilli()
	}
	s.mu.Lock()
	holder := s.holder
	s.mu.Unlock()
	if r.URL.Query().Get("wait") != "1" || holder == nil {
		s.sink(pane, env)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sunk := false
	wait := holder(pane, env, func() string {
		if sunk {
			return ""
		}
		sunk = true
		return s.sink(pane, env)
	})
	if !sunk {
		s.sink(pane, env)
	}
	if wait == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 200 at once tells the hook to wait for the body; its header timeout stays short, so a
	// hung hab still costs a hook no more than Budget.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(harness.MaxPermissionWait + 10*time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	answer, written := wait(r.Context())
	if answer == nil {
		answer = []byte("{}")
	}
	_, err = w.Write(answer)
	if err == nil {
		err = rc.Flush()
	}
	if written != nil {
		written(err)
	}
}

// peerListener refuses Unix connections from other users.
type peerListener struct{ net.Listener }

func (l peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uc, ok := c.(*net.UnixConn); ok {
			if err := control.CheckPeer(uc); err != nil {
				c.Close()
				continue
			}
		}
		return c, nil
	}
}
