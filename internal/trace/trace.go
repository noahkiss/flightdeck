// Package trace makes and checks the trace ids that follow one action across processes
// (diagnostics.md 5). A trace is 12 characters of Crockford base32: the first, in lower case,
// says who made it; the other 11, in upper case, are random. It holds no content and no time.
package trace

import (
	"context"
	"crypto/rand"
	"strings"
)

// Len is the length of a trace.
const Len = 12

// The first character of a trace: who made it.
const (
	App byte = 'a' // the app
	Gff byte = 'g' // the gff client
	Hab byte = 'h' // gff-hab, for a request that came without a valid trace
)

// alphabet is Crockford's base32.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a new trace made by origin, one of App, Gff and Hab.
func New(origin byte) string {
	var r [Len - 1]byte
	rand.Read(r[:])
	b := make([]byte, Len)
	b[0] = origin
	for i, x := range r {
		b[i+1] = alphabet[x&31]
	}
	return string(b)
}

// Valid reports whether s is a trace in its written form: a known origin, then 11 upper-case
// Crockford base32 characters. Anything else is not one; Normalize fixes the case a user typed.
func Valid(s string) bool {
	if len(s) != Len {
		return false
	}
	switch s[0] {
	case App, Gff, Hab:
	default:
		return false
	}
	for i := 1; i < Len; i++ {
		if !inAlphabet(s[i]) {
			return false
		}
	}
	return true
}

func inAlphabet(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c < 'A' || c > 'Z':
		return false
	}
	return c != 'I' && c != 'L' && c != 'O' && c != 'U'
}

// Normalize puts a trace a user typed in its written form: the first character in lower case,
// the rest in upper case. It checks nothing.
func Normalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + strings.ToUpper(s[1:])
}

// Check returns s when it is a valid trace. Otherwise it returns a new Hab trace, and replaced
// is true when s was not empty (diagnostics.md 5.2).
func Check(s string) (id string, replaced bool) {
	if Valid(s) {
		return s, false
	}
	return New(Hab), s != ""
}

type key struct{}

// With returns ctx carrying id. An empty id returns ctx unchanged.
func With(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, id)
}

// From returns the trace ctx carries, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
