//go:build !darwin && !linux

package main

// procfsRoot is empty: listProcs uses ps.
const procfsRoot = ""

// procStart reads no start here: a registry entry's pid counts as its own process.
func procStart(int) (string, bool) { return "", false }
