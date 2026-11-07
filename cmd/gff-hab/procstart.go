package main

import (
	"bytes"
	"strings"
)

// statStart reads the start tick, field 22, of a /proc/<pid>/stat line. The command name in
// field 2 may hold spaces and parentheses, so the fields count from its last ')'.
func statStart(b []byte) (string, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}
