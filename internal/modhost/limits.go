package modhost

import (
	"context"
	"runtime"
	"runtime/metrics"
	"time"
)

// Limits are the guards of modules.md 8. DefaultLimits returns the spec's values.
type Limits struct {
	Load    time.Duration // running main.js's top level
	Render  time.Duration
	Refresh time.Duration
	Act     time.Duration
	// Memory is the Go heap, in bytes, above which a call is interrupted. The heap is the
	// process's, so it bounds the module host as a whole; the host's own resident-memory kill
	// stays the hard limit (modules.md 8.2). Zero turns the watch off.
	Memory uint64
	State  int // bytes of one module's state as JSON, keys included
	Output int // bytes of each exec stream and of a fetch body
	// Stack is the deepest JS call stack, so deep recursion fails with a RangeError instead of
	// growing the goroutine stack.
	Stack int
}

// DefaultLimits are modules.md 8.1 and 8.2's values.
func DefaultLimits() Limits {
	return Limits{
		Load:    time.Second,
		Render:  250 * time.Millisecond,
		Refresh: 30 * time.Second,
		Act:     10 * time.Second,
		Memory:  256 << 20,
		State:   256 << 10,
		Output:  1 << 20,
		Stack:   1000,
	}
}

// withDefaults fills each zero field from DefaultLimits, except Memory: zero Limits are the
// defaults, while a Limits with other fields set and Memory zero turns the watch off.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l == (Limits{}) {
		return d
	}
	if l.Load <= 0 {
		l.Load = d.Load
	}
	if l.Render <= 0 {
		l.Render = d.Render
	}
	if l.Refresh <= 0 {
		l.Refresh = d.Refresh
	}
	if l.Act <= 0 {
		l.Act = d.Act
	}
	if l.State <= 0 {
		l.State = d.State
	}
	if l.Output <= 0 {
		l.Output = d.Output
	}
	if l.Stack <= 0 {
		l.Stack = d.Stack
	}
	return l
}

// memoryPoll is how often a running call samples the heap.
const memoryPoll = 5 * time.Millisecond

// heapBytes is the heap memory held by objects, live or not yet collected.
func heapBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// watchMemory samples the heap until ctx ends, and calls over once when the heap stays above
// limit after a collection. A collection runs at most every 100 ms, so garbage alone never
// trips the limit and a busy module does not spend its time collecting.
func watchMemory(ctx context.Context, limit uint64, over func()) {
	t := time.NewTicker(memoryPoll)
	defer t.Stop()
	var lastGC time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if heapBytes() <= limit {
			continue
		}
		if time.Since(lastGC) < 100*time.Millisecond {
			continue
		}
		runtime.GC()
		lastGC = time.Now()
		if heapBytes() > limit {
			over()
			return
		}
	}
}
