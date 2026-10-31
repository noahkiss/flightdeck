package hook

import (
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

// Sink receives each valid envelope for a known pane. *harness.Store's Hook method fits.
type Sink func(pane string, env harness.Envelope)

// Server answers POST /hooks/<token>. It validates the envelope, resolves the pane from the
// token and hands the envelope to the sink. It answers 204 on success, 400 on a bad envelope,
// 404 on an unknown token and 413 on a body over MaxBody. The hook command ignores the answer.
type Server struct {
	reg  *Registry
	sink Sink
	now  func() time.Time
	logf func(string, ...any)

	mu      sync.Mutex
	unknown map[string]bool // tokens already logged as unknown
	http    *http.Server
}

// NewServer returns a server. logf may be nil.
func NewServer(reg *Registry, sink Sink, logf func(string, ...any)) *Server {
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
	s.sink(pane, env)
	w.WriteHeader(http.StatusNoContent)
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
