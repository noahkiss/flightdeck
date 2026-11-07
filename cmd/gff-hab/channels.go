package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// channelPeers is the peer list as channels read it.
type channelPeers struct{ m *peers.Manager }

func (c channelPeers) List() api.PeerList { return c.m.List() }

func (c channelPeers) Links() []channels.Link {
	ls := c.m.Links()
	out := make([]channels.Link, 0, len(ls))
	for _, l := range ls {
		cl := channels.Link{Label: l.Label, DeviceID: l.DeviceID, Online: l.Online, Caps: l.Caps}
		if l.Conn != nil {
			cl.Caller = l.Conn
		}
		out = append(out, cl)
	}
	return out
}

// channelLocal is this hab's sessions and agents as channels need them. Delivery goes through
// session.send's own path (harness-adapters.md, Input delivery).
type channelLocal struct {
	store  *structure.Store
	agents *harness.Store
	send   *agentService // nil without the holder link
	trust  *trustService // nil without the holder link
	// lock is the round lock's in-use reason for a session, "" when free (rounds.md 5.2, 6.1).
	lock func(api.Session) string
	// capcom is the Capcom service: a message to its session goes into the thread
	// (capcom-service.md 2.4, 9.1). Nil: the service is off.
	capcom *capcomEngine
}

var _ channels.RoundLocal = (*channelLocal)(nil)

// CheckRound is Check under the round lock, for a message with the round mark (rounds.md 5.2):
// a session in use holds it with the lock's reason.
func (l *channelLocal) CheckRound(sessionID string) error {
	if l.service(sessionID) {
		return nil // the thread takes every message
	}
	ss, err := l.ready(sessionID)
	if err != nil {
		return err
	}
	if l.lock != nil {
		if why := l.lock(ss); why != "" {
			return fmt.Errorf("%w: %s", channels.ErrBusy, why)
		}
	}
	return nil
}

// service reports whether a session id is the running Capcom service's.
func (l *channelLocal) service(id string) bool {
	return l.capcom != nil && id != "" && l.capcom.Session() == id
}

func (l *channelLocal) Session(name string) (string, bool) {
	ss, ok := l.store.SessionByName(name)
	return ss.ID, ok
}

func (l *channelLocal) Name(id string) (string, bool) {
	ss, ok := l.store.Session(id)
	return ss.Name, ok
}

// ready checks, writing nothing, that the session's agent may take a message now.
func (l *channelLocal) ready(sessionID string) (api.Session, error) {
	ss, ok := l.store.Session(sessionID)
	if !ok {
		return ss, fmt.Errorf("session %s is gone", sessionID)
	}
	if l.trust != nil && ss.Pane != "" {
		switch declined, pending := l.trust.Declined(ss.Pane); {
		case declined:
			return ss, errUntrusted
		case pending:
			return ss, fmt.Errorf("%w: an answer to the trust prompt is on its way", channels.ErrBusy)
		}
	}
	if ss.State == api.SessionStateExited || ss.Pane == "" {
		return ss, errors.New("agent not running: the session has exited")
	}
	if l.send == nil {
		return ss, errors.New("this hab runs without its holder link")
	}
	st, ok := l.agents.Get(ss.Pane)
	if !ok || st.Revision == 0 {
		return ss, fmt.Errorf("%w for session %s", channels.ErrNoState, ss.Name)
	}
	switch st.State {
	case api.AgentStatusNone, api.AgentStatusExited:
		if st.Hooks == api.HookHealthMissing {
			return ss, fmt.Errorf("agent not running: %s reported no state, so its hooks are likely missing (gff-hab hook -print %s)", st.Harness, st.Harness)
		}
		return ss, fmt.Errorf("agent not running: the pane shows %s", st.State)
	}
	// opencode takes messages through its server (agentService.Send), only when gff-hab follows
	// the pane's server; the terminal path takes none.
	if _, ok := l.send.opencodeClient(ss.Pane, st.Harness); !ok {
		if _, err := harness.MessageWrites(st.Harness, "x"); errors.Is(err, harness.ErrUnsupported) {
			if st.Harness == detect.KindOpencode {
				return ss, errors.New("opencode takes messages only through its server, and gff-hab follows no opencode server for this pane; start opencode from Flightdeck")
			}
			return ss, fmt.Errorf("%s takes no messages yet", st.Harness)
		}
	}
	if _, err := l.agents.CheckSend(ss.Pane); err != nil {
		return ss, fmt.Errorf("%w: %v", channels.ErrBusy, err)
	}
	return ss, nil
}

func (l *channelLocal) Check(sessionID string) error {
	if l.service(sessionID) {
		return nil // the thread takes every message
	}
	_, err := l.ready(sessionID)
	return err
}

func (l *channelLocal) Deliver(ctx context.Context, sessionID, text string) error {
	if l.service(sessionID) {
		return l.capcom.DeliverAgent(text)
	}
	ss, err := l.ready(sessionID)
	if err != nil {
		return err
	}
	return deliverErr(l.send.Send(ctx, "channels", api.SessionSendParams{Session: ss.ID, Text: text}))
}

// deliverErr maps a session.send error to a channel outcome. A partial error (the paste is in
// the prompt but Enter was not sent, or an opencode server may have queued the prompt) is not
// delivered and not retried; a permanent one (an opencode server refused the prompt for good,
// or its session is gone) refuses the message; busy or away holds it; any other error refuses
// it.
func deliverErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errPermanent) {
		return err
	}
	var ce *control.Error
	if errors.As(err, &ce) {
		switch {
		case ce.IsPartial():
			return fmt.Errorf("%w: %s", channels.ErrNotDelivered, ce.Message)
		case ce.Data.Kind == api.ErrorKindConflict, ce.Data.Kind == api.ErrorKindUnavailable:
			// Busy, a message still starting its turn, or a pane whose holder is away.
			return fmt.Errorf("%w: %s", channels.ErrBusy, ce.Message)
		}
	}
	return err
}

func (l *channelLocal) Agent(session string) (api.AgentStatus, bool, <-chan struct{}) {
	ss, ok := l.store.SessionByName(session)
	if !ok {
		return "", false, nil
	}
	if l.service(ss.ID) {
		return api.AgentStatusIdle, true, nil
	}
	if ss.State == api.SessionStateExited || ss.Pane == "" {
		return api.AgentStatusExited, true, nil
	}
	ch := l.agents.Changed(ss.Pane)
	st, ok := l.agents.Get(ss.Pane)
	if !ok {
		return api.AgentStatusNone, true, ch
	}
	return st.State, true, ch
}

// channelService is what the pane route calls; *channels.Service is the one gff-hab runs.
type channelService interface {
	Send(context.Context, api.ChannelSendParams) (api.ChannelSent, error)
	Status(context.Context, api.ChannelStatusParams) (api.ChannelStatusResult, error)
	WaitTake(context.Context, api.ChannelWaitParams, time.Duration) (api.ChannelWaitResult, func(bool), error)
}

// channelHook answers channel calls from a pane (mesh.md 6.5): the pane's session sends and
// waits.
func channelHook(store *structure.Store, svc channelService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		ss, ok := store.SessionByPane(pane)
		if !ok {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "this pane is not a session; channels send and wait as a session")
		}
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		switch method {
		case "channel.send":
			var p api.ChannelSendParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			p.From = ss.Name // the pane's own session, whatever the params say
			r, err := svc.Send(ctx, p)
			return r, nil, err
		case "channel.status":
			var p api.ChannelStatusParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			r, err := svc.Status(ctx, p)
			return r, nil, err
		default: // channel.wait
			var p api.ChannelWaitParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := checkStates(p.For); err != nil {
				return nil, nil, err
			}
			p.Session = ss.Name // the pane's own session, whatever the params say
			// A message counts as delivered once the answer is written to the command.
			r, taken, err := svc.WaitTake(ctx, p, channels.PaneWait)
			if taken == nil {
				return r, nil, err
			}
			return r, func(werr error) { taken(werr == nil) }, err
		}
	}
}

func checkStates(ss []api.AgentStatus) error {
	for _, s := range ss {
		ok := false
		for _, w := range api.Enums["AgentStatus"] {
			ok = ok || string(s) == w
		}
		if !ok {
			return control.Errorf(api.ErrorKindInvalid, "state %q is not one of %v", s, api.Enums["AgentStatus"])
		}
	}
	return nil
}

// --- gff-hab send and gff-hab wait ---

// caller makes one call, through the control socket or the pane's hook endpoint.
type caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

type hookCaller struct{ addr string }

func (h hookCaller) Call(ctx context.Context, method string, params, result any) error {
	return hook.Call(ctx, h.addr, method, params, result)
}

// channelCaller picks the door (mesh.md 6.5): -socket or -via is the control connection, which
// needs the session named; else GFF_HAB, from a pane; else the default control socket.
// named says whether the session flag was given; missing is the error when the control
// connection needs it and it was not.
func channelCaller(ctx context.Context, sock, via string, named bool, flagName string, missing bool) (caller, func(), error) {
	if sock == "" && via == "" && os.Getenv(hook.Env) != "" {
		if named {
			return nil, nil, fmt.Errorf("-%s names a session for the control socket; from a pane the pane's own session is used", flagName)
		}
		return hookCaller{os.Getenv(hook.Env)}, func() {}, nil
	}
	if missing {
		return nil, nil, fmt.Errorf("outside a pane, give -%s: the session on this hab", flagName)
	}
	c, err := open(sock, via)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		c.Close()
		return nil, nil, err
	}
	c.KeepAlive(control.DefaultHeartbeat) // a wait outlasts the server's idle timeout
	return c, func() { c.Close() }, nil
}

// sendCmd is `gff-hab send`: send a message to an agent by address.
func sendCmd(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	from := fs.String("from", "", "the sending session on this hab, by name or id (outside a pane only)")
	replyTo := fs.String("reply-to", "", "the id of the message this one answers")
	wait := fs.Duration("wait", 0, "wait up to this long for delivered or another final state")
	fs.Parse(args)
	if fs.NArg() < 2 {
		return errors.New("send needs an address (label/name or name) and the text, or - to read it from stdin")
	}
	text := strings.Join(fs.Args()[1:], " ")
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, channels.MaxText+1))
		if err != nil {
			return err
		}
		text = string(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second+*wait)
	defer cancel()
	c, done, err := channelCaller(ctx, *sock, *via, *from != "", "from", *from == "")
	if err != nil {
		return err
	}
	defer done()
	var sent api.ChannelSent
	if err := c.Call(ctx, "channel.send", api.ChannelSendParams{To: fs.Arg(0), Text: text, From: *from, ReplyTo: *replyTo}, &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s %s %s\n", sent.ID, sent.State, sent.To)
	if *wait <= 0 {
		return nil
	}
	var st api.ChannelStatusResult
	if err := c.Call(ctx, "channel.status", api.ChannelStatusParams{ID: sent.ID, WaitMS: int(*wait / time.Millisecond)}, &st); err != nil {
		return err
	}
	r := st.Record
	if r.State == sent.State {
		return fmt.Errorf("message %s is still %s after %v", r.ID, r.State, *wait)
	}
	fmt.Fprintf(out, "%s %s %s\n", r.ID, r.State, r.Reason)
	if r.State != api.ChannelStateDelivered && channels.Final(r.State) {
		return fmt.Errorf("message %s %s: %s", r.ID, r.State, r.Reason)
	}
	return nil
}

// waitCmd is `gff-hab wait`: wait for a message to this session, or an agent's state.
func waitCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	session := fs.String("session", "", "wait for messages to this session on this hab (outside a pane only)")
	agent := fs.String("agent", "", "also wait for this agent to reach a state in -for: a session on this hab, or label/name (or a bare name) on a peer")
	forStates := fs.String("for", "", "comma-separated agent states that end the wait for -agent (default: any change)")
	timeout := fs.Duration("timeout", channels.DefaultWait, "give up after this long (default 110s from a pane, under a harness's 2 min tool limit)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return errors.New("wait takes flags only")
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "timeout" })
	*timeout = waitTimeout(*timeout, set, *sock != "" || *via != "", os.Getenv(hook.Env) != "")
	p := api.ChannelWaitParams{Session: *session, Agent: *agent, TimeoutMS: int(*timeout / time.Millisecond)}
	if *forStates != "" {
		for _, s := range strings.Split(*forStates, ",") {
			p.For = append(p.For, api.AgentStatus(strings.TrimSpace(s)))
		}
		if err := checkStates(p.For); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+30*time.Second)
	defer cancel()
	// On the control socket an agent wait alone needs no session.
	c, done, err := channelCaller(ctx, *sock, *via, *session != "", "session", *session == "" && *agent == "")
	if err != nil {
		return err
	}
	defer done()
	var r api.ChannelWaitResult
	if err := c.Call(ctx, "channel.wait", p, &r); err != nil {
		return err
	}
	if *asJSON {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", b)
	}
	switch {
	case r.Message != nil:
		if !*asJSON {
			fmt.Fprintf(out, "from %s (%s):\n%s\n", r.Message.From, r.Message.ID, r.Message.Text)
		}
	case r.Agent != nil:
		if !*asJSON {
			fmt.Fprintf(out, "%s %s\n", r.Agent.Session, r.Agent.State)
		}
	default:
		return fmt.Errorf("nothing within %v", *timeout)
	}
	return nil
}

// waitTimeout is the wait of gff-hab wait: the -timeout given, else 110 s from a pane (the hook
// door), under the 2 minute limit a harness puts on one tool command, else the flag's default.
func waitTimeout(flagValue time.Duration, set, control, inPane bool) time.Duration {
	if !set && !control && inPane {
		return channels.PaneWait
	}
	return flagValue
}
