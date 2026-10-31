package holder

import (
	"errors"
	"fmt"
	"os"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// openPTY opens a PTY pair. The master comes back non-blocking and registered with Go's poller,
// so a pump blocked in Read returns as soon as the master is closed. Both descriptors are
// close-on-exec: a child never inherits another pane's master.
//
// creack/pty opens the pair. Its own ioctl helpers call File.Fd, which switches the descriptor
// back to blocking mode, so the holder never uses them on the master; it uses the ioctls below
// through SyscallConn instead.
func openPTY() (master, tty *os.File, err error) {
	m, t, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	// m.Fd has made m blocking. Take a fresh close-on-exec descriptor for the same file
	// description, make it non-blocking, and let os.NewFile register it with the poller.
	fd, err := unix.FcntlInt(m.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	m.Close()
	if err != nil {
		t.Close()
		return nil, nil, fmt.Errorf("dup pty master: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		t.Close()
		return nil, nil, fmt.Errorf("pty master non-blocking: %w", err)
	}
	return os.NewFile(uintptr(fd), "ptmx"), t, nil
}

// control runs fn on f's descriptor without changing its blocking mode.
func control(f *os.File, fn func(fd int) error) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := sc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// setSize sets the PTY window size. The kernel sends SIGWINCH to the foreground group when the
// size changes.
func setSize(f *os.File, cols, rows uint16) error {
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

// foregroundGroup returns the PTY's foreground process group, or 0 when it has none.
func foregroundGroup(f *os.File) (int, error) {
	var pgid int
	err := control(f, func(fd int) error {
		var err error
		pgid, err = unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		return err
	})
	if errors.Is(err, unix.ENOTTY) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if pgid < 0 {
		pgid = 0
	}
	return pgid, nil
}
