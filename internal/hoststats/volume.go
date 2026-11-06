package hoststats

import (
	"os"
	"path/filepath"
)

// Volume is the free and total bytes of the file system that holds path, as df counts them:
// free is what an ordinary user may still write. A path that does not exist yet is read at its
// nearest existing parent, so a folder made later counts on the volume it will be made on. ok
// is false when no reading could be taken.
func Volume(path string) (free, total uint64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	p := filepath.Clean(path)
	for {
		if _, err := os.Stat(p); err == nil {
			return statVolume(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, 0, false
		}
		p = parent
	}
}
