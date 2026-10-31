//go:build !linux && !darwin

package holderreg

import "errors"

// peerUID is not implemented on this system, so every connection is refused.
func peerUID(int) (int, error) {
	return -1, errors.New("peer credentials are not supported on this system")
}
