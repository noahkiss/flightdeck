// Package opencode is a client for the HTTP API of an opencode 2.x server: the server a pane's
// opencode TUI is a client of (harness-adapters.md, opencode). gff-hab attaches to that same
// server as a second client, so nothing forks the session.
//
// The client is typed by hand for the few operations the adapter uses. Operations names each
// one as the server's OpenAPI document (/openapi.json) does, and CheckDoc checks a server's
// document against it: the document does not type the event stream, so a generated client
// would add little.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TestedVersion is the opencode version the client and the adapter were tested against. A
// server that reports another version still works; the adapter logs the drift.
const TestedVersion = "2.2.5"

// User is the basic auth user name opencode's server expects with OPENCODE_PASSWORD.
const User = "opencode"

// PasswordEnv is the variable opencode's server and TUI read the server password from.
const PasswordEnv = "OPENCODE_PASSWORD"

// Operation is one API operation the client calls, as the OpenAPI document names it.
type Operation struct {
	ID     string // operationId
	Method string
	Path   string
}

// Operations lists every operation the client calls.
var Operations = []Operation{
	{"server.info", http.MethodGet, "/api/info"},
	{"event.subscribe", http.MethodGet, "/api/event"},
	{"session.get", http.MethodGet, "/api/session/{sessionID}"},
	{"session.active", http.MethodGet, "/api/session/active"},
	{"session.message.list", http.MethodGet, "/api/session/{sessionID}/message"},
	{"session.prompt", http.MethodPost, "/api/session/{sessionID}/prompt"},
	{"session.synthetic", http.MethodPost, "/api/session/{sessionID}/synthetic"},
	{"permission.request.list", http.MethodGet, "/api/permission/request"},
	{"session.permission.reply", http.MethodPost, "/api/session/{sessionID}/permission/{requestID}/reply"},
}

// maxBody bounds a JSON answer the client reads whole.
const maxBody = 32 << 20

// Client talks to one opencode server. It is safe for concurrent use.
type Client struct {
	base     string // http://127.0.0.1:<port>, no trailing slash
	password string
	hc       *http.Client
}

// New returns a client of the server at base, which must be an http URL on a loopback host.
func New(base, password string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("opencode server %q: want http://<loopback host>:<port>", base)
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "::1" && h != "localhost" {
		return nil, fmt.Errorf("opencode server %q: not a loopback host", base)
	}
	return &Client{base: strings.TrimSuffix(base, "/"), password: password,
		hc: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}}}, nil
}

// Base returns the server URL.
func (c *Client) Base() string { return c.base }

// APIError is an answer other than success. Tag is the server's error tag, such as
// PermissionNotFoundError.
type APIError struct {
	Op      string
	Status  int
	Tag     string
	Message string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("opencode %s: HTTP %d", e.Op, e.Status)
	if e.Tag != "" {
		s += " " + e.Tag
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Error tags the server answers with when what a request names does not exist.
const (
	TagSessionNotFound    = "SessionNotFoundError"
	TagPermissionNotFound = "PermissionNotFoundError"
)

// NotFound reports whether err is a 404 from the server with one of tags as its error tag. A
// 404 with another tag, or with none, is a route the server does not have (API drift), not a
// missing session or request.
func NotFound(err error, tags ...string) bool {
	var e *APIError
	if !errors.As(err, &e) || e.Status != http.StatusNotFound {
		return false
	}
	for _, t := range tags {
		if e.Tag == t {
			return true
		}
	}
	return false
}

// Drift reports whether err is a 404 that names no known missing thing: the route itself is
// missing, so the server's API moved.
func Drift(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound &&
		e.Tag != TagSessionNotFound && e.Tag != TagPermissionNotFound
}

// idPattern is the shape of a session, request or message id the client puts in a path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func checkID(what, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("opencode: %s id %q is not an id", what, id)
	}
	return nil
}

func (c *Client) request(ctx context.Context, op, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(User, c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode %s: %w", op, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		e := &APIError{Op: op, Status: resp.StatusCode}
		var tagged struct {
			Tag     string `json:"_tag"`
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &tagged) == nil {
			e.Tag, e.Message = tagged.Tag, tagged.Message
		}
		return nil, e
	}
	return resp, nil
}

// call runs one operation and decodes a JSON answer into out, when out is not nil.
func (c *Client) call(ctx context.Context, op, method, path string, q url.Values, body, out any) error {
	resp, err := c.request(ctx, op, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("opencode %s: %w", op, err)
	}
	if len(b) > maxBody {
		return fmt.Errorf("opencode %s: answer over %d bytes", op, maxBody)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("opencode %s: %w", op, err)
	}
	return nil
}

// Info is what server.info returns. PID is the server's process id: opencode 2.2.5 reports
// it, and another version may not (0).
type Info struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Info returns the server's identity.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, "server.info", http.MethodGet, "/api/info", nil, nil, &out)
	return out, err
}

// Doc returns the server's OpenAPI document.
func (c *Client) Doc(ctx context.Context) ([]byte, error) {
	var out json.RawMessage
	err := c.call(ctx, "openapi", http.MethodGet, "/openapi.json", nil, nil, &out)
	return out, err
}

// CheckDoc checks that an OpenAPI document has every operation in Operations, under the same
// method and path. It names each one that is missing or moved.
func CheckDoc(doc []byte) error {
	var d struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return fmt.Errorf("opencode OpenAPI document: %w", err)
	}
	var bad []string
	for _, op := range Operations {
		if d.Paths[op.Path][strings.ToLower(op.Method)].OperationID != op.ID {
			bad = append(bad, op.ID+" ("+op.Method+" "+op.Path+")")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("opencode OpenAPI document lacks %s", strings.Join(bad, ", "))
	}
	return nil
}

// Session is the part of a session's record the adapter reads.
type Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
}

// Session returns one session.
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	if err := checkID("session", id); err != nil {
		return Session{}, err
	}
	var out struct {
		Data Session `json:"data"`
	}
	err := c.call(ctx, "session.get", http.MethodGet, "/api/session/"+id, nil, nil, &out)
	return out.Data, err
}

// Active returns the ids of the sessions whose agent loop runs in this server process now.
func (c *Client) Active(ctx context.Context) ([]string, error) {
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := c.call(ctx, "session.active", http.MethodGet, "/api/session/active", nil, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for id := range out.Data {
		ids = append(ids, id)
	}
	return ids, nil
}

// Permission is a pending permission request: the data of a permission.asked event.
type Permission struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Save      []string `json:"save"`
	Source    struct {
		Type      string `json:"type"`
		MessageID string `json:"messageID"`
		ID        string `json:"id"`
	} `json:"source"`
}

// Permissions returns the pending permission requests of every session this server runs, each
// as its raw JSON.
func (c *Client) Permissions(ctx context.Context) ([]json.RawMessage, error) {
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	err := c.call(ctx, "permission.request.list", http.MethodGet, "/api/permission/request", nil, nil, &out)
	return out.Data, err
}

// MessagePage is one page of a session's messages, each as its raw JSON.
type MessagePage struct {
	Data   []json.RawMessage `json:"data"`
	Cursor struct {
		Previous string `json:"previous"`
		Next     string `json:"next"`
	} `json:"cursor"`
}

// Messages returns a page of a session's messages, newest first. An empty cursor starts at the
// newest; a page's Cursor.Next reads the older ones.
func (c *Client) Messages(ctx context.Context, session, cursor string, limit int) (MessagePage, error) {
	var out MessagePage
	if err := checkID("session", session); err != nil {
		return out, err
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	} else {
		q.Set("order", "desc")
	}
	err := c.call(ctx, "session.message.list", http.MethodGet, "/api/session/"+session+"/message", q, nil, &out)
	return out, err
}

// Prompt sends one message to a session as the owner's input. The server queues it and runs
// the agent loop; the TUI shows it as its own turn.
func (c *Client) Prompt(ctx context.Context, session, text string) error {
	if err := checkID("session", session); err != nil {
		return err
	}
	return c.call(ctx, "session.prompt", http.MethodPost, "/api/session/"+session+"/prompt", nil,
		map[string]any{"text": text}, nil)
}

// Synthetic adds text to a session as a synthetic message that starts no turn (resume false):
// the server keeps it in the session's inbox and the agent reads it with the next prompt.
// description is the label clients show for it.
func (c *Client) Synthetic(ctx context.Context, session, text, description string) error {
	if err := checkID("session", session); err != nil {
		return err
	}
	return c.call(ctx, "session.synthetic", http.MethodPost, "/api/session/"+session+"/synthetic", nil,
		map[string]any{"text": text, "description": description, "resume": false}, nil)
}

// Decisions a permission reply takes.
const (
	ReplyOnce   = "once"
	ReplyAlways = "always"
	ReplyReject = "reject"
)

// Reply answers a pending permission request of a session. A request already answered, or
// unknown, is a 404 (NotFound).
func (c *Client) Reply(ctx context.Context, session, request, decision, message string) error {
	if err := checkID("session", session); err != nil {
		return err
	}
	if err := checkID("permission request", request); err != nil {
		return err
	}
	body := map[string]any{"decision": decision}
	if message != "" {
		body["message"] = message
	}
	return c.call(ctx, "session.permission.reply", http.MethodPost,
		"/api/session/"+session+"/permission/"+request+"/reply", nil, body, nil)
}
