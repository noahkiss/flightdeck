//go:build unix

package structure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is the name of the lock file inside the state directory.
const LockFile = "structure.lock"

// dirLock is an exclusive flock on the state directory's lock file. The kernel drops it when the
// process exits, so a crash never leaves a stale lock.
type dirLock struct{ f *os.File }

func lockDir(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state directory lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
		}
		return nil, fmt.Errorf("state directory lock: %w", err)
	}
	return &dirLock{f: f}, nil
}

func (l *dirLock) release() {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
