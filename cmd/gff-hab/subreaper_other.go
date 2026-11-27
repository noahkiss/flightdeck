//go:build !linux

package main

// startSubreaper does nothing here: only Linux has a child subreaper. What a program serve
// started leaves running moves to launchd; its mark and its session still catch it
// (hab-logic-api.md 4.1).
func startSubreaper(func(format string, args ...any)) {}
