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
	// Stuck is a call whose runtime did not stop within a short grace after its time limit or
	// the caller's cancel: a long built-in that never checks the interrupt. The runtime is
	// abandoned and the module stopped (ErrStopped after); the host drops it.
	Stuck bool
}

func (e *TimeoutError) Error() string {
	if e.Stuck {
		return fmt.Sprintf("%s did not stop after its limit of %s; the module is stopped", e.Func, e.Limit)
	}
	return fmt.Sprintf("%s did not finish within %s", e.Func, e.Limit)
}

// InternalError is a Go panic inside a call: in the bridge, a backend or the runtime. The
// runtime may be inconsistent, so the module is stopped (ErrStopped after).
type InternalError struct {
	Func  string
	Value string // the panic value
	Stack string // the Go stack where it panicked
}

func (e *InternalError) Error() string {
	return fmt.Sprintf("%s: internal error: %s", e.Func, e.Value)
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
	// ErrStopped is returned by every call after a MemoryError, an InternalError or a stuck
	// TimeoutError. The host drops the Module and loads the module again.
	ErrStopped = errors.New("module stopped; load it again")
	// ErrHeapPressure is a call refused before any module code ran, because the module host's
	// heap was over Limits.Memory even after a collection: something else holds the memory.
	// The module is not blamed and keeps running; the host looks for the module whose calls
	// grew the heap (Module.HeapGrowth).
	ErrHeapPressure = errors.New("the module host's heap is over its memory limit")
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
