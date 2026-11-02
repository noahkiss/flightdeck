package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/attach"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Attached context for Capcom (capcom.md 8.1): capcom.attach and capcom.open's context resolve
// each ref on this hab when attached and keep the result in capcom.json; the next session.send
// to the session delivers it as blocks before the user's text, once.

// capcomResolvers are what the context kinds read. Each may be nil: its kind then answers
// unavailable.
type capcomResolvers struct {
	// notice reads a notice by id on this hab or a peer (readtools' notice.get).
	notice func(ctx context.Context, id, host string) (api.NoticeRecord, error)
	// command reads one console command of a pane.
	command func(pane string, id uint64) (api.Command, error)
	// attachment reads one stored attachment of a session.
	attachment func(session, id string) (attach.Record, error)
	// agent is a pane's agent state.
	agent func(pane string) (api.AgentState, bool)
}

// Attach answers capcom.attach.
func (c *capcomService) Attach(ctx context.Context, p api.CapcomAttachParams) (api.CapcomAttached, error) {
	c.mu.Lock()
	rec, ok := c.recordLocked(p.Session)
	c.mu.Unlock()
	if !ok {
		return api.CapcomAttached{}, control.Errorf(api.ErrorKindInvalid,
			"session %s is not a Capcom session on this hab; context attaches only to a Capcom conversation", p.Session)
	}
	it, err := c.resolve(ctx, *rec, api.CapcomContextRef{Kind: p.Kind, Ref: p.Ref, Host: p.Host})
	if err != nil {
		return api.CapcomAttached{}, err
	}
	pending, err := c.addPending(p.Session, []capcom.Item{it})
	if err != nil {
		return api.CapcomAttached{}, err
	}
	return api.CapcomAttached{Session: p.Session, Pending: pending}, nil
}

// addPending appends items to the session's pending context and saves capcom.json. Items past
// the cap refuse the whole call, and nothing is added.
func (c *capcomService) addPending(session string, items []capcom.Item) ([]api.CapcomContextItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.st.Sessions {
		rec := &c.st.Sessions[i]
		if rec.ID != session {
			continue
		}
		pending := rec.Pending
		for _, it := range items {
			var err error
			if pending, err = capcom.Add(pending, it); err != nil {
				return nil, control.Errorf(api.ErrorKindConflict, "session %s: %v", session, err)
			}
		}
		if len(pending) != len(rec.Pending) {
			// Saved before it counts: a failed save keeps nothing, so a restart never loses
			// an item the client was told is waiting.
			old := rec.Pending
			rec.Pending = pending
			if err := c.trySaveLocked(); err != nil {
				rec.Pending = old
				c.logf("capcom: save: %v", err)
				return nil, control.Errorf(api.ErrorKindUnavailable, "session %s: the context could not be saved (%v); nothing was attached", session, err)
			}
		}
		return apiPending(pending), nil
	}
	return nil, control.Errorf(api.ErrorKindNotFound, "session %s is no longer a Capcom session on this hab", session)
}

// pending returns the context waiting for a Capcom session's next message.
func (c *capcomService) pending(session string) []capcom.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.recordLocked(session); ok {
		return rec.Pending
	}
	return nil
}

// delivered drops the items a message carried; an item attached meanwhile stays.
func (c *capcomService) delivered(session string, sent []capcom.Item) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.st.Sessions {
		if rec := &c.st.Sessions[i]; rec.ID == session {
			rec.Pending = capcom.Drop(rec.Pending, sent)
			c.saveLocked()
			return
		}
	}
}

func apiPending(items []capcom.Item) []api.CapcomContextItem {
	out := make([]api.CapcomContextItem, 0, len(items))
	for _, it := range items {
		out = append(out, api.CapcomContextItem{Kind: api.CapcomContextKind(it.Kind), Ref: it.Ref, Host: it.Host, Title: it.Title})
	}
	return out
}

// ownHost reports whether host names this hab: absent, self, its label or its device id.
func (c *capcomService) ownHost(host string) bool {
	return host == "" || host == "self" || host == c.host || host == c.self
}

// resolve looks a ref up on this hab, or a notice on a peer, and builds its block. The client
// gives only the kind, the ref and the host; every word in the block comes from the hab's own
// records.
func (c *capcomService) resolve(ctx context.Context, rec capcom.Session, r api.CapcomContextRef) (capcom.Item, error) {
	if strings.TrimSpace(r.Ref) == "" {
		return capcom.Item{}, control.Errorf(api.ErrorKindInvalid, "%s context needs a ref", r.Kind)
	}
	kind := string(r.Kind)
	if kind != capcom.KindNotification && kind != capcom.KindLogbook && !c.ownHost(r.Host) {
		// Only notice.get reads a peer's records today; a peer's session, file or command needs
		// a read path of its own (capcom.md 8.4).
		return capcom.Item{}, control.Errorf(api.ErrorKindInvalid,
			"%s context names this hab's own %ss only; host %q is another host. Open Capcom's context from that host's notification instead", kind, kind, r.Host)
	}
	it := capcom.Item{Kind: kind, Ref: r.Ref, Host: c.host}
	switch kind {
	case capcom.KindNotification:
		return c.resolveNotice(ctx, it, r.Host)
	case capcom.KindSession:
		return c.resolveSession(it)
	case capcom.KindFile:
		return c.resolveFile(rec, it)
	case capcom.KindCommand:
		return c.resolveCommand(it)
	case capcom.KindLogbook:
		return capcom.Item{}, control.Errorf(api.ErrorKindUnavailable,
			"a Logbook record cannot be attached yet: this hab has no Logbook, and nothing was attached")
	}
	return capcom.Item{}, control.Errorf(api.ErrorKindInvalid, "kind %q is not one of %v", r.Kind, api.Enums["CapcomContextKind"])
}

func (c *capcomService) resolveNotice(ctx context.Context, it capcom.Item, host string) (capcom.Item, error) {
	if c.res.notice == nil {
		return it, control.Errorf(api.ErrorKindUnavailable, "this hab cannot read notices; nothing was attached")
	}
	n, err := c.res.notice(ctx, it.Ref, host)
	if err != nil {
		return it, err
	}
	it.Host = n.Host
	it.Title = capcom.Title(n.Notice.Title)
	it.Body = []capcom.Field{
		{Name: "title", Value: capcom.Cut(n.Notice.Title, capcom.MaxField)},
		{Name: "body", Value: capcom.Cut(n.Notice.Body, capcom.MaxField)},
		{Name: "level", Value: string(n.Notice.Level)},
		{Name: "raised", Value: n.Notice.At},
	}
	if n.Session != nil {
		it.Body = append(it.Body, capcom.Field{Name: "session", Value: n.Session.Name})
	}
	it.Hint = fmt.Sprintf("Read it with notice_read id=%s host=%s.", it.Ref, it.Host)
	return it, nil
}

func (c *capcomService) resolveSession(it capcom.Item) (capcom.Item, error) {
	ss, ok := c.store.Session(it.Ref)
	if !ok {
		return it, control.Errorf(api.ErrorKindNotFound, "session %s does not exist on %s", it.Ref, c.host)
	}
	state := string(ss.State)
	switch {
	case ss.Exit != nil && ss.Exit.Code != nil:
		state = fmt.Sprintf("%s (status %d)", state, *ss.Exit.Code)
	case ss.Exit != nil && ss.Exit.Signal != "":
		state = fmt.Sprintf("%s (signal %s)", state, ss.Exit.Signal)
	}
	it.Title = capcom.Title(ss.Name)
	it.Body = []capcom.Field{{Name: "name", Value: ss.Name}, {Name: "state", Value: state}}
	if ss.Pane != "" && c.res.agent != nil {
		if st, ok := c.res.agent(ss.Pane); ok && st.State != api.AgentStatusNone {
			agent := st.Harness + ", " + string(st.State)
			if st.Kind != "" {
				agent += " (" + string(st.Kind) + ")"
			}
			it.Body = append(it.Body, capcom.Field{Name: "agent", Value: agent})
		}
	}
	if ss.Title != "" {
		it.Body = append(it.Body, capcom.Field{Name: "title", Value: capcom.Cut(ss.Title, capcom.MaxField)})
	}
	it.Hint = fmt.Sprintf("Read its transcript with transcript_read session=%s host=%s; agents shows its state now.", ss.ID, c.host)
	return it, nil
}

// resolveFile takes an attachment of the Capcom session, or an absolute path under its folder
// once links are resolved, that names a regular file.
func (c *capcomService) resolveFile(rec capcom.Session, it capcom.Item) (capcom.Item, error) {
	var path, name, typ string
	if filepath.IsAbs(it.Ref) {
		path = it.Ref
	} else {
		if c.res.attachment == nil {
			return it, control.Errorf(api.ErrorKindUnavailable, "this hab has no attachment store; name a path under the Capcom folder")
		}
		r, err := c.res.attachment(rec.ID, it.Ref)
		if err != nil {
			return it, err
		}
		path, name, typ = r.Path, r.Name, r.Type
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return it, control.Errorf(api.ErrorKindNotFound, "file %s does not exist", path)
		}
		return it, control.Errorf(api.ErrorKindInvalid, "file %s: %v", path, err)
	}
	dir, err := filepath.EvalSymlinks(rec.Dir)
	if err != nil {
		return it, control.Errorf(api.ErrorKindUnavailable, "the Capcom folder %s: %v", rec.Dir, err)
	}
	if rel, err := filepath.Rel(dir, real); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return it, control.Errorf(api.ErrorKindInvalid,
			"file %s is outside the Capcom folder %s; put it with attachment.put first", path, rec.Dir)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return it, control.Errorf(api.ErrorKindNotFound, "file %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return it, control.Errorf(api.ErrorKindInvalid, "%s is not a regular file", path)
	}
	if name == "" {
		name = filepath.Base(path)
	}
	it.Title = capcom.Title(name)
	it.Body = []capcom.Field{{Name: "path", Value: path}, {Name: "size", Value: fmt.Sprintf("%d bytes", fi.Size())}}
	if typ != "" {
		it.Body = append(it.Body, capcom.Field{Name: "type", Value: typ})
	}
	it.Hint = fmt.Sprintf("It is under your folder: read it with your own tools, or file_read path=%s host=%s.", path, c.host)
	return it, nil
}

// resolveCommand takes "<pane id>:<command id>".
func (c *capcomService) resolveCommand(it capcom.Item) (capcom.Item, error) {
	i := strings.LastIndexByte(it.Ref, ':')
	var id uint64
	var err error
	if i > 0 {
		id, err = strconv.ParseUint(it.Ref[i+1:], 10, 64)
	}
	if i <= 0 || err != nil {
		return it, control.Errorf(api.ErrorKindInvalid, "command ref %q: want <pane id>:<command id>, as command.list gives them", it.Ref)
	}
	pane := it.Ref[:i]
	if c.res.command == nil {
		return it, control.Errorf(api.ErrorKindUnavailable, "this hab has no console commands; nothing was attached")
	}
	cmd, err := c.res.command(pane, id)
	if err != nil {
		return it, err
	}
	line := cmd.Text
	if line == "" {
		line = "(the shell did not send the command line)"
	} else if cmd.TextTruncated {
		line += " …"
	}
	status := string(cmd.State)
	if cmd.Exit != nil {
		status = fmt.Sprintf("exit %d", *cmd.Exit)
	}
	it.Title = capcom.Title(line)
	it.Body = []capcom.Field{{Name: "command", Value: line}, {Name: "status", Value: status}, {Name: "folder", Value: cmd.Cwd}, {Name: "pane", Value: pane}}
	it.Hint = fmt.Sprintf(`Its output is in that pane, not in the read tools: gff-hab api call command.output '{"pane":"%s","command":%d}' prints it.`, pane, id)
	return it, nil
}

// commandByID reads one command of a pane from command.list's source.
func commandByID(cs control.Commands) func(pane string, id uint64) (api.Command, error) {
	return func(pane string, id uint64) (api.Command, error) {
		before := id + 1
		l, err := cs.CommandList(api.CommandListParams{Pane: pane, Before: &before, Limit: 1})
		if err != nil {
			return api.Command{}, err
		}
		if len(l.Commands) == 0 || l.Commands[0].ID != id {
			return api.Command{}, control.Errorf(api.ErrorKindNotFound, "pane %s has no command %d", pane, id)
		}
		return l.Commands[0], nil
	}
}

// capcomSender delivers a Capcom session's pending context with the user's next message: the
// blocks go before the text in the same paste, and leave the list only once the send reports
// success (capcom.md 8.1). Channel mail goes to agentService directly and never takes them.
type capcomSender struct {
	control.Agents
	cc *capcomService
}

func (s capcomSender) Send(ctx context.Context, client string, p api.SessionSendParams) error {
	items := s.cc.pending(p.Session)
	if len(items) == 0 {
		if p.Text == "" && len(p.Attachments) == 0 {
			// The context it was let through for went with another message meanwhile.
			return control.Errorf(api.ErrorKindInvalid, "session.send needs a session, and text or attachments; no context waits for session %s", p.Session)
		}
		return s.Agents.Send(ctx, client, p)
	}
	p.Text = capcom.Message(items, p.Text)
	if err := s.Agents.Send(ctx, client, p); err != nil {
		return err
	}
	s.cc.delivered(p.Session, items)
	return nil
}

// SendsContext lets a send with no text through to a Capcom session that has context waiting:
// the blocks go alone (capcom.md 8.4).
func (s capcomSender) SendsContext(session string) bool { return len(s.cc.pending(session)) > 0 }

var _ control.ContextSender = capcomSender{}
