// Package fixture breaks each ban of the source check (docs/specs/diagnostics.md 10.2). The
// check's test runs on it and on fixture/relay, and expects each finding at a line whose comment
// names its rule; nothing builds it otherwise.
package fixture

import (
	"fmt"
	"log/slog" // ban: slog outside internal/diaglog
	"net"      // ban: network package in a diagnostics package (the test lists this one)
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/diaglog"
)

// Log is the fixture's logger; relay logs through it.
var Log = diaglog.For("fixture")

// apply calls f with a message that is no constant: the constant type does not stop it, so the
// check bans the method value instead.
func apply[T ~string](f func(T, ...diaglog.Attr), s string) { f(T(s)) }

// Message logs a message that is not a constant, and a value under a reserved key.
func Message(what string) {
	Log.Info("fine")
	(Log.Info)("fine, in parentheses")
	apply(Log.Info, what)                             // ban: message (a method value)
	Log.Info("reserved", diaglog.Word("msg", "fake")) // ban: message (a reserved key)
}

// Built builds a safe value with fmt and strings.
func Built(id string) {
	Log.Info("built", diaglog.Session(fmt.Sprintf("s-%s", id))) // ban: built
	Log.Info("fine", diaglog.Text(strings.ToUpper(id)))         // text class: allowed
}

// Content passes content to a diaglog call.
func Content(b []byte, m api.ChatMessage) {
	Log.Info("content", diaglog.Text(string(b)), diaglog.Count("n", int64(len(m.Text)))) // ban: content, twice
}

// hot is listed as a hot path by the test.
func hot() {
	Log.Debug("hot") // ban: hot-path
}

// Package-level values are checked as function bodies are.
var (
	early = diaglog.Session(fmt.Sprint(1)) // ban: built
	_     = func(b []byte) {
		Log.Info("literal", diaglog.Text(string(b))) // ban: content
	}
)

var _ = hot
var _ = early
var _ = slog.Default
var _ = net.IPv4len
