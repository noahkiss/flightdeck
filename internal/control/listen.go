package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Listen opens the control socket at path (spec 4.1). It makes the socket's directory with mode
// 0700 when missing and refuses a directory that other users can reach. A stale socket is
// removed; a live one is an error, and so is any path that is not a socket. The socket itself is mode 0600. Listen opens no TCP port.
func Listen(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("control: %s exists and is not a socket; not removing it", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("control: socket %s is in use by another gff-hab", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// checkPrivateDir requires d to be a directory owned by this user with no access for others.
func checkPrivateDir(d string) error {
	fi, err := os.Lstat(d)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("control: socket directory %s is not a directory", d)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("control: socket directory %s is not owned by this user", d)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control: socket directory %s is open to other users (mode %v)", d, fi.Mode().Perm())
	}
	return nil
}

// CheckPeer refuses a caller that runs as another user (spec 4.1). The hook endpoint uses it
// on its own socket.
func CheckPeer(nc *net.UnixConn) error { return checkPeer(nc) }

// checkPeer refuses a caller that runs as another user (spec 4.1).
func checkPeer(nc *net.UnixConn) error {
	raw, err := nc.SyscallConn()
	if err != nil {
		return err
	}
	var uid int
	var perr error
	if err := raw.Control(func(fd uintptr) { uid, perr = peerUID(int(fd)) }); err != nil {
		return err
	}
	if perr != nil {
		return perr
	}
	if uid != os.Getuid() {
		return errors.New("control: caller runs as another user")
	}
	return nil
}

// DefaultSocket picks the control socket path: $GFF_HAB_SOCKET, else gff/hab.sock under the
// user config directory on macOS (not Caches, which cleanup may empty) and under
// $XDG_RUNTIME_DIR elsewhere. With no XDG_RUNTIME_DIR it fails rather than fall back to a
// shared directory.
func DefaultSocket(goos string, getenv func(string) string, configDir func() (string, error)) (string, error) {
	if p := getenv("GFF_HAB_SOCKET"); p != "" {
		return p, nil
	}
	if goos == "darwin" {
		d, err := configDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "gff", "hab.sock"), nil
	}
	if d := getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "gff", "hab.sock"), nil
	}
	return "", errors.New("no socket path: set GFF_HAB_SOCKET or XDG_RUNTIME_DIR, or pass -socket")
}
