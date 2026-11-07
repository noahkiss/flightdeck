package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/modhost"
)

// TestModuleHostProcess: the real gff-hab binary as the module host (modules.md 7.4). The
// sample module runs refresh, an action and render through it with its state in this process;
// the child killed by its pid fails the next call with a HostError, and after the restart the
// module answers again with the state it kept.
func TestModuleHostProcess(t *testing.T) {
	h := modhost.NewHost(modhost.HostOptions{
		Command:    func() *exec.Cmd { return exec.Command(bin, "module-host") },
		BackoffMin: 100 * time.Millisecond,
		Log:        func(l string) { t.Log(l) },
	})
	t.Cleanup(func() { h.Close() })
	b, err := modhost.Load(filepath.Join("..", "..", "internal", "modhost", "testdata", "bundles", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	store := &modhost.MemStore{}
	r, err := h.New(b, modhost.Options{Backends: modhost.Backends{State: store}})
	if err != nil {
		t.Fatal(err)
	}
	call := modhost.Call{Host: "box-a", OS: "linux", Place: "board", Vocabulary: 1}
	ctx := context.Background()
	if err := r.Act(ctx, "counter.tap", json.RawMessage(`{"by":3}`), call); err != nil {
		t.Fatalf("Act: %v", err)
	}
	tree, err := r.Render(ctx, call)
	if err != nil || !strings.Contains(string(tree), "box-a") {
		t.Fatalf("Render: %s, %v", tree, err)
	}
	pid := h.Pid()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var he *modhost.HostError
	sawDown := false
	for {
		tree, err = r.Render(ctx, call)
		if err == nil && h.Pid() != pid {
			break
		}
		if errors.As(err, &he) {
			sawDown = true
		} else if err != nil {
			t.Fatalf("Render after the kill: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no restart: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawDown {
		t.Error("no HostError while the module host was down")
	}
	st, _ := store.Load("hello")
	if string(st["count"]) != "3" || !strings.Contains(string(tree), "tapped 3") {
		t.Errorf("after the restart: state %s, tree %s", st["count"], tree)
	}

	// Run by hand it refuses arguments.
	if out, err := exec.Command(bin, "module-host", "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "usage") {
		t.Errorf("module-host x: %v %s", err, out)
	}
}
