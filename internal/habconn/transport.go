package habconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// UnixTransport reaches a hab on this machine through its control socket
// (docs/specs/hab-logic-api.md 4.1).
type UnixTransport struct {
	Path string
}

// Dial opens one connection to the socket.
func (t UnixTransport) Dial(ctx context.Context, _ Kind) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", t.Path)
}

// CommandTransport runs a command per stream and speaks over its stdin and stdout, as
// `gff-hab api call -via` does. Argv[0] is looked up in PATH; no shell runs.
type CommandTransport struct {
	Argv []string
	// Env, when set, replaces the command's environment.
	Env []string
	// CloseGrace is how long Close waits for the command to exit after its stdin closes
	// before it kills it. Default 5 s.
	CloseGrace time.Duration
}

// Dial starts the command.
func (t CommandTransport) Dial(ctx context.Context, _ Kind) (io.ReadWriteCloser, error) {
	if len(t.Argv) == 0 {
		return nil, errors.New("habconn: command transport has no command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(t.Argv[0], t.Argv[1:]...)
	cmd.Env = t.Env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &commandStream{cmd: cmd, in: in, out: out, done: make(chan struct{})}
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	grace := t.CloseGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	go func() {
		// Wait closes stdout once the command exited, which unblocks a pending Read.
		cmd.Wait()
		close(s.done)
	}()
	s.grace = grace
	return s, nil
}

// commandStream is a running command's stdio.
type commandStream struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.ReadCloser
	stderr tail
	grace  time.Duration
	done   chan struct{} // closed once the command exited
	once   sync.Once
}

func (s *commandStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *commandStream) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close closes the command's stdin, so `gff-hab connect` half-closes and the hab ends the
// connection, and its stdout, which unblocks a pending Read at once. A command still running
// after the grace period is killed. Close does not wait.
func (s *commandStream) Close() error {
	s.once.Do(func() {
		s.in.Close()
		s.out.Close()
		go func() {
			t := time.NewTimer(s.grace)
			defer t.Stop()
			select {
			case <-s.done:
			case <-t.C:
				s.cmd.Process.Kill()
			}
		}()
	})
	return nil
}

// stderrWait bounds how long Stderr waits for the command to exit.
const stderrWait = 500 * time.Millisecond

// Stderr returns the last lines the command wrote to its standard error. It waits up to
// stderrWait for the command to exit first: ssh prints why it failed as it exits, and the
// copy from its stderr ends only then.
func (s *commandStream) Stderr() string {
	t := time.NewTimer(stderrWait)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
	}
	return strings.TrimSpace(s.stderr.String())
}

// tail keeps the last 4 KiB written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailMax = 4 << 10

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailMax {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-tailMax:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// SSHTransport runs the system ssh, one process per stream, with `gff-hab connect` on the far
// side (docs/specs/hab-logic-api.md 4.2). The user's OpenSSH config, known-hosts files, agent
// and keys apply unchanged. Why the system ssh and not an in-process client:
// docs/specs/gff-client.md 11.7.
//
// Connection sharing is on unless NoSharing is set: the first stream starts an OpenSSH master
// with its control socket in ControlDir, and every later stream is a session on that one SSH
// connection.
type SSHTransport struct {
	// Host is the ssh destination: a host alias from the user's config, a name, or user@name.
	Host string
	// SSH is the ssh program. Default "ssh".
	SSH string
	// Options go on the command line before the transport's own -o options. OpenSSH keeps the
	// first value it reads for an option, so these override the defaults below.
	Options []string
	// Remote is the command run on the host. Default `gff-hab connect`. Its words are sent as
	// they are: the remote shell parses them.
	Remote []string
	// ControlDir holds the control sockets. Empty: DefaultControlDir(). Dial creates it with
	// mode 0700 when it is missing, and refuses one that another user owns, that others may
	// enter, or whose socket paths would not fit (MaxControlDir).
	ControlDir string
	// NoSharing turns connection sharing off: one SSH connection per stream.
	NoSharing bool
	// Env, when set, replaces ssh's environment.
	Env []string
}

// MaxControlDir is the longest control directory, in bytes, whose socket paths fit in macOS's
// 104-byte sun_path with its closing NUL: the directory, a slash, OpenSSH's %C (40 hex
// characters) and the 17-character suffix of the master's temporary socket. The check applies
// on every platform, so a setup that works on Linux also works on a Mac.
const MaxControlDir = 104 - 1 - 1 - 40 - 17

// DefaultControlDir is where the control sockets go when ControlDir is empty: gff/ssh under
// $XDG_RUNTIME_DIR when that is set and short enough, else gff-ssh-<uid> under /tmp. macOS's
// own temporary directory is too long for socket paths, so /tmp serves there.
func DefaultControlDir() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(rt) {
		if d := filepath.Join(rt, "gff", "ssh"); len(d) <= MaxControlDir {
			return d
		}
	}
	return "/tmp/gff-ssh-" + strconv.Itoa(os.Getuid())
}

// controlDir returns the directory for the control sockets, checked for length only.
func (t SSHTransport) controlDir() (string, error) {
	d := t.ControlDir
	if d == "" {
		d = DefaultControlDir()
	}
	if d != "/" {
		d = strings.TrimRight(d, "/")
	}
	if !filepath.IsAbs(d) {
		return "", fmt.Errorf("habconn: ssh control directory %q is not an absolute path", d)
	}
	if len(d) > MaxControlDir {
		return "", fmt.Errorf("habconn: ssh control directory %s is %d bytes long; its socket paths would pass the 104-byte limit (at most %d bytes)", d, len(d), MaxControlDir)
	}
	return d, nil
}

// prepareControlDir creates the control directory when it is missing and checks that only
// this user can reach it: the sockets in it open the SSH connection without a key.
func (t SSHTransport) prepareControlDir() (string, error) {
	d, err := t.controlDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("habconn: ssh control directory: %w", err)
	}
	fi, err := os.Lstat(d)
	if err != nil {
		return "", fmt.Errorf("habconn: ssh control directory: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("habconn: ssh control directory %s is not a directory", d)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("habconn: ssh control directory %s belongs to another user (uid %d)", d, st.Uid)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("habconn: ssh control directory %s has mode %04o; want 0700", d, perm)
	}
	return d, nil
}

// checkHost refuses a destination ssh would read as an option or that holds a space.
func (t SSHTransport) checkHost() error {
	if t.Host == "" {
		return errors.New("habconn: ssh transport has no host")
	}
	if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host, " \t\r\n") {
		return errors.New("habconn: ssh host " + t.Host + " is not a destination")
	}
	return nil
}

// Args returns the ssh command line for one stream, without the program name.
func (t SSHTransport) Args() ([]string, error) {
	if err := t.checkHost(); err != nil {
		return nil, err
	}
	args := append([]string{}, t.Options...)
	args = append(args,
		"-T",                  // no PTY: the stream carries bytes, not a terminal
		"-o", "BatchMode=yes", // never prompt: stdin is the protocol, not a person
		"-o", "ConnectTimeout=10", // a dead endpoint fails fast
		"-o", "ServerAliveInterval=15", // ssh notices a dead link on idle data streams
		"-o", "ServerAliveCountMax=3",
	)
	if !t.NoSharing {
		d, err := t.controlDir()
		if err != nil {
			return nil, err
		}
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(d, "%C"),
			"-o", "ControlPersist=60",
		)
	}
	remote := t.Remote
	if len(remote) == 0 {
		remote = []string{"gff-hab", "connect"}
	}
	args = append(args, "--", t.Host)
	return append(args, remote...), nil
}

func (t SSHTransport) prog() string {
	if t.SSH == "" {
		return "ssh"
	}
	return t.SSH
}

// Dial starts one ssh process.
func (t SSHTransport) Dial(ctx context.Context, kind Kind) (io.ReadWriteCloser, error) {
	args, err := t.Args()
	if err != nil {
		return nil, err
	}
	if !t.NoSharing {
		if _, err := t.prepareControlDir(); err != nil {
			return nil, err
		}
	}
	return CommandTransport{Argv: append([]string{t.prog()}, args...), Env: t.Env}.Dial(ctx, kind)
}

// Reset tells the shared connection's master to exit (`ssh -O exit`), so the next stream
// starts a fresh SSH connection. A master whose link died while the machine slept still takes
// new sessions, and ConnectTimeout does not apply to them; without a reset each reconnect
// would wait until ssh's own keepalive gave up. With sharing off, Reset does nothing. An error
// means no master was running, or it did not answer within ctx.
func (t SSHTransport) Reset(ctx context.Context) error {
	if t.NoSharing {
		return nil
	}
	if err := t.checkHost(); err != nil {
		return err
	}
	d, err := t.controlDir()
	if err != nil {
		return err
	}
	args := append([]string{}, t.Options...)
	args = append(args, "-o", "BatchMode=yes", "-o", "ControlPath="+filepath.Join(d, "%C"), "-O", "exit", "--", t.Host)
	cmd := exec.CommandContext(ctx, t.prog(), args...)
	cmd.Env = t.Env
	var stderr tail
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("habconn: ssh -O exit: %w (%s)", err, msg)
		}
		return fmt.Errorf("habconn: ssh -O exit: %w", err)
	}
	return nil
}
