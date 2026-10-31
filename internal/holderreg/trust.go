package holderreg

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// The holder and gff-hab trust the holder directory and the sockets in it only as far as these
// checks go: the holder runs them before it listens, and gff-hab before it reads the registry
// and after it dials a holder.

func ownerOf(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// CheckPrivateDir requires d to be a real directory (not a symlink) owned by uid with no access
// for others.
func CheckPrivateDir(d string, uid int) error {
	fi, err := os.Lstat(d)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("holder: socket directory %s is not a directory (mode %v)", d, fi.Mode())
	}
	if owner, ok := ownerOf(fi); !ok || owner != uid {
		return fmt.Errorf("holder: socket directory %s is owned by uid %d, not %d", d, owner, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("holder: socket directory %s is open to other users (mode %v)", d, fi.Mode().Perm())
	}
	return nil
}

// CheckParent requires that no other user can rename entries in d, unless d is sticky. A
// symlinked parent (macOS has /tmp -> /private/tmp) is checked at its target.
func CheckParent(d string, uid int) error {
	fi, err := os.Stat(d)
	if err != nil {
		return err
	}
	owner, ok := ownerOf(fi)
	if !ok {
		return fmt.Errorf("holder: cannot read the owner of %s", d)
	}
	if owner != uid && owner != 0 {
		return fmt.Errorf("holder: %s is owned by uid %d, not by this user or root", d, owner)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("holder: %s is writable by other users and not sticky (mode %v)", d, fi.Mode())
	}
	return nil
}

// CheckDir runs the checks the holder runs on its socket directory before it listens: dir is a
// private directory of this user, and its parent does not let other users rename it away. A
// missing directory passes: it holds no registry.
func CheckDir(dir string) error {
	uid := os.Getuid()
	if err := CheckPrivateDir(dir, uid); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return CheckParent(filepath.Dir(filepath.Clean(dir)), uid)
}

// CheckSocket requires path to be a socket file (not a symlink) owned by this user.
func CheckSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("holder: %s is not a socket (mode %v)", path, fi.Mode())
	}
	if owner, ok := ownerOf(fi); !ok || owner != os.Getuid() {
		return fmt.Errorf("holder: socket %s is owned by uid %d, not %d", path, owner, os.Getuid())
	}
	return nil
}

// PeerUID returns the uid of the process at the other end of a Unix socket connection.
func PeerUID(nc *net.UnixConn) (int, error) {
	sc, err := nc.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var uerr error
	if err := sc.Control(func(fd uintptr) { uid, uerr = peerUID(int(fd)) }); err != nil {
		return -1, err
	}
	return uid, uerr
}
