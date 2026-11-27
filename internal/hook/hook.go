// Package hook is gff-hab's one hook endpoint (docs/specs/harness-adapters.md, Hook endpoint).
// Every harness's hook config runs `gff-hab hook`, which posts the native payload in an
// envelope to gff-hab over HTTP on a local Unix socket. No TCP port is opened.
//
// The address is the GFF_HAB variable, one per pane: http+unix://<socket path>/hooks/<token>.
// The token names the pane: its uuid plus a keyed hash of that uuid under a secret in the state
// directory (Tokens). gff-hab puts GFF_HAB into each pane's environment when it spawns the pane.
package hook

import (
	"errors"
	"fmt"
	"strings"
)

// Scheme is the URL scheme of GFF_HAB for a local hab.
const Scheme = "http+unix://"

// Env is the variable that carries the per-pane hook address.
const Env = "GFF_HAB"

// Addr formats the per-pane address: http+unix://<socket path>/hooks/<token>. The socket path
// must be absolute.
func Addr(socket, token string) string {
	return Scheme + socket + "/hooks/" + token
}

// childPath ends the GFF_HAB value of a program gff-hab serve starts (ChildAddr).
const childPath = "/serve-child"

// ChildAddr is the GFF_HAB value gff-hab serve gives each program it starts that may run code a
// repository holds: git, lane checks, worktree scripts (hab-logic-api.md 4.1). It names the hook
// socket, so the pane caller check counts such a program as a pane's, but it holds no token, so
// it reaches no hook and names no pane. ParseAddr refuses it.
func ChildAddr(socket string) string { return Scheme + socket + childPath }

// ParseChildAddr returns the hook socket a ChildAddr value names.
func ParseChildAddr(s string) (socket string, ok bool) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return "", false
	}
	socket, ok = strings.CutSuffix(rest, childPath)
	if !ok || !strings.HasPrefix(socket, "/") {
		return "", false
	}
	return socket, true
}

// ParseAddr splits a GFF_HAB value into the socket path and the pane token.
func ParseAddr(s string) (socket, token string, err error) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return "", "", fmt.Errorf("hook address %q does not start with %s", s, Scheme)
	}
	if _, child := ParseChildAddr(s); child {
		return "", "", fmt.Errorf("hook address %q is the mark gff-hab serve gives the programs it starts: it reaches no hook", s)
	}
	i := strings.LastIndex(rest, "/hooks/")
	if i <= 0 {
		return "", "", fmt.Errorf("hook address %q has no /hooks/<token>", s)
	}
	socket, token = rest[:i], rest[i+len("/hooks/"):]
	if !strings.HasPrefix(socket, "/") || !validToken(token) {
		return "", "", fmt.Errorf("hook address %q is malformed", s)
	}
	return socket, token, nil
}

func validToken(t string) bool {
	if len(t) < 16 || len(t) > 128 {
		return false
	}
	for _, c := range t {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ErrNoHab means GFF_HAB is unset: the hook runs outside any pane gff-hab owns.
var ErrNoHab = errors.New("GFF_HAB is not set")
