//go:build !unix

package structure

// LockFile is the name of the lock file inside the state directory.
const LockFile = "structure.lock"

// dirLock does nothing where flock is missing. gff-hab runs on macOS and Linux only.
type dirLock struct{}

func lockDir(string) (*dirLock, error) { return &dirLock{}, nil }

func (*dirLock) release() {}
