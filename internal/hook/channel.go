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

// ChannelMethods are the channel methods the route takes.
var ChannelMethods = []string{"channel.send", "channel.status", "channel.wait"}

// ReadMethods are the read tool methods the route takes (capcom.md 4): the agent kit's MCP
// server and CLI in a pane reach the hab through GFF_HAB.
var ReadMethods = []string{"catalog.list", "peers.list", "transcript.list", "transcript.search", "transcript.read",
	"notice.get", "file.read"}

// LogbookMethods are the Logbook methods the route takes (logbook.md 10.3): the agent's reads
// and writes. The pane's token names the writer: the hab records its harness, session and host
// (2.5), and limits its writes (14). logbook.decide is never on the route: an agent does not
// decide a proposal.
var LogbookMethods = []string{"logbook.brief", "logbook.find", "logbook.get", "logbook.list", "logbook.ready", "logbook.history",
	"logbook.write", "logbook.resolve", "logbook.projects", "logbook.status"}

// ConfirmMethods are the confirmation methods the route takes (capcom.md 6.1): an agent asks
// for a start and reads where it stands. confirm.answer is never on the route: no tool answers
// a confirmation.
var ConfirmMethods = []string{"confirm.request", "confirm.get", "confirm.list"}

// RoundMethods are the round methods the route takes (rounds.md 13): an agent's `gff-hab round`
// from its pane. The pane's session is the caller: it fills created_by and target self.
var RoundMethods = []string{"round.list", "round.get", "round.create", "round.update", "round.delete",
	"round.pause", "round.unpause", "round.run", "round.runs", "round.watch_event"}

// GroupMethods are the group methods the route takes (groups.md 15.2): the coordinator's and
// the members' tools. The pane's session is the caller: the coordinator of a new group, the
// sender of a report or a note, the lead that submits, the reviewer with its verdict.
// group.pause and group.resume are not on the route: the user pauses a group. group.message is
// not on it either: only a home hab sends it, over the link.
var GroupMethods = []string{"group.create", "group.amend", "group.get", "group.list", "group.report", "group.note",
	"group.close", "group.retry", "member.compact", "member.handoff", "member.action", "lane.submit", "review.verdict"}

// AgentMethods are the methods an agent calls about itself (harness-adapters.md, Self-compact).
// The pane's token names the agent: it can act only on its own pane.
var AgentMethods = []string{"agent.compact"}

// PersonalMethods are Capcom's personal tools (capcom-service.md 8, E12). The route takes them
// from the Capcom engine's token only; a pane's call is refused.
var PersonalMethods = []string{"personal.location", "personal.calendar", "personal.weather", "personal.places", "personal.place",
	"personal.calendar_add", "personal.reminder_add"}

// ReadHandler answers one read tool call from a pane. An error that is a *control.Error keeps
// its kind; any other is unavailable.
type ReadHandler func(ctx context.Context, method string, params json.RawMessage) (result any, err error)

// ChannelHandler answers one channel call from the pane. An error that is a *control.Error keeps
// its kind; any other is unavailable. written, when not nil, is called once with the result of
// writing the answer to the caller.
type ChannelHandler func(ctx context.Context, pane, method string, params json.RawMessage) (result any, written func(error), err error)

// maxChannelBody bounds one channel call: a 64 KiB message as JSON, with room to spare.
const maxChannelBody = 1 << 20

// readWait bounds one read call on the route: the per-host budget, the forward's grace and a
// margin.
const readWait = 30 * time.Second

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

// SetReads sets the handler of the read methods on the route. With none, each answers
// unavailable.
func (s *Server) SetReads(h ReadHandler) {
	s.mu.Lock()
	s.reads = h
	s.mu.Unlock()
}

// SetConfirm sets the handler of the confirmation methods on the route. With none, each answers
// unavailable.
func (s *Server) SetConfirm(h ChannelHandler) {
	s.mu.Lock()
	s.confirm = h
	s.mu.Unlock()
}

// SetRounds sets the handler of the round methods on the route. With none, each answers
// unavailable.
func (s *Server) SetRounds(h ChannelHandler) {
	s.mu.Lock()
	s.rounds = h
	s.mu.Unlock()
}

// SetLogbook sets the handler of the Logbook methods on the route. With none, each answers
// unavailable.
func (s *Server) SetLogbook(h ChannelHandler) {
	s.mu.Lock()
	s.logbook = h
	s.mu.Unlock()
}

// SetGroups sets the handler of the group methods on the route. With none, each answers
// unavailable.
func (s *Server) SetGroups(h ChannelHandler) {
	s.mu.Lock()
	s.groups = h
	s.mu.Unlock()
}

// SetAgent sets the handler of the agent methods on the route. With none, each answers
// unavailable.
func (s *Server) SetAgent(h ChannelHandler) {
	s.mu.Lock()
	s.agent = h
	s.mu.Unlock()
}

// SetPersonal sets the handler of the personal tools on the route. With none, each answers
// unavailable. The handler gets the route id and refuses any but the Capcom engine's.
func (s *Server) SetPersonal(h ChannelHandler) {
	s.mu.Lock()
	s.personal = h
	s.mu.Unlock()
}

// SetGuard sets a check every non-read call on the route passes first, with the caller's pane
// and the method: an error refuses the call with its kind. The Capcom service refuses an event
// turn's acting calls with it (capcom-service.md 9.4).
func (s *Server) SetGuard(g func(pane, method string) error) {
	s.mu.Lock()
	s.guard = g
	s.mu.Unlock()
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
	known, read, conf, round, lb, ag, grp, pers := false, false, false, false, false, false, false, false
	for _, m := range PersonalMethods {
		pers = pers || m == call.Method
	}
	for _, m := range ChannelMethods {
		known = known || m == call.Method
	}
	for _, m := range ConfirmMethods {
		conf = conf || m == call.Method
	}
	for _, m := range RoundMethods {
		round = round || m == call.Method
	}
	for _, m := range ReadMethods {
		read = read || m == call.Method
	}
	for _, m := range LogbookMethods {
		lb = lb || m == call.Method
	}
	for _, m := range AgentMethods {
		ag = ag || m == call.Method
	}
	for _, m := range GroupMethods {
		grp = grp || m == call.Method
	}
	if read {
		s.mu.Lock()
		rh := s.reads
		s.mu.Unlock()
		if rh == nil {
			fail(api.ErrorKindUnavailable, "%s needs the read tools, which this hab is running without", call.Method)
			return
		}
		if len(call.Params) == 0 {
			call.Params = json.RawMessage("{}")
		}
		ctx, cancel := context.WithTimeout(r.Context(), readWait)
		defer cancel()
		res, err := rh(ctx, call.Method, call.Params)
		if err != nil {
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
			fail(api.ErrorKindUnavailable, "%v", err)
			return
		}
		_ = answer(channelAnswer{Result: b})
		return
	}
	if call.Method == "logbook.decide" {
		fail(api.ErrorKindInvalid, "only a person decides a proposal (logbook.md 9.2): logbook.decide is not on a pane's route. The user approves or rejects it with gff lb approve or reject, run outside a pane, or in the app")
		return
	}
	if !known && !conf && !round && !lb && !ag && !grp && !pers {
		fail(api.ErrorKindInvalid, "%q is not one of %v, %v, %v, %v, %v, %v or %v", call.Method, ChannelMethods, ReadMethods, ConfirmMethods, RoundMethods, LogbookMethods, AgentMethods, GroupMethods)
		return
	}
	s.mu.Lock()
	guard := s.guard
	s.mu.Unlock()
	if guard != nil {
		if err := guard(pane, call.Method); err != nil {
			var ce *control.Error
			if errors.As(err, &ce) {
				fail(ce.Data.Kind, "%s", ce.Message)
			} else {
				fail(api.ErrorKindDenied, "%v", err)
			}
			return
		}
	}
	s.mu.Lock()
	h := s.channels
	switch {
	case conf:
		h = s.confirm
	case round:
		h = s.rounds
	case lb:
		h = s.logbook
	case ag:
		h = s.agent
	case grp:
		h = s.groups
	case pers:
		h = s.personal
	}
	s.mu.Unlock()
	if h == nil && pers {
		fail(api.ErrorKindUnavailable, "%s is a Capcom personal tool, served only on the Capcom service's route (capcom-service.md 8)", call.Method)
		return
	}
	if h == nil && lb {
		fail(api.ErrorKindUnavailable, "%s needs Logbook, which this hab is running without", call.Method)
		return
	}
	if h == nil {
		fail(api.ErrorKindUnavailable, "%s needs a service this hab is running without", call.Method)
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

// StatusError is an answer from the hab that is not 200: an unknown token, a body too large. The
// hab was reached and refused the call, so a caller never treats it as a hab that is down.
type StatusError struct {
	Status string
	Code   int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("hab answered %s: %s", e.Status, e.Body) }

// Call makes one channel call from a pane through the address in its GFF_HAB value, and decodes
// the result into result. An answer with an error returns a *control.Error of its kind; an HTTP
// status other than 200 a *StatusError. Only an error from dialing the socket means the hab was
// not reached.
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
		return &StatusError{Status: resp.Status, Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
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
