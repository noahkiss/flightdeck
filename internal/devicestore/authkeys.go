// Package devicestore keeps the devices a hab has paired (docs/specs/transport.md 6.1.4): the
// hab's own device list in the state directory, and the one line per device that pairing adds
// to an authorized_keys file for the system sshd.
//
// Edits to authorized_keys append or remove only Flightdeck's own lines. Every other byte of the
// file stays as it was, in the same order.
package devicestore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// CommentPrefix starts the comment of every line pairing writes. Revoke removes only lines whose
// comment starts with it.
const CommentPrefix = "flightdeck:"

// maxAuthorizedKeys bounds the authorized_keys file read into memory.
const maxAuthorizedKeys = 4 << 20

// maxName is the longest device name kept, in runes.
const maxName = 64

// CleanName makes a device name safe to print and to put in a key comment: control and format
// characters (bidi marks among them) are dropped, any run of white space becomes one space, and
// the result is cut to 64 runes. It may return "".
func CleanName(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cs, r) || r == unicode.ReplacementChar {
			continue
		}
		if space && b.Len() > 0 {
			if n == maxName {
				break
			}
			b.WriteByte(' ')
			n++
		}
		space = false
		if n == maxName {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// KeyLine is the authorized_keys line for a paired device: `restrict`, the forced command when
// one is given, the key, and a comment that starts with CommentPrefix and names the device.
func KeyLine(pub ssh.PublicKey, command, name string, now time.Time) (string, error) {
	var opts []string
	opts = append(opts, "restrict")
	if command != "" {
		if strings.ContainsAny(command, "\"\\\r\n") {
			return "", fmt.Errorf("the forced command %q holds a quote, a backslash or a line break", command)
		}
		opts = append(opts, `command="`+command+`"`)
	}
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	name = CleanName(name)
	if name == "" {
		name = "device"
	}
	return fmt.Sprintf("%s %s %s %s (paired %s)", strings.Join(opts, ","), key, CommentPrefix, name, now.Format("2006-01-02")), nil
}

// lines splits b after each newline, keeping every byte.
func lines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// lineKey parses one authorized_keys line. ok is false for a blank line, a comment or a line it
// cannot parse.
func lineKey(l []byte) (pub ssh.PublicKey, comment string, ok bool) {
	t := bytes.TrimRight(l, "\r\n")
	if len(bytes.TrimSpace(t)) == 0 || bytes.HasPrefix(bytes.TrimSpace(t), []byte("#")) {
		return nil, "", false
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey(t)
	if err != nil || len(bytes.TrimSpace(rest)) > 0 {
		return nil, "", false
	}
	return pub, comment, true
}

func sameKey(a, b ssh.PublicKey) bool { return bytes.Equal(a.Marshal(), b.Marshal()) }

// errChanged means the file changed between the read and the rename.
var errChanged = errors.New("the file changed while it was being written")

// AppendAuthorizedKey appends line to the authorized_keys file at path, unless a line already
// holds pub. It returns whether it added the line. A missing file is made; a missing parent
// folder is made at 0700. The file ends at mode 0600.
func AppendAuthorizedKey(path string, pub ssh.PublicKey, line string) (added bool, err error) {
	if strings.ContainsAny(line, "\r\n") {
		return false, errors.New("the key line holds a line break")
	}
	err = retryEdit(path, func(old []byte) ([]byte, bool) {
		for _, l := range lines(old) {
			if k, _, ok := lineKey(l); ok && sameKey(k, pub) {
				return nil, false
			}
		}
		out := append([]byte(nil), old...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, line...)
		out = append(out, '\n')
		added = true
		return out, true
	})
	return added, err
}

// RemoveAuthorizedKey removes each line of the file at path that holds pub and whose comment
// starts with CommentPrefix. It returns how many it removed. A missing file removes nothing.
func RemoveAuthorizedKey(path string, pub ssh.PublicKey) (removed int, err error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	err = retryEdit(path, func(old []byte) ([]byte, bool) {
		removed = 0
		var out []byte
		for _, l := range lines(old) {
			if k, c, ok := lineKey(l); ok && sameKey(k, pub) && strings.HasPrefix(c, CommentPrefix) {
				removed++
				continue
			}
			out = append(out, l...)
		}
		return out, removed > 0
	})
	return removed, err
}

// retryEdit reads the file, asks edit for the new bytes, and writes them through a temporary
// file and a rename, again from the start when the file changed meanwhile (three tries).
func retryEdit(path string, edit func(old []byte) (out []byte, write bool)) error {
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, lerr := os.Lstat(path); lerr == nil {
			return fmt.Errorf("%s is a link to a file that does not exist", path)
		}
		real = path
		if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for range 3 {
		old, err := readSmall(real)
		if err != nil {
			return err
		}
		out, write := edit(old)
		if !write {
			return nil
		}
		err = writeAtomic(real, old, out)
		if errors.Is(err, errChanged) {
			continue
		}
		return err
	}
	return fmt.Errorf("%s: %w, three times", real, errChanged)
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
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
	if fi.Size() > maxAuthorizedKeys {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxAuthorizedKeys)
	}
	return os.ReadFile(path)
}

// writeAtomic writes out to a temporary file beside path at 0600 and renames it over path, unless
// path no longer holds old.
func writeAtomic(path string, old, out []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".flightdeck-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	now, err := readSmall(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(now, old) {
		return errChanged
	}
	return os.Rename(name, path)
}
