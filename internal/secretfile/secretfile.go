// Package secretfile reads and creates the small owner-only secret files in gff-hab's state
// directory: the hook key and the hab's host key. A secret file must be a regular file, not a
// symlink, owned by this user, closed to group and others, and within a size bound. The checks
// run on the open file, so the checks and the read see the same file. A new secret appears
// complete or not at all, and racing creators agree on one. The package uses the standard
// library only.
package secretfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Read reads the secret file at path after the checks. It refuses a file larger than max bytes.
// A missing file gives an error that matches os.ErrNotExist.
func Read(path string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is open to group or others (mode %v); want 0600", path, fi.Mode().Perm())
	}
	if err := checkOwner(fi); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s holds %d bytes, more than %d", path, fi.Size(), max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s grew past %d bytes while read", path, max)
	}
	return b, nil
}

// LoadOrCreate reads dir/name with Read, or, when it does not exist, writes make's bytes there
// with mode 0600 and returns them with created true. dir is created with mode 0700 when missing.
// The new file is written whole to a temp file and hard-linked into place: the link fails when
// the file exists, so a racing creator never sees a half-written secret, and the first one wins.
// A file that exists but fails the checks is an error; it is never replaced.
func LoadOrCreate(dir, name string, max int64, make func() ([]byte, error)) (data []byte, created bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, name)
	b, err := Read(path, max)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return b, false, err
	}
	b, err = make()
	if err != nil {
		return nil, false, err
	}
	f, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return nil, false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, false, err
	}
	if err := os.Link(tmp, path); errors.Is(err, os.ErrExist) {
		b, err := Read(path, max) // another process made it first
		return b, false, err
	} else if err != nil {
		return nil, false, err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return b, true, nil
}
