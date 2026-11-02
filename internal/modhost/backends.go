package modhost

import (
	"context"
	"encoding/json"
	"time"
)

// Hab is the hab API as the bridge reaches it (modules.md 7.1). Get serves fd.hab.get and must
// refuse a method that changes something; Act serves fd.hab.act, which only a module with the
// act power reaches. Params and results are JSON. The module host maps the module's bridge
// version to an API revision (modules.md 9).
type Hab interface {
	Get(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	Act(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
}

// Exec runs a program for fd.exec: argv[0] found on PATH or given as a path, no shell.
type Exec interface {
	Run(ctx context.Context, req ExecRequest) (ExecResult, error)
}

// ExecRequest is one fd.exec call. Timeout is never past the current call's deadline, which ctx
// also carries.
type ExecRequest struct {
	Argv    []string
	Dir     string
	Timeout time.Duration
}

// ExecResult is what the program did. The runtime cuts each stream to Limits.Output.
type ExecResult struct {
	Code   int
	Stdout []byte
	Stderr []byte
}

// Fetch makes the HTTP or HTTPS request of fd.fetch.
type Fetch interface {
	Do(ctx context.Context, req FetchRequest) (FetchResponse, error)
}

// FetchRequest is one fd.fetch call. URL is http or https; Method defaults to GET.
type FetchRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

// FetchResponse is the answer. The runtime cuts Body to Limits.Output.
type FetchResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Store keeps each module's state across restarts (fd.state, modules.md 7.1). Load returns an
// empty map for a module with none. Save replaces the module's whole state.
type Store interface {
	Load(module string) (map[string]json.RawMessage, error)
	Save(module string, state map[string]json.RawMessage) error
}

// Backends are what the bridge reaches. A nil Hab, Exec or Fetch makes the call it serves
// throw; a nil State keeps state in memory only; a nil Log drops fd.log lines.
type Backends struct {
	Hab   Hab
	Exec  Exec
	Fetch Fetch
	State Store
	Log   func(line string)
}
