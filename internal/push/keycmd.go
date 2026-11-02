package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// Key command limits (docs/push.md).
const (
	// KeyCommandTimeout bounds one run of the key command.
	KeyCommandTimeout = 30 * time.Second
	// maxKeyOutput bounds the command's stdout; a real .p8 key is about 250 bytes.
	maxKeyOutput = 16 << 10
	// maxKeyStderr is how much stderr is kept, for the first line of an error.
	maxKeyStderr = 4 << 10
	// stderrLine is the longest stderr line an error quotes.
	stderrLine = 120
)

// Keyring backoff: after a failed run the next waits 30 s, doubling to 30 minutes. After a
// good run, a re-run on a rejected token waits at least 5 minutes.
const (
	keyRetryFirst = 30 * time.Second
	keyRetryMax   = 30 * time.Minute
	keyRerunAfter = 5 * time.Minute
)

// ErrNoKey reports that the key command has not printed a usable key yet.
var ErrNoKey = errors.New("push key not loaded")

// errKeyWait reports a run skipped for the backoff.
var errKeyWait = errors.New("push key command waits out its backoff")

// KeyCommand runs the command that prints the APNs key (docs/push.md). It runs the argv with
// no shell, gff-hab's environment, stdin from /dev/null and its own process group. The key it
// reads stays in memory: it is never logged and never written to disk.
type KeyCommand struct {
	Argv      []string
	Timeout   time.Duration // 0: KeyCommandTimeout
	MaxOutput int           // 0: maxKeyOutput
}

// Run runs the command once and parses its stdout as a .p8 key. An error names the program,
// the exit status and at most a cut first line of stderr; never stdout.
func (k KeyCommand) Run(ctx context.Context) (*ecdsa.PrivateKey, error) {
	if len(k.Argv) == 0 || k.Argv[0] == "" {
		return nil, errors.New("push key_command names no program")
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = KeyCommandTimeout
	}
	limit := k.MaxOutput
	if limit <= 0 {
		limit = maxKeyOutput
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.Argv[0], k.Argv[1:]...)
	cmd.Stdin = nil // /dev/null: no password prompt can wait on a terminal
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the whole group
	}
	cmd.WaitDelay = 2 * time.Second
	stdout := &capBuffer{limit: limit, cancel: cancel}
	stderr := &capBuffer{limit: maxKeyStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	name := k.Argv[0]
	err := cmd.Run()
	defer stdout.wipe()
	switch {
	case stdout.over:
		return nil, fmt.Errorf("push key_command %s printed more than %d bytes", name, limit)
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("push key_command %s ran past %v", name, timeout)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("exit status %d", ee.ExitCode())
		}
		if line := stderrHint(stderr.buf.Bytes()); line != "" {
			return nil, fmt.Errorf("push key_command %s: %v: %s", name, err, line)
		}
		return nil, fmt.Errorf("push key_command %s: %v", name, err)
	}
	key, err := ParseKey(stdout.buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("push key_command %s printed no APNs key: %v", name, err)
	}
	return key, nil
}

// stderrHint returns the first non-empty line of stderr, cut to stderrLine bytes, or "" when
// it may hold key material: a PEM marker or a long run of base64.
func stderrHint(b []byte) string {
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if strings.Contains(line, "-----") || strings.Contains(line, "PRIVATE KEY") {
		return ""
	}
	run := 0
	for _, r := range line {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '/' || r == '=') {
			if run++; run >= 32 {
				return ""
			}
		} else {
			run = 0
		}
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return '?'
		}
		return r
	}, line)
	if len(line) > stderrLine {
		cut := stderrLine
		for cut > 0 && !utf8Start(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	return line
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// capBuffer keeps at most limit bytes. Past it, it sets over and calls cancel, which stops the
// command.
type capBuffer struct {
	limit  int
	cancel func()
	buf    bytes.Buffer
	over   bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		if !c.over {
			c.over = true
			if c.cancel != nil {
				c.cancel()
			}
		}
		return len(p), nil // keep draining; the cancel ends the command
	}
	return c.buf.Write(p)
}

// wipe overwrites what the buffer held.
func (c *capBuffer) wipe() {
	b := c.buf.Bytes()
	clear(b[:cap(b)])
	c.buf.Reset()
}

// Keyring holds the key the command printed, and decides when the command may run again. Safe
// for concurrent use.
type Keyring struct {
	run  func(context.Context) (*ecdsa.PrivateKey, error)
	now  func() time.Time
	logf func(string, ...any)

	mu      sync.Mutex
	running bool
	key     *ecdsa.PrivateKey
	next    time.Time     // the earliest next run
	wait    time.Duration // the current failure backoff
	failing bool          // a failure has been logged since the last good run
}

// NewKeyring returns a keyring for the command. now and logf may be nil.
func NewKeyring(cmd KeyCommand, now func() time.Time, logf func(string, ...any)) *Keyring {
	return newKeyring(cmd.Run, now, logf)
}

func newKeyring(run func(context.Context) (*ecdsa.PrivateKey, error), now func() time.Time, logf func(string, ...any)) *Keyring {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Keyring{run: run, now: now, logf: logf}
}

// Key returns the key, or nil before the command printed one.
func (r *Keyring) Key() *ecdsa.PrivateKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.key
}

// Next returns how long until the command may run again; 0 when it may run now.
func (r *Keyring) Next() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return max(r.next.Sub(r.now()), 0)
}

// Load runs the command when its backoff allows, and keeps the key it prints. It reports
// whether the key changed. A run skipped for the backoff, or while another run is on, returns
// errKeyWait. A failure keeps the key held before, logs one line for the first failure since
// the last good run, and doubles the wait before the next run.
func (r *Keyring) Load(ctx context.Context) (changed bool, err error) {
	r.mu.Lock()
	if r.running || r.now().Before(r.next) {
		r.mu.Unlock()
		return false, errKeyWait
	}
	r.running = true
	r.mu.Unlock()

	key, err := r.run(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	if err != nil {
		r.wait = min(max(2*r.wait, keyRetryFirst), keyRetryMax)
		r.next = r.now().Add(r.wait)
		if !r.failing {
			r.failing = true
			state := "push is off"
			if r.key != nil {
				state = "push keeps its last key"
			}
			r.logf("%s: %v; trying again in %v, backing off to %v (docs/push.md)", state, err, r.wait, keyRetryMax)
		}
		return false, err
	}
	if r.failing {
		r.logf("push: the key command printed a key")
	}
	r.failing, r.wait = false, 0
	r.next = r.now().Add(keyRerunAfter)
	changed = r.key == nil || !r.key.Equal(key)
	r.key = key
	return changed, nil
}
