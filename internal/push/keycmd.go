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
	var key *ecdsa.PrivateKey
	err := RunSecret(ctx, "push key_command", k.Argv, k.Timeout, k.MaxOutput, func(out []byte) error {
		var err error
		if key, err = ParseKey(out); err != nil {
			return fmt.Errorf("printed no APNs key: %v", err)
		}
		return nil
	})
	return key, err
}

// RunSecret runs a command that prints a secret (docs/push.md): the argv with no shell,
// gff-hab's environment, stdin from /dev/null and its own process group, killed with the
// group past timeout (0: KeyCommandTimeout), its stdout capped at limit bytes (0:
// maxKeyOutput). use reads stdout, which is wiped when use returns; its error is reported as
// "<what> <program> <error>". Every error names what, the program, the exit status and at
// most a cut first line of stderr; never stdout.
func RunSecret(ctx context.Context, what string, argv []string, timeout time.Duration, limit int, use func([]byte) error) error {
	return runSecret(ctx, what, argv, timeout, limit, true, use)
}

// RunSecretNoStderr is RunSecret whose errors never quote stderr, for a command whose stderr
// may echo a short secret that stderrHint cannot tell from a message (an MCP token).
func RunSecretNoStderr(ctx context.Context, what string, argv []string, timeout time.Duration, limit int, use func([]byte) error) error {
	return runSecret(ctx, what, argv, timeout, limit, false, use)
}

func runSecret(ctx context.Context, what string, argv []string, timeout time.Duration, limit int, quote bool, use func([]byte) error) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("%s names no program", what)
	}
	if timeout <= 0 {
		timeout = KeyCommandTimeout
	}
	if limit <= 0 {
		limit = maxKeyOutput
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
	name := argv[0]
	err := cmd.Run()
	defer stdout.wipe()
	defer stderr.wipe()
	// A program that exits 0 but leaves a child holding stdout (an agent it started) has
	// printed all it will: WaitDelay closed the pipe, and the output stands.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	switch {
	case stdout.over:
		return fmt.Errorf("%s %s printed more than %d bytes", what, name, limit)
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Errorf("%s %s ran past %v", what, name, timeout)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				err = fmt.Errorf("killed by signal %v", ws.Signal())
			} else {
				err = fmt.Errorf("exit status %d", ee.ExitCode())
			}
		}
		if line := stderrHint(stderr.buf.Bytes()); quote && line != "" {
			return fmt.Errorf("%s %s: %v: %s", what, name, err, line)
		}
		return fmt.Errorf("%s %s: %v", what, name, err)
	}
	if err := use(stdout.buf.Bytes()); err != nil {
		return fmt.Errorf("%s %s %v", what, name, err)
	}
	return nil
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

// Keyring holds the key the command printed, and decides when the command may run again. One
// run at a time; a caller that asks during a run shares its result. Runs use the keyring's own
// context, ended by Close, with the command's own timeout: never a caller's shorter deadline.
// Safe for concurrent use.
type Keyring struct {
	run  func(context.Context) (*ecdsa.PrivateKey, error)
	now  func() time.Time
	logf func(string, ...any)
	ctx  context.Context
	stop context.CancelFunc

	mu      sync.Mutex
	waiters []chan keyResult // nil: no run is on
	key     *ecdsa.PrivateKey
	next    time.Time     // the earliest next run
	wait    time.Duration // the current failure backoff
	failing bool          // a failure has been logged since the last good run
}

type keyResult struct {
	changed bool
	err     error
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
	ctx, stop := context.WithCancel(context.Background())
	return &Keyring{run: run, now: now, logf: logf, ctx: ctx, stop: stop}
}

// Close ends a run that is on and refuses later ones. A run it ends logs nothing.
func (r *Keyring) Close() { r.stop() }

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

// Load runs the command when its backoff allows, and waits for it, or for ctx. It reports
// whether the key changed. A run skipped for the backoff returns errKeyWait; a call during a
// run waits for that run. A failure keeps the key held before, logs one line for the first
// failure since the last good run, and doubles the wait before the next run.
func (r *Keyring) Load(ctx context.Context) (changed bool, err error) {
	select {
	case res := <-r.refresh():
		return res.changed, res.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// refresh starts a run when the backoff allows, without waiting for it. The channel gets the
// run's result; at once errKeyWait inside the backoff.
func (r *Keyring) refresh() <-chan keyResult {
	ch := make(chan keyResult, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.waiters != nil:
		r.waiters = append(r.waiters, ch)
	case r.ctx.Err() != nil:
		ch <- keyResult{err: r.ctx.Err()}
	case r.now().Before(r.next):
		ch <- keyResult{err: errKeyWait}
	default:
		r.waiters = []chan keyResult{ch}
		go r.runOnce()
	}
	return ch
}

func (r *Keyring) runOnce() {
	key, err := r.run(r.ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.settle(key, err)
	for _, w := range r.waiters {
		w <- res
	}
	r.waiters = nil
}

// settle records a run's outcome. Called with mu held.
func (r *Keyring) settle(key *ecdsa.PrivateKey, err error) keyResult {
	if r.ctx.Err() != nil {
		return keyResult{err: r.ctx.Err()} // closing: no log, no backoff
	}
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
		return keyResult{err: err}
	}
	if r.failing {
		r.logf("push: the key command printed a key")
	}
	r.failing, r.wait = false, 0
	r.next = r.now().Add(keyRerunAfter)
	changed := r.key == nil || !r.key.Equal(key)
	if changed {
		r.key = key
	}
	return keyResult{changed: changed}
}
