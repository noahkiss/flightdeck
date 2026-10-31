//go:build unix

package hook

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path for reading and refuses a symlink.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("hook: %s is a symlink", path)
	}
	return f, err
}

// checkOwner refuses a file another user owns.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("no owner information")
	}
	if uid := os.Geteuid(); int(st.Uid) != uid {
		return fmt.Errorf("owned by uid %d, not %d", st.Uid, uid)
	}
	return nil
}
