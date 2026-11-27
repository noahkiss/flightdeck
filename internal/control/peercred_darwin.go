package control

import "golang.org/x/sys/unix"

// peerUID returns the uid of the process at the other end of a Unix socket.
func peerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}

// peerPID returns the pid of the process at the other end of a Unix socket: LOCAL_PEERPID. Read
// at accept, before the caller says hello, it names the process that connected. Whether macOS
// moves it to another process that uses the socket later is not relied on: the hab reads it
// once.
func peerPID(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
