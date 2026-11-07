package capcomengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// JobSpec is one start of the engine job: its argv, folder, environment and log file.
type JobSpec struct {
	Argv []string
	Dir  string
	Env  []string // the whole environment of the engine process
	Log  string   // stdout and stderr go here, appended
}

// Job starts and finds the engine job. Each init system has one (3.2); ProcessJob runs the
// engine as a plain process, for a hand-run gff-hab with no init system and for tests.
type Job interface {
	// Start starts the engine. It returns once the process runs.
	Start(ctx context.Context, spec JobSpec) (Proc, error)
	// Adopt finds an engine job that runs now, started by an earlier gff-hab.
	Adopt(ctx context.Context) (Proc, bool)
}

// Proc is one running engine.
type Proc interface {
	// Done is closed when the process has ended.
	Done() <-chan struct{}
	// Stop ends the process: SIGTERM, then SIGKILL after grace.
	Stop(grace time.Duration) error
	Pid() int
}

// ProcessJob runs the engine as a process of its own process group, so a signal to gff-hab's
// group never reaches it, and records its pid in PidFile so a later gff-hab adopts it.
type ProcessJob struct {
	PidFile string
	// Match reports whether a live pid is the engine (its command line). Adopt takes the pid
	// file's process only when it matches, so a pid reused after a reboot is never adopted or
	// signalled. Nil adopts any live pid (tests).
	Match func(pid int) bool
}

type process struct {
	pid  int
	done chan struct{}
}

func (p *process) Done() <-chan struct{} { return p.done }
func (p *process) Pid() int              { return p.pid }

func (p *process) Stop(grace time.Duration) error {
	syscall.Kill(p.pid, syscall.SIGTERM)
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}
	syscall.Kill(p.pid, syscall.SIGKILL)
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("capcom engine: pid %d did not end", p.pid)
	}
}

// Start starts spec.Argv in spec.Dir with spec.Env, output appended to spec.Log.
func (j ProcessJob) Start(ctx context.Context, spec JobSpec) (Proc, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("capcom engine: no command")
	}
	log, err := os.OpenFile(spec.Log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Dir, spec.Env, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		os.Remove(j.PidFile)
		close(p.done)
	}()
	if j.PidFile != "" {
		if err := os.WriteFile(j.PidFile, []byte(strconv.Itoa(p.pid)+"\n"), 0o600); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Adopt finds the process the pid file names, while it lives.
func (j ProcessJob) Adopt(context.Context) (Proc, bool) {
	b, err := os.ReadFile(j.PidFile)
	if err != nil {
		return nil, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || !alive(pid) || (j.Match != nil && !j.Match(pid)) {
		os.Remove(j.PidFile)
		return nil, false
	}
	p := &process{pid: pid, done: make(chan struct{})}
	go func() {
		for alive(pid) {
			time.Sleep(250 * time.Millisecond)
		}
		os.Remove(j.PidFile)
		close(p.done)
	}()
	return p, true
}

// alive reports whether pid runs, and is not a zombie this process could reap.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// LastLine returns the last non-empty line of the log, cut to 200 bytes, for the session row's
// error (3.2). The engine never writes its credential there.
func LastLine(log string) string {
	f, err := os.Open(log)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(0, fi.Size()-4096)
	buf := make([]byte, fi.Size()-off)
	f.ReadAt(buf, off)
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

// EngineDir is the engine's own folder under the Capcom folder (3.4).
func EngineDir(capcomDir string) string { return filepath.Join(capcomDir, "engine") }
