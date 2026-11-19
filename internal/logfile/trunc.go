package logfile

import (
	"errors"
	"io"
	"os"
)

// CopyTruncate caps a file that another process holds open and writes, such as the service
// manager's capture of a process's stderr (4.3). Once path is over max bytes, its old files
// shift (Shift), its content is copied to path.1, and path is truncated in place. The writer
// opened it for append, so its next line lands at the new end. Lines written during the copy
// may be lost. It reports whether it truncated. A missing file is not an error.
func CopyTruncate(path string, max int64, keep int) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() || st.Size() <= max {
		return false, nil
	}
	if keep > 0 {
		// Shift the old files only: path itself stays, as the writer's file.
		os.Remove(Old(path, keep+1))
		for k := keep; k >= 2; k-- {
			os.Rename(Old(path, k-1), Old(path, k))
		}
		if err := copyFile(path, Old(path, 1)); err != nil {
			return false, err
		}
	}
	return true, os.Truncate(path, 0)
}

// RenameCap renames path to path.1 (Shift) once it is over max bytes. It is for a file whose
// writers open it fresh for each line, such as hook.log (4.4). It reports whether it renamed.
func RenameCap(path string, max int64, keep int) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st.Size() <= max {
		return false, nil
	}
	Shift(path, keep)
	return true, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, to)
}
