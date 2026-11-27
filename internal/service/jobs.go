package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// JobState is one job's state for a diagnostics bundle (diagnostics.md 8.2): four parsed fields,
// never the init system's raw output, which names paths and the environment.
type JobState struct {
	Job      string
	Loaded   bool
	Running  bool
	LastExit string // the exit status or signal of the last run; "" when unknown
	Runs     string // launchd's run count, systemd's restart count; "" when unknown
}

// JobStates reads the install record in state and returns the state of the logic job and of
// each holder instance's job. With no install record it returns nil and no error: gff-hab then
// runs by hand or under a test, and no init system is asked.
func JobStates(ctx context.Context, state string, instances []string, run Runner) ([]JobState, error) {
	c := Config{StateDir: state, UID: os.Getuid()}
	m, err := readManifest(c)
	if err != nil || m == nil {
		return nil, err
	}
	c.Init, c.Prefix = m.Init, m.Prefix
	jobs := []string{c.LogicJob()}
	for _, in := range instances {
		jobs = append(jobs, c.HolderJob(in))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []JobState
	for _, j := range jobs {
		switch c.Init {
		case Launchd:
			out = append(out, launchdState(ctx, c, run, j))
		case Systemd:
			out = append(out, systemdState(ctx, run, j))
		default:
			return nil, fmt.Errorf("service: install record names init %q", c.Init)
		}
	}
	return out, nil
}

// launchdState parses `launchctl print`: state, pid, last exit code, runs.
func launchdState(ctx context.Context, c Config, run Runner, label string) JobState {
	s := JobState{Job: label}
	b, err := launchctl(ctx, run, "print", c.target(label))
	if err != nil {
		return s
	}
	s.Loaded = true
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			s.Running = true
		case "last exit code":
			s.LastExit = word(v)
		case "runs":
			s.Runs = number(v)
		}
	}
	return s
}

// systemdState parses `systemctl --user show` for its load and active state, the main process's
// exit status and the restart count.
func systemdState(ctx context.Context, run Runner, unit string) JobState {
	s := JobState{Job: unit}
	b, err := systemctl(ctx, run, "show", "-p", "LoadState,ActiveState,ExecMainStatus,NRestarts", unit)
	if err != nil {
		return s
	}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			s.Loaded = v == "loaded"
		case "ActiveState":
			s.Running = v == "active" || v == "activating" || v == "reloading"
		case "ExecMainStatus":
			s.LastExit = number(v)
		case "NRestarts":
			s.Runs = number(v)
		}
	}
	return s
}

// word keeps a short status word or number, such as "0" or "(never exited)"; anything else is
// unknown.
func word(v string) string {
	if len(v) > 40 || strings.ContainsAny(v, "/\\\"=") {
		return ""
	}
	return v
}

func number(v string) string {
	if _, err := strconv.Atoi(v); err != nil {
		return ""
	}
	return v
}

// String is the job's line in service.txt.
func (s JobState) String() string {
	or := func(v string) string {
		if v == "" {
			return "unknown"
		}
		return v
	}
	return fmt.Sprintf("%s: loaded %v, running %v, last exit %s, runs %s", s.Job, s.Loaded, s.Running, or(s.LastExit), or(s.Runs))
}
