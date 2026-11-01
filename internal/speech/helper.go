package speech

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/speechproto"
)

// stopGrace is how long a helper whose stdin closed may take to exit before it is killed.
const stopGrace = 5 * time.Second

// helper is one running speech helper process.
type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex // one frame at a time on stdin

	ready   chan struct{} // closed when the model is loaded
	done    chan struct{} // closed when the process ended
	replies chan speechproto.Frame

	mu      sync.Mutex
	hello   speechproto.Hello
	err     error // why it ended, or why the load failed
	stopped bool  // gff-hab stopped it on purpose
}

// helperInfo runs `gff-hab-speech -info` and reads the hello it prints.
func helperInfo(ctx context.Context, bin string) (speechproto.Hello, error) {
	var h speechproto.Hello
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-info")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return h, fmt.Errorf("%s -info: %w", bin, err)
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return h, fmt.Errorf("%s -info: %w", bin, err)
	}
	if h.Protocol != speechproto.Version {
		return h, fmt.Errorf("%s speaks protocol %d, this gff-hab speaks %d", bin, h.Protocol, speechproto.Version)
	}
	return h, nil
}

// startHelper starts the helper on the model in dir. It returns at once; ready closes when the
// load ends well, done when the process ends.
func startHelper(bin, dir string, threads int, logf func(string, ...any)) (*helper, error) {
	cmd := exec.Command(bin, "-model", dir, "-threads", strconv.Itoa(threads))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &helper{cmd: cmd, stdin: stdin, ready: make(chan struct{}), done: make(chan struct{}),
		replies: make(chan speechproto.Frame, 1)}
	logf("speech: helper started (pid %d)", cmd.Process.Pid)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { // the helper's own log lines; it never writes audio or text there
		defer readers.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 4096), 4096)
		for sc.Scan() {
			logf("speech helper: %s", sc.Text())
		}
		io.Copy(io.Discard, stderr)
	}()
	go func() {
		defer readers.Done()
		h.fail(h.read(bufio.NewReader(stdout)))
		io.Copy(io.Discard, stdout)
	}()
	go func() {
		readers.Wait()
		werr := cmd.Wait()
		h.mu.Lock()
		if h.err == nil {
			h.err = errors.New("the helper exited")
			if werr != nil {
				h.err = fmt.Errorf("the helper exited: %w", werr)
			}
		}
		h.mu.Unlock()
		close(h.done)
	}()
	return h, nil
}

// read reads the helper's frames until its stdout ends. It returns why it stopped.
func (h *helper) read(r io.Reader) error {
	f, err := speechproto.Read(r)
	if err != nil {
		return fmt.Errorf("no hello from the helper: %w", err)
	}
	var hello speechproto.Hello
	if f.Kind != speechproto.KindHello || json.Unmarshal(f.Payload, &hello) != nil {
		return errors.New("the helper's first frame is not a hello")
	}
	if hello.Protocol != speechproto.Version {
		return fmt.Errorf("the helper speaks protocol %d, this gff-hab speaks %d", hello.Protocol, speechproto.Version)
	}
	h.mu.Lock()
	h.hello = hello
	h.mu.Unlock()
	loaded := false
	for {
		f, err := speechproto.Read(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch f.Kind {
		case speechproto.KindReady:
			if !loaded {
				loaded = true
				close(h.ready)
			}
		case speechproto.KindError:
			var e speechproto.Error
			if json.Unmarshal(f.Payload, &e) != nil {
				return errors.New("the helper sent a bad error frame")
			}
			if e.ID == 0 {
				return fmt.Errorf("the model did not load: %s", e.Message)
			}
			h.reply(f)
		case speechproto.KindResult:
			h.reply(f)
		default:
			return fmt.Errorf("the helper sent frame kind %d", f.Kind)
		}
	}
}

// reply hands a result or error frame to the waiting request. A frame nobody waits for (a
// second answer) is dropped.
func (h *helper) reply(f speechproto.Frame) {
	select {
	case h.replies <- f:
	default:
	}
}

// fail records why the helper is ending and makes sure it ends.
func (h *helper) fail(err error) {
	h.mu.Lock()
	if h.err == nil && err != nil {
		h.err = err
	}
	h.mu.Unlock()
	if err != nil {
		h.kill()
	}
}

// failure returns why the helper ended.
func (h *helper) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return errors.New("the helper was stopped")
	}
	return h.err
}

// stoppedOnPurpose reports whether gff-hab stopped the helper (idle exit or close).
func (h *helper) stoppedOnPurpose() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

// send writes one transcribe frame.
func (h *helper) send(req speechproto.Request, audio []byte) error {
	p, err := speechproto.EncodeTranscribe(req, audio)
	if err != nil {
		return err
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	return speechproto.Write(h.stdin, speechproto.KindTranscribe, p)
}

// stop asks the helper to exit by closing its stdin, and kills it after stopGrace.
func (h *helper) stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.stdin.Close()
	go func() {
		select {
		case <-h.done:
		case <-time.After(stopGrace):
			h.kill()
		}
	}()
}

func (h *helper) kill() {
	if h.cmd.Process != nil {
		h.cmd.Process.Kill()
	}
}
