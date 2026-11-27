//go:build !linux && !darwin

package peerproc

import "errors"

// System cannot read processes on this system, so every caller counts as a pane's.
func System() Table { return noTable{} }

type noTable struct{}

func (noTable) Proc(int) (Proc, error) {
	return Proc{}, errors.New("process reads are not supported on this system")
}
func (noTable) Env(int) []byte { return nil }
