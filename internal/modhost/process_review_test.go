package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bigHab answers a read with a result larger than one pipe message.
type bigHab struct{}

func (bigHab) Get(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"` + strings.Repeat("x", maxFrame+1) + `"`), nil
}

func (bigHab) Act(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

type bigExec struct{}

func (bigExec) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	return ExecResult{Stdout: make([]byte, 20<<20)}, nil
}

// TestHostBadInput: malformed action args and settings JSON cannot hold fail the call, as in
// process, and never panic gff-hab's side.
func TestHostBadInput(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{})
	r, err := h.New(writeBundle(t, "args", "", `exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function () {};`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Act(context.Background(), "x", json.RawMessage(`{bad`), fixedCall); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Errorf("bad args: %v", err)
	}
	if err := r.Act(context.Background(), "x", nil, fixedCall); err != nil {
		t.Errorf("no args: %v", err)
	}
	if _, err := h.New(writeBundle(t, "nan", "", `exports.render = function () {};`), Options{Settings: map[string]any{"n": math.NaN()}}); err == nil {
		t.Error("NaN in settings loaded")
	}
}

// TestHostOversizedAnswers: a backend result over the pipe's message limit fails its bridge
// call at once, and the module and the child live on; exec output is cut before the pipe.
func TestHostOversizedAnswers(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{})
	r, err := h.New(writeBundle(t, "big", `"exec"`, `
exports.render = function () { return { type: "text", text: "x" }; };
exports.refresh = function () { fd.hab.get("structure.get"); };
exports.act = function () { var r = fd.exec(["x"]); if (!r.stdout_cut) throw new Error("not cut"); };
`), Options{Backends: Backends{Hab: bigHab{}, Exec: bigExec{}}})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.Pid()
	start := time.Now()
	var se *ScriptError
	if err := r.Refresh(context.Background(), fixedCall); !errors.As(err, &se) || !strings.Contains(se.Message, "too large") {
		t.Errorf("oversized hab answer: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
	if err := r.Act(context.Background(), "x", nil, fixedCall); err != nil {
		t.Errorf("exec output: %v", err)
	}
	if _, err := r.Render(context.Background(), fixedCall); err != nil || h.Pid() != pid {
		t.Errorf("after: %v, pid %d -> %d", err, pid, h.Pid())
	}
}

// TestHostWriteRacesCrash: a call sent while the child dies gets a HostError, never a raw pipe
// error.
func TestHostWriteRacesCrash(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{BackoffMin: 10 * time.Millisecond, BackoffMax: 10 * time.Millisecond})
	r, err := h.New(writeBundle(t, "racer", "", `exports.render = function () { return { type: "text", text: "x" }; };`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		waitFor(t, 5*time.Second, func() bool { _, err := r.Render(context.Background(), fixedCall); return err == nil })
		if err := syscall.Kill(h.Pid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for range 20 {
			if _, err := r.Render(context.Background(), fixedCall); err != nil && !errors.Is(err, ErrHostDown) {
				t.Fatalf("err = %v, want a HostError", err)
			}
		}
	}
}

// TestHostHasDoesNotWait: Has answers while a long call runs.
func TestHostHasDoesNotWait(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{})
	release := make(chan struct{})
	r, err := h.New(writeBundle(t, "busy", `"exec"`, `exports.render = function () { return { type: "text", text: "x" }; };
exports.refresh = function () { fd.exec(["x"]); };`), Options{Backends: Backends{Exec: &blockingExec{release: release}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Refresh(context.Background(), fixedCall) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if !r.Has("refresh") || r.Has("act") {
		t.Error("Has is wrong")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Has waited %s", d)
	}
	close(release)
	if err := <-done; err != nil {
		t.Error(err)
	}
}
