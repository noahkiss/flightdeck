//go:build !linux && !darwin

package hoststats

func statVolume(string) (free, total uint64, ok bool) { return 0, 0, false }
