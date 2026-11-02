package modhost

import (
	"errors"
	"fmt"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

// TimeoutError is a call interrupted at its time limit (modules.md 8.1).
type TimeoutError struct {
	Func  string // load, refresh, render or act
	Limit time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s did not finish within %s", e.Func, e.Limit)
}

// MemoryError is a call interrupted because the Go heap passed Limits.Memory. The module is
// stopped: every later call returns ErrStopped, and the host loads it again.
type MemoryError struct {
	Func  string
	Limit uint64
}

func (e *MemoryError) Error() string {
	return fmt.Sprintf("%s passed the memory limit of %d MiB", e.Func, e.Limit>>20)
}

// ScriptError is an exception the module did not catch, or a syntax error in one of its files.
type ScriptError struct {
	Func    string
	Name    string // the JS error's name: Error, TypeError, PowerError, BridgeError, SyntaxError
	Message string
	Stack   string
}

func (e *ScriptError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("%s: %s", e.Func, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Func, e.Name, e.Message)
}

// TreeError is a render whose result is not a valid block tree.
type TreeError struct {
	Problems modules.Problems
}

func (e *TreeError) Error() string {
	return "render returned a bad tree: " + e.Problems.Error()
}

// ClientTooOld is a render asked for a vocabulary below the module's blocks version. The client
// shows the module's name and "Needs a newer Flightdeck" (modules.md 9).
type ClientTooOld struct {
	Vocabulary int
	Blocks     int64
}

func (e *ClientTooOld) Error() string {
	return fmt.Sprintf("the module needs block vocabulary %d; the client draws %d", e.Blocks, e.Vocabulary)
}

var (
	// ErrStopped is returned by every call after a MemoryError.
	ErrStopped = errors.New("module stopped after passing its memory limit")
	// ErrNoAct is returned by Act for a module that exports no act function.
	ErrNoAct = errors.New("the module has no act function")
	// ErrClientAction is returned by Act for a client.* action, which never reaches a module.
	ErrClientAction = errors.New("a client action never reaches the module")
)

// Names of the errors the bridge throws into JS. A module may catch them.
const (
	// PowerError: a call that needs a power the manifest does not declare (modules.md 7.2).
	PowerError = "PowerError"
	// BridgeError: a bad argument, a call not allowed in this function, a backend failure or a
	// limit passed (state size).
	BridgeError = "BridgeError"
)
