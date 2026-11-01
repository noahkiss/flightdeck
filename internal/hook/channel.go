package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// ChannelPath is the route of channel calls from a pane: POST /channel/<token> (mesh.md 6.5).
// The token names the pane, so the pane's session is the caller.
const ChannelPath = "/channel/"

// ChannelMethods are the methods the route takes.
var ChannelMethods = []string{"channel.send", "channel.status", "channel.wait"}

// ChannelHandler answers one channel call from the pane. An error that is a *control.Error keeps
// its kind; any other is unavailable. written, when not nil, is called once with the result of
// writing the answer to the caller.
type ChannelHandler func(ctx context.Context, pane, method string, params json.RawMessage) (result any, written func(error), err error)

// maxChannelBody bounds one channel call: a 64 KiB message as JSON, with room to spare.
const maxChannelBody = 1 << 20

// channelWait bounds one call on the route: the longest channel wait plus a margin.
const channelWait = 610 * time.Second

type channelCall struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type channelAnswer struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *channelError   `json:"error,omitempty"`
}

type channelError struct {
	Kind    api.ErrorKind `json:"kind"`
	Message string        `json:"message"`
}

// SetChannels sets the handler of the channel route. With none, every call answers unavailable.
func (s *Server) SetChannels(h ChannelHandler) {
	s.mu.Lock()
	s.channels = h
	s.mu.Unlock()
}

func (s *Server) serveChannel(w http.ResponseWriter, r *http.Request, token string) {
	answer := func(a channelAnswer) error {
		b, _ := json.Marshal(a)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write(b)
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		return err
	}
	fail := func(kind api.ErrorKind, format string, args ...any) {
		_ = answer(channelAnswer{Error: &channelError{Kind: kind, Message: fmt.Sprintf(format, args...)}})
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChannelBody+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxChannelBody {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	pane, ok := s.reg.Pane(token)
	if !ok {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	var call channelCall
	if err := json.Unmarshal(body, &call); err != nil {
		fail(api.ErrorKindInvalid, "not a channel call: %v", err)
		return
	}
	known := false
	for _, m := range ChannelMethods {
		known = known || m == call.Method
	}
	if !known {
		fail(api.ErrorKindInvalid, "%q is not one of %v", call.Method, ChannelMethods)
		return
	}
	s.mu.Lock()
	h := s.channels
	s.mu.Unlock()
	if h == nil {
		fail(api.ErrorKindUnavailable, "%s needs channels, which this hab is running without", call.Method)
		return
	}
	if len(call.Params) == 0 {
		call.Params = json.RawMessage("{}")
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(channelWait + 10*time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), channelWait)
	defer cancel()
	res, written, err := h(ctx, pane, call.Method, call.Params)
	if written == nil {
		written = func(error) {}
	}
	if err != nil {
		written(err)
		var ce *control.Error
		if errors.As(err, &ce) {
			fail(ce.Data.Kind, "%s", ce.Message)
		} else {
			fail(api.ErrorKindUnavailable, "%v", err)
		}
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		written(err)
		fail(api.ErrorKindUnavailable, "%v", err)
		return
	}
	written(answer(channelAnswer{Result: b}))
}

// Call makes one channel call from a pane through the address in its GFF_HAB value, and decodes
// the result into result. An answer with an error returns a *control.Error of its kind.
func Call(ctx context.Context, addr, method string, params, result any) error {
	sock, token, err := ParseAddr(addr)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{method, params})
	if err != nil {
		return err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hab"+ChannelPath+token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChannelBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hab answered %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var a channelAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("hab answer: %w", err)
	}
	if a.Error != nil {
		return control.Errorf(a.Error.Kind, "%s", a.Error.Message)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(a.Result, result)
}
