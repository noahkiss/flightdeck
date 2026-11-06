//go:build !linux && !darwin

package procscope

import "errors"

func listProcesses() ([]int, error)                        { return nil, errors.New("process scope reads unavailable") }
func inspectProcess(pid int, token string) (process, bool) { return process{}, false }
