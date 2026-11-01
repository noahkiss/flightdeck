package control

import (
	"encoding/json"
	"fmt"

	"github.com/noahkiss/flightdeck/api"
)

// JSON-RPC 2.0 error codes used besides api.ErrorCode.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// message is any JSON-RPC 2.0 message on the control connection, in either direction. A request
// has Method and ID, a notification has Method and no ID, a response has ID and Result or Error.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    api.ErrorData `json:"data"`
}

// Error is a JSON-RPC error a call returned. Clients branch on Data.Kind (spec 3.6).
type Error struct {
	Code    int
	Message string
	Data    api.ErrorData

	partial bool // the call may have taken effect in part; see Partial
}

// Partial marks an error after which the call may have taken effect in part, such as a
// session.send whose paste reached the program but whose Enter did not. A call with an
// idempotency key keeps such an error as it keeps a success: a retry gets the same error and
// runs nothing (spec 3.9, Idempotency keys).
func Partial(e *Error) *Error {
	c := *e
	c.partial = true
	return &c
}

// IsPartial reports whether Partial marked e.
func (e *Error) IsPartial() bool { return e.partial }

func (e *Error) Error() string {
	if e.Data.Hint != "" && e.Data.Hint != e.Message {
		return fmt.Sprintf("%s (%s): %s", e.Message, e.Data.Kind, e.Data.Hint)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Data.Kind)
}

// Notification is one notification from the server: event, heartbeat or closing.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Event decodes the params of an event notification.
func (n Notification) Event() (api.Event, error) {
	var e api.Event
	err := json.Unmarshal(n.Params, &e)
	return e, err
}

// appErr builds an application error (spec 3.6).
func appErr(kind api.ErrorKind, format string, args ...any) *Error {
	hint := fmt.Sprintf(format, args...)
	return &Error{Code: api.ErrorCode, Message: hint, Data: api.ErrorData{Kind: kind, Hint: hint}}
}

// Errorf builds an application error of kind (spec 3.6). Actions return it to choose the kind.
func Errorf(kind api.ErrorKind, format string, args ...any) *Error {
	return appErr(kind, format, args...)
}

// protoErr builds a JSON-RPC protocol error. Its data still carries kind invalid and the seq.
func protoErr(code int, format string, args ...any) *Error {
	hint := fmt.Sprintf(format, args...)
	return &Error{Code: code, Message: hint, Data: api.ErrorData{Kind: api.ErrorKindInvalid, Hint: hint}}
}
