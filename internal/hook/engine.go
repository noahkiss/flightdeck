package hook

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// EnginePath is the Capcom engine's route on the hook socket: POST /capcom/<token> with an
// HTTP upgrade to EngineProtocol, then one JSON object per line both ways
// (capcom-service.md 3.3, 4.4).
const EnginePath = "/capcom/"

// EngineProtocol is the upgrade the engine route takes.
const EngineProtocol = "gff-capcom/1"

// EngineHandler takes the engine's connections. Accept reports whether a token is the engine's
// token now; Serve runs one upgraded connection until it closes.
type EngineHandler interface {
	Accept(token string) bool
	Serve(conn io.ReadWriteCloser) error
}

// SetEngine sets the handler of the engine route. With none, the route answers 404.
func (s *Server) SetEngine(h EngineHandler) {
	s.mu.Lock()
	s.engine = h
	s.mu.Unlock()
}

func (s *Server) serveEngine(w http.ResponseWriter, r *http.Request, token string) {
	s.mu.Lock()
	h := s.engine
	s.mu.Unlock()
	if h == nil || !validToken(token) || !h.Accept(token) {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), EngineProtocol) || !headerHas(r.Header, "Connection", "upgrade") {
		http.Error(w, "upgrade to "+EngineProtocol, http.StatusUpgradeRequired)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{}) // the server's read and write timeouts are for hook posts
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + EngineProtocol + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		c.Close()
		return
	}
	go func() {
		if err := h.Serve(&upgraded{Conn: c, r: brw.Reader}); err != nil {
			s.logf("capcom engine route: %v", err)
		}
	}()
}

func headerHas(h http.Header, key, word string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), word) {
				return true
			}
		}
	}
	return false
}

// upgraded reads through the buffer the HTTP server filled, then the connection.
type upgraded struct {
	net.Conn
	r *bufio.Reader
}

func (u *upgraded) Read(p []byte) (int, error) { return u.r.Read(p) }
