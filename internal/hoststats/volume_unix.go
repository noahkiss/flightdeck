//go:build linux || darwin

package hoststats

import "golang.org/x/sys/unix"

// statVolume reads the file system that holds path.
func statVolume(path string) (free, total uint64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	_, total = diskFigures(uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bavail), bsize)
	if total == 0 {
		return 0, 0, false
	}
	return uint64(st.Bavail) * bsize, total, true
}
