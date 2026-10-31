//go:build !unix

package secretfile

import (
	"errors"
	"os"
)

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

// checkOwner is not implemented on this system, so every key file is refused.
func checkOwner(os.FileInfo) error {
	return errors.New("file owners are not supported on this system")
}
