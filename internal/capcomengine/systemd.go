package capcomengine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runner runs a command and returns its output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// SystemdJob runs the engine as a transient systemd user unit of its own (3.2). A process
// gff-hab starts itself would sit in gff-hab's own unit, and a restart of that unit would end
// it; a unit of its own outlives gff-hab and its updates.
type SystemdJob struct {
	Unit    string   // the unit name, such as <prefix>-gff-hab-capcom.service
	EnvFile string   // the job's environment file (3.3)
	Keep    []string // names of gff-hab's variables the engine gets too (PATH, HOME, the locale)
	Run     Runner   // nil: the real systemctl and systemd-run
}

func (j SystemdJob) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if j.Run != nil {
		return j.Run(ctx, name, args...)
	}
	return execRunner(ctx, name, args...)
}

// Args are the systemd-run arguments for spec: the engine's own unit, its folder, its log, the
// environment file and the kept variables. The route token stays in the file.
func (j SystemdJob) Args(spec JobSpec) []string {
	args := []string{"--user", "--unit=" + j.Unit, "--collect", "--quiet",
		"--property=WorkingDirectory=" + spec.Dir,
		"--property=EnvironmentFile=" + j.EnvFile,
		"--property=StandardOutput=append:" + spec.Log,
		"--property=StandardError=append:" + spec.Log,
		"--property=KillMode=mixed"}
	keep := map[string]bool{}
	for _, k := range j.Keep {
		keep[k] = true
	}
	for _, kv := range spec.Env {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "LC_") {
			args = append(args, "--setenv="+kv)
		}
	}
	return append(append(args, "--"), spec.Argv...)
}

// Start starts the unit and returns its main process.
func (j SystemdJob) Start(ctx context.Context, spec JobSpec) (Proc, error) {
	if out, err := j.run(ctx, "systemd-run", j.Args(spec)...); err != nil {
		return nil, fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p, ok := j.Adopt(ctx); ok {
			return p, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("systemd-run started no process for " + j.Unit)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Adopt finds the unit's main process while the unit runs.
func (j SystemdJob) Adopt(ctx context.Context) (Proc, bool) {
	out, err := j.run(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", j.Unit)
	if err != nil {
		return nil, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 || !alive(pid) {
		return nil, false
	}
	p := &unitProc{job: j, process: process{pid: pid, done: make(chan struct{})}}
	go func() {
		for alive(pid) {
			time.Sleep(250 * time.Millisecond)
		}
		close(p.done)
	}()
	return p, true
}

type unitProc struct {
	job SystemdJob
	process
}

// Stop stops the unit: systemd sends SIGTERM, then SIGKILL after its own timeout.
func (p *unitProc) Stop(grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace+10*time.Second)
	defer cancel()
	if _, err := p.job.run(ctx, "systemctl", "--user", "stop", p.job.Unit); err != nil {
		syscall.Kill(p.pid, syscall.SIGKILL)
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("capcom engine: unit %s did not stop", p.job.Unit)
	}
}
