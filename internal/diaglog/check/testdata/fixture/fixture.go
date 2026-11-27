// Package fixture breaks each ban of the source check once (docs/specs/diagnostics.md 10.2).
// The check's test runs on it and expects one finding per rule; nothing builds it otherwise.
package fixture

import (
	"fmt"
	"log/slog" // ban: slog outside internal/diaglog
	"net"      // ban: a network package in a diagnostics package (the test lists this one)
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/diaglog"
)

var log = diaglog.For("fixture")

// Message logs a message that is not a constant.
func Message(what string) {
	log.Info("fine")
	log.Info(what) // ban: message
}

// Built builds a safe value with fmt and strings.
func Built(id string) {
	log.Info("built", diaglog.Session(fmt.Sprintf("s-%s", id))) // ban: built
	log.Info("fine", diaglog.Text(strings.ToUpper(id)))         // text class: allowed
}

// Content passes content to a diaglog call.
func Content(b []byte, m api.ChatMessage) {
	log.Info("content", diaglog.Text(string(b)), diaglog.Count("n", int64(len(m.Text)))) // ban: content, twice
}

// hot is listed as a hot path by the test.
func hot() {
	log.Debug("hot") // ban: hot-path
}

var _ = hot
var _ = slog.Default
var _ = net.IPv4len
