// Package holderreg is the holder registry: one small JSON file per live holder in a per-user
// directory (docs/specs/hab-holder.md 4.2). Each holder writes and removes its own file;
// gff-hab lists the directory to find every holder and prunes files that outlived their holder.
//
// One file per holder, rather than one shared file, means holders that start and exit
// independently never contend for a lock.
//
// Layout of the directory:
//
//	<instance>.json         the registry file of holder <instance>
//	<instance>.sock         its socket (the socket path is also in the file)
//	.tmp-<instance>-<rand>  a registry file being written; renamed into place when complete
//	.tmp-rm-<instance>-<rand>  a file being removed; renamed aside first, see Remove
//
// The package is pure Go and has no state.
package holderreg

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Suffix is the registry file extension.
const Suffix = ".json"

// tmpPrefix starts the name of a registry file that is still being written. List ignores such
// files; Prune removes them once they are older than TmpGrace.
const tmpPrefix = ".tmp-"

// TmpGrace is how old a temporary or unreadable file must be before Prune removes it. A holder
// writes and renames its file within milliseconds, so anything older was left by a crash.
const TmpGrace = time.Minute

// Protocol is the range of protocol versions a holder speaks.
type Protocol struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
}

// Entry is the content of one registry file.
type Entry struct {
	Instance string    `json:"instance"`
	Build    string    `json:"build"`
	Protocol Protocol  `json:"protocol"`
	Pid      int       `json:"pid"`
	Socket   string    `json:"socket"`
	Started  time.Time `json:"started"`
	Draining bool      `json:"draining"`
}

// Path returns the registry file path of instance in dir.
func Path(dir, instance string) string { return filepath.Join(dir, instance+Suffix) }

// ErrNoDir means no holder directory could be chosen: on Linux, neither $GFF_HOLDER_DIR nor
// $XDG_RUNTIME_DIR is set.
var ErrNoDir = errors.New("no socket directory: set XDG_RUNTIME_DIR or GFF_HOLDER_DIR, or pass -dir or -socket")

// DefaultDir picks the per-user holder directory (spec 4.2): $GFF_HOLDER_DIR when set; on macOS,
// gff/holders under configDir (the user's Application Support directory); elsewhere,
// $XDG_RUNTIME_DIR/gff/holders. With no runtime directory it fails rather than fall back to a
// shared directory. The holder and gff-hab both use it, so they agree.
func DefaultDir(goos string, getenv func(string) string, configDir func() (string, error)) (string, error) {
	if d := getenv("GFF_HOLDER_DIR"); d != "" {
		return d, nil
	}
	if goos == "darwin" {
		d, err := configDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "gff", "holders"), nil
	}
	if d := getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "gff", "holders"), nil
	}
	return "", ErrNoDir
}

// SocketPath returns the conventional socket path of instance in dir.
func SocketPath(dir, instance string) string { return filepath.Join(dir, instance+".sock") }

// ValidInstance reports whether id can name a registry file: non-empty, no path separator, no
// leading dot, and short enough to keep the socket path within macOS's 104-byte limit.
func ValidInstance(id string) bool {
	return id != "" && len(id) <= 64 && !strings.HasPrefix(id, ".") &&
		!strings.ContainsAny(id, "/\\\x00")
}

// Write writes e's registry file in dir atomically: a temporary file in the same directory,
// synced, then renamed over <instance>.json. A reader sees the old file or the new one, never
// a partial one. The file gets mode 0600.
func Write(dir string, e Entry) error {
	if !ValidInstance(e.Instance) {
		return fmt.Errorf("holderreg: bad instance id %q", e.Instance)
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, tmpPrefix+e.Instance+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, Path(dir, e.Instance)); err != nil {
		return err
	}
	ok = true
	return nil
}

// Read reads one registry file.
func Read(path string) (Entry, error) {
	e, err := readAs(path, strings.TrimSuffix(filepath.Base(path), Suffix))
	if err == nil && filepath.Base(path) != e.Instance+Suffix {
		err = fmt.Errorf("holderreg: %s: instance %q does not match the file name", path, e.Instance)
	}
	return e, err
}

// readAs reads a registry file that must describe instance, whatever the file is called now.
func readAs(path, instance string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("holderreg: %s: %w", path, err)
	}
	if !ValidInstance(e.Instance) || e.Instance != instance {
		return e, fmt.Errorf("holderreg: %s: instance %q does not match the file name", path, e.Instance)
	}
	return e, nil
}

// List reads every registry file in dir, oldest start first. Files it cannot read are reported
// in errs and left alone; Prune deals with them. A missing directory is an empty registry.
func List(dir string) (entries []Entry, errs []error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, Suffix) || de.IsDir() {
			continue
		}
		e, err := Read(filepath.Join(dir, name))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) { // removed while we listed
				errs = append(errs, err)
			}
			continue
		}
		entries = append(entries, e)
	}
	slices.SortStableFunc(entries, func(a, b Entry) int { return a.Started.Compare(b.Started) })
	return entries, errs
}

// Remove removes e's registry file in dir, but only while the file still describes the same
// holder (same instance, pid and start time). A holder restarted under the same instance id
// writes a new file, and a reader that decided the old one was stale must not remove the new
// one. A missing file is not an error.
//
// Reading the file and then removing it would leave a window in which a restarted holder's new
// file is removed. So Remove first renames the file aside (one atomic step), then reads what it
// took. When that is a newer holder's file, Remove puts it back, unless an even newer one has
// been written meanwhile.
func Remove(dir string, e Entry) error {
	if !ValidInstance(e.Instance) {
		return fmt.Errorf("holderreg: bad instance id %q", e.Instance)
	}
	path := Path(dir, e.Instance)
	return removeAside(dir, path, e.Instance, func(aside string) bool {
		cur, err := readAs(aside, e.Instance)
		return err != nil || (cur.Pid == e.Pid && cur.Started.Equal(e.Started))
	})
}

// hookAside runs after removeAside has moved a file aside and before it decides. Tests use it.
var hookAside func(path string)

// removeAside renames path to a temporary name in dir and asks gone whether what it took may go.
// If not, it links the file back to path, which fails harmlessly when a newer file is there.
func removeAside(dir, path, instance string, gone func(aside string) bool) error {
	aside := filepath.Join(dir, tmpPrefix+"rm-"+instance+"-"+randSuffix())
	if err := os.Rename(path, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if hookAside != nil {
		hookAside(path)
	}
	if !gone(aside) {
		if err := os.Link(aside, path); err != nil && !errors.Is(err, os.ErrExist) {
			// No hard links here: rename back. A file written meanwhile is newer still, so
			// prefer it.
			if _, serr := os.Lstat(path); errors.Is(serr, os.ErrNotExist) {
				return os.Rename(aside, path)
			}
		}
	}
	if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func randSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Alive reports whether a process with pid exists. EPERM means it exists under another user.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Refused reports whether a dial error means nothing listens on the socket: the socket file is
// gone, or no process accepts on it.
func Refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// Stale reports whether e has outlived its holder, and why. The rule (spec 4.2): the pid is not
// alive, or the socket refuses connections. A pid that is alive may belong to an unrelated
// process that reused it, so the socket decides then. probe dials the socket; with probe false
// only the pid is checked, for callers that are about to dial anyway.
func Stale(e Entry, probe bool) (bool, string) {
	if !Alive(e.Pid) {
		return true, fmt.Sprintf("pid %d is not running", e.Pid)
	}
	if !probe {
		return false, ""
	}
	c, err := net.DialTimeout("unix", e.Socket, time.Second)
	if err != nil {
		if Refused(err) {
			return true, fmt.Sprintf("socket refuses connections: %v", err)
		}
		return false, ""
	}
	c.Close()
	return false, ""
}

// RemoveStale removes a stale holder's registry file and, when it lies in dir and refuses
// connections, its socket file. Call it only for an entry Stale reported. The socket is renamed
// aside before it is probed, so a socket a restarted holder created meanwhile answers the probe
// and is put back, never removed.
func RemoveStale(dir string, e Entry) error {
	if err := Remove(dir, e); err != nil {
		return err
	}
	if filepath.Dir(e.Socket) != filepath.Clean(dir) || e.Socket != SocketPath(dir, e.Instance) {
		return nil // not ours to remove
	}
	return removeAside(dir, e.Socket, e.Instance, func(aside string) bool {
		c, err := net.DialTimeout("unix", aside, time.Second)
		if err == nil {
			c.Close() // a new holder listens on it
			return false
		}
		return Refused(err)
	})
}

// Prune removes what dead holders left in dir: registry files Stale reports (with probe), their
// sockets, and the junk PruneJunk removes. Dialling a live holder's socket to probe it costs
// that holder one connection that ends without a hello. It returns the entries it removed.
func Prune(dir string, now time.Time) ([]Entry, error) {
	entries, _ := List(dir)
	var removed []Entry
	var errs []error
	for _, e := range entries {
		if stale, _ := Stale(e, true); stale {
			if err := RemoveStale(dir, e); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, e)
		}
	}
	if err := PruneJunk(dir, now); err != nil {
		errs = append(errs, err)
	}
	return removed, errors.Join(errs...)
}

// PruneJunk removes temporary files and unreadable registry files in dir that are older than
// TmpGrace. It dials nothing.
func PruneJunk(dir string, now time.Time) error {
	var errs []error
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, de := range des {
		name := de.Name()
		path := filepath.Join(dir, name)
		var junk bool
		switch {
		case strings.HasPrefix(name, tmpPrefix):
			junk = true
		case !strings.HasPrefix(name, ".") && strings.HasSuffix(name, Suffix):
			_, rerr := Read(path)
			junk = rerr != nil && !errors.Is(rerr, os.ErrNotExist)
		}
		if !junk {
			continue
		}
		if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) >= TmpGrace {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
