package transcript

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// The opencode versions whose message shapes the parser was written and tested against.
// opencode 2 serves a session's messages through its server API; the shape is in its OpenAPI
// document (Session.Message.Info). A version outside the range still reads, and the reader
// reports drift.
const (
	OpencodeTestedMin = "2.2.0"
	OpencodeTestedMax = "2.2.5"
)

// OpencodePage is how many messages a FetchFunc should ask for in one page.
const OpencodePage = 100

// opencodeMaxPages bounds one fetch, so a server that keeps handing out cursors cannot hold a
// read forever.
const opencodeMaxPages = 1000

// opencodeStale is how long a loaded conversation goes without a fetch when nothing marked it
// dirty: a missed event costs at most this long.
const opencodeStale = 15 * time.Second

// FetchFunc reads one page of a session's messages from the opencode server, newest first.
// An empty cursor starts at the newest; next is empty on the last page.
type FetchFunc func(ctx context.Context, cursor string) (msgs []json.RawMessage, next string, err error)

// OpencodeConfig is what NewOpencode needs. Fetch is required.
type OpencodeConfig struct {
	Session string // gff-hab session id for the messages
	Fetch   FetchFunc
	Logf    func(string, ...any)
	// OnChange, FromEmpty: as in Config.
	OnChange  func([]api.ChatMessage)
	FromEmpty bool
	Now       func() time.Time // default time.Now
}

// OpencodeReader serves an opencode session's conversation, read through its server's API
// (harness-adapters.md, opencode). It never opens opencode's database. Cursors count the
// changes this reader saw, under a name random per reader: a cursor from before a gff-hab
// restart is stale, and the client takes the tail again. It is safe for concurrent use.
type OpencodeReader struct {
	session  string
	fetch    FetchFunc
	logf     func(string, ...any)
	onChange func([]api.ChatMessage)
	now      func() time.Time

	mu      sync.Mutex
	c       *conversation
	gen     string
	seq     int64             // changes seen; the cursor offset
	raw     map[string]string // message id to the JSON last parsed
	final   map[string]bool   // messages that no longer change
	dirty   bool
	fetched time.Time
	loaded  bool
	version string
	drift   string
	warned  map[string]bool
	pending []*item
}

// NewOpencode returns a reader of one opencode session. It fetches nothing until the first
// call.
func NewOpencode(cfg OpencodeConfig) *OpencodeReader {
	var b [6]byte
	rand.Read(b[:])
	r := &OpencodeReader{session: cfg.Session, fetch: cfg.Fetch, logf: cfg.Logf, onChange: cfg.OnChange, now: cfg.Now,
		c: newConversation(), gen: "oc" + hex.EncodeToString(b[:]), raw: map[string]string{}, final: map[string]bool{},
		dirty: true, loaded: cfg.FromEmpty, warned: map[string]bool{}}
	if r.logf == nil {
		r.logf = func(string, ...any) {}
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// MarkDirty says the session's messages changed: the next read fetches them.
func (r *OpencodeReader) MarkDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// SetVersion records the version of the opencode server the messages come from, and reports
// drift when it is outside the tested range.
func (r *OpencodeReader) SetVersion(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v == "" || v == r.version {
		return
	}
	r.version = v
	lo, ok1 := compareVersions(v, OpencodeTestedMin)
	hi, ok2 := compareVersions(v, OpencodeTestedMax)
	switch {
	case !ok1 || !ok2:
		r.drift = fmt.Sprintf("opencode version %q is unreadable; reading in tolerant mode", v)
	case lo < 0 || hi > 0:
		r.drift = fmt.Sprintf("opencode %s is outside the tested range %s to %s; reading in tolerant mode", v, OpencodeTestedMin, OpencodeTestedMax)
	default:
		r.drift = ""
		return
	}
	r.warn("version:"+v, "%s", r.drift)
}

// Drift describes why the reader runs in tolerant mode, or is empty.
func (r *OpencodeReader) Drift() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drift
}

func (r *OpencodeReader) warn(key, format string, args ...any) {
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	r.logf("opencode session %s: "+format, append([]any{r.session}, args...)...)
}

// Refresh fetches the messages when they changed, or when the last fetch is opencodeStale old.
func (r *OpencodeReader) Refresh() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refresh(context.Background())
}

// Size counts the changes the reader saw. It grows when the conversation does.
func (r *OpencodeReader) Size() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// refresh fetches pages from the newest back to the first message it already holds in its
// final form, then parses the changed ones, oldest first.
func (r *OpencodeReader) refresh(ctx context.Context) error {
	if !r.dirty && r.now().Sub(r.fetched) < opencodeStale {
		return nil
	}
	// Every message held in a form that may still change must be fetched again.
	open := map[string]bool{}
	for id := range r.raw {
		if !r.final[id] {
			open[id] = true
		}
	}
	var got []json.RawMessage
	cursor := ""
	reachedFinal := false
	for pages := 1; ; pages++ {
		if pages > opencodeMaxPages {
			return fmt.Errorf("opencode session %s: more than %d pages of messages", r.session, opencodeMaxPages)
		}
		page, next, err := r.fetch(ctx, cursor)
		if err != nil {
			return err
		}
		got = append(got, page...)
		for _, m := range page {
			var head struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(m, &head) == nil {
				reachedFinal = reachedFinal || r.final[head.ID]
				delete(open, head.ID)
			}
		}
		if next == "" || next == cursor || (reachedFinal && len(open) == 0) {
			break
		}
		cursor = next
	}
	r.dirty, r.fetched = false, r.now()
	r.pending = r.pending[:0]
	for i := len(got) - 1; i >= 0; i-- { // oldest first
		r.message(got[i])
	}
	if r.loaded && len(r.pending) > 0 && r.onChange != nil {
		out := make([]api.ChatMessage, 0, len(r.pending))
		for _, it := range dedupe(r.pending) {
			m := it.msg
			m.Session = r.session
			out = append(out, m)
		}
		r.onChange(out)
	}
	r.pending = r.pending[:0]
	r.loaded = true
	return nil
}

// Page fetches what changed, then returns up to limit messages before the message with id
// before (the tail when before is empty), oldest first, whether older ones exist, and a cursor
// for Since.
func (r *OpencodeReader) Page(ctx context.Context, before string, limit int) (msgs []api.ChatMessage, more bool, cursor string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(ctx); err != nil {
		return nil, false, "", err
	}
	msgs, more, err = r.c.page(before, limit)
	if err != nil {
		return nil, false, "", err
	}
	if msgs == nil {
		msgs = []api.ChatMessage{}
	}
	return r.withSession(msgs), more, formatCursor(r.gen, r.seq), nil
}

// Since fetches what changed, then returns the messages added or changed after cursor, oldest
// first, the cursor to pass next, and whether more changes wait after it. A cursor of another
// reader is ErrStaleCursor.
func (r *OpencodeReader) Since(ctx context.Context, cursor string, limit int) (msgs []api.ChatMessage, more bool, next string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(ctx); err != nil {
		return nil, false, "", err
	}
	from, err := parseCursor(cursor, r.gen, r.seq)
	if err != nil {
		return nil, false, "", err
	}
	msgs, k, more := r.c.since(from, r.seq, limit)
	if msgs == nil {
		msgs = []api.ChatMessage{}
	}
	return r.withSession(msgs), more, formatCursor(r.gen, k), nil
}

func (r *OpencodeReader) withSession(msgs []api.ChatMessage) []api.ChatMessage {
	for i := range msgs {
		msgs[i].Session = r.session
	}
	return msgs
}

// ocMessage is the part of an opencode message (Session.Message.Info) the parser reads.
type ocMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   float64 `json:"created"`
		Completed float64 `json:"completed"`
	} `json:"time"`
	Text  string `json:"text"`
	Files []struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	} `json:"files"`
	Content []ocBlock `json:"content"`
	Error   *ocError  `json:"error"`
	Outcome string    `json:"outcome"` // idle
	Status  string    `json:"status"`  // shell, compaction
	Command string    `json:"command"` // shell
	Exit    any       `json:"exit"`    // shell: a number, or Infinity and the like as a string
	Output  struct {
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
	} `json:"output"` // shell
	Name  string `json:"name"`  // skill
	Agent string `json:"agent"` // agent-switched
	Model struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"` // model-switched
}

type ocError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ocBlock is one block of an assistant message's content.
type ocBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	State struct {
		Status  string          `json:"status"`
		Input   json.RawMessage `json:"input"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *ocError `json:"error"`
	} `json:"state"`
}

// opencodeSkipped lists the message types the parser knows and leaves out: text the agent
// loop adds for the model, and folder switches.
var opencodeSkipped = map[string]bool{"synthetic": true, "system": true, "location-switched": true}

func msTime(ms float64) string {
	if ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return ""
	}
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339Nano)
}

// message parses one message when its JSON changed since the last fetch.
func (r *OpencodeReader) message(raw json.RawMessage) {
	var m ocMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
		r.seq++
		id := "unreadable-" + strconv.FormatInt(r.seq, 10)
		r.warn("unreadable", "an opencode message in an unknown shape: %v", err)
		r.emit(api.ChatMessage{ID: id, Kind: api.ChatKindEvent, Event: api.ChatEventUnknown, Text: "An unreadable opencode message"})
		return
	}
	if r.raw[m.ID] == string(raw) {
		return
	}
	r.raw[m.ID] = string(raw)
	r.seq++
	at := msTime(m.Time.Created)
	emit := func(msg api.ChatMessage) {
		if msg.At == "" {
			msg.At = at
		}
		r.emit(msg)
	}
	final := true
	switch m.Type {
	case "user":
		texts := []string{}
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, t)
		}
		for _, f := range m.Files {
			texts = append(texts, "[file "+firstNonEmpty(f.Name, f.URI)+"]")
		}
		if len(texts) > 0 {
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindUser, Text: strings.Join(texts, "\n\n")})
		}
	case "assistant":
		final = m.Time.Completed > 0
		for i, b := range m.Content {
			id := m.ID + "." + strconv.Itoa(i)
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					emit(api.ChatMessage{ID: id, Kind: api.ChatKindAgent, Text: t})
				}
			case "reasoning":
			case "tool":
				if b.ID != "" {
					id = b.ID
				}
				emit(api.ChatMessage{ID: id, Kind: api.ChatKindTool, Tool: opencodeTool(b)})
			default:
				r.warn("block:"+b.Type, "unknown opencode content block %q; shown as a placeholder", b.Type)
				emit(api.ChatMessage{ID: id, Kind: api.ChatKindEvent, Event: api.ChatEventUnknown, Text: "Unsupported opencode content: " + b.Type})
			}
		}
		if m.Error != nil && m.Error.Message != "" {
			emit(api.ChatMessage{ID: m.ID + ".error", Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: m.Error.Message})
		}
	case "idle":
		switch m.Outcome {
		case "failed":
			text := "The turn failed"
			if m.Error != nil && m.Error.Message != "" {
				text += ": " + m.Error.Message
			}
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: text})
		case "interrupted":
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventInterrupt, Text: "Interrupted by the owner"})
		}
	case "compaction":
		switch m.Status {
		case "completed":
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventCompaction, Text: "Conversation compacted"})
		case "failed":
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: "Compaction failed"})
		default:
			final = false
		}
	case "shell":
		final = m.Status != "running"
		tool := &api.ChatTool{Name: "shell", Target: firstLine(m.Command), Status: api.ToolStatusDone}
		switch {
		case m.Status == "running":
			tool.Status = api.ToolStatusRunning
		case m.Status != "exited":
			tool.Status = api.ToolStatusError
		default:
			if n, ok := m.Exit.(float64); !ok || n != 0 {
				tool.Status = api.ToolStatusError
			}
		}
		if in, err := json.Marshal(map[string]string{"command": m.Command}); err == nil && len(in) <= maxToolInput {
			tool.Input = in
		}
		tool.Output, tool.OutputTruncated = cut(m.Output.Output, maxToolOutput)
		tool.OutputTruncated = tool.OutputTruncated || m.Output.Truncated
		emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindTool, Tool: tool})
	case "skill":
		emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: "Skill " + m.Name})
	case "agent-switched":
		emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice, Text: "Agent: " + m.Agent})
	case "model-switched":
		emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventNotice,
			Text: "Model: " + strings.Trim(m.Model.ProviderID+"/"+m.Model.ID, "/")})
	default:
		if !opencodeSkipped[m.Type] {
			r.warn("type:"+m.Type, "unknown opencode message type %q; shown as a placeholder", m.Type)
			emit(api.ChatMessage{ID: m.ID, Kind: api.ChatKindEvent, Event: api.ChatEventUnknown, Text: "Unsupported opencode message: " + m.Type})
		}
	}
	if final {
		r.final[m.ID] = true
	}
}

func (r *OpencodeReader) emit(msg api.ChatMessage) {
	r.pending = append(r.pending, r.c.put(msg, r.seq))
}

// opencodeTool maps an assistant message's tool block to a chat tool.
func opencodeTool(b ocBlock) *api.ChatTool {
	t := &api.ChatTool{Name: b.Name, Target: opencodeTarget(b.State.Input)}
	switch b.State.Status {
	case "completed":
		t.Status = api.ToolStatusDone
	case "error":
		t.Status = api.ToolStatusError
		if b.State.Error != nil && strings.Contains(strings.ToLower(b.State.Error.Type+" "+b.State.Error.Message), "reject") {
			t.Status = api.ToolStatusDenied
		}
	default: // streaming, running
		t.Status = api.ToolStatusRunning
	}
	if in := b.State.Input; len(in) > 0 && string(in) != "null" && len(in) <= maxToolInput {
		t.Input = api.RawJSON(in)
	}
	var parts []string
	for _, c := range b.State.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "media":
			parts = append(parts, "["+c.Type+"]")
		}
	}
	if len(parts) == 0 && b.State.Error != nil {
		parts = append(parts, b.State.Error.Message)
	}
	t.Output, t.OutputTruncated = cut(strings.Join(parts, "\n"), maxToolOutput)
	return t
}

// opencodeTarget picks a short target from an opencode tool input: a file, a command, a
// pattern, a URL.
func opencodeTarget(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"filePath", "path", "command", "pattern", "url", "query", "description"} {
		var s string
		if json.Unmarshal(m[k], &s) == nil && strings.TrimSpace(s) != "" {
			return firstLine(s)
		}
	}
	return ""
}
