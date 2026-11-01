package habconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
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

// Stderr returns the last lines the command wrote to its standard error.
func (s *commandStream) Stderr() string { return strings.TrimSpace(s.stderr.String()) }

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
	// ControlDir, when set, turns on OpenSSH connection sharing with control sockets in this
	// directory, so every stream after the first is a session on one SSH connection. The
	// directory must exist and be private to the user. Empty: one SSH connection per stream.
	ControlDir string
	// Env, when set, replaces ssh's environment.
	Env []string
}

// Args returns the ssh command line for one stream, without the program name.
func (t SSHTransport) Args() ([]string, error) {
	if t.Host == "" {
		return nil, errors.New("habconn: ssh transport has no host")
	}
	if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host, " \t\r\n") {
		return nil, errors.New("habconn: ssh host " + t.Host + " is not a destination")
	}
	args := append([]string{}, t.Options...)
	args = append(args,
		"-T",                  // no PTY: the stream carries bytes, not a terminal
		"-o", "BatchMode=yes", // never prompt: stdin is the protocol, not a person
		"-o", "ConnectTimeout=10", // a dead endpoint fails fast
		"-o", "ServerAliveInterval=15", // ssh notices a dead link on idle data streams
		"-o", "ServerAliveCountMax=3",
	)
	if t.ControlDir != "" {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+strings.TrimRight(t.ControlDir, "/")+"/%C",
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

// Dial starts one ssh process.
func (t SSHTransport) Dial(ctx context.Context, kind Kind) (io.ReadWriteCloser, error) {
	args, err := t.Args()
	if err != nil {
		return nil, err
	}
	prog := t.SSH
	if prog == "" {
		prog = "ssh"
	}
	return CommandTransport{Argv: append([]string{prog}, args...), Env: t.Env}.Dial(ctx, kind)
}
