// Package relay logs through fixture's logger and does not import diaglog itself: the check
// covers it all the same.
package relay

import (
	"github.com/noahkiss/flightdeck/internal/diaglog"
	"github.com/noahkiss/flightdeck/internal/diaglog/check/testdata/fixture"
)

// Relay takes the logger's method as a value.
func Relay() {
	info := fixture.Log.Info // ban: message (a method value)
	info("fine")
}

// info calls Info with a message that is no constant, through an interface of a type parameter.
func info[T ~string](l interface{ Info(T, ...diaglog.Attr) }, s string) { l.Info(T(s)) }

// Generic instantiates info with diaglog's constant type, so no method value shows.
func Generic(what string) {
	info(fixture.Log, what) // ban: message (the constant type as a type argument)
}
