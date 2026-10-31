package holderreg

import "golang.org/x/sys/unix"

// peerUID returns the uid of the process at the other end of a Unix socket.
func peerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}
