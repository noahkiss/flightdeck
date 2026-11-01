package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	habusage "github.com/noahkiss/flightdeck/internal/usage"
)

// usageTail is how much of the end of a Codex rollout one read takes. A token_count event comes
// after every model answer, so the latest one is near the end.
const usageTail = 256 << 10

// usageDebounce is how long after a Codex hook the rollout is read: Codex writes the token_count
// event of an answer around the hooks of the same turn, and a burst of hooks is one read.
const usageDebounce = 2 * time.Second

// usageService feeds the usage store from the harness adapters (harness-adapters.md, Usage):
// Claude Code's status-line input, posted to the hook endpoint by `gff-hab statusline`, and
// the token_count events at the end of each Codex session's rollout, read after its hooks.
type usageService struct {
	store     *habusage.Store
	host      string
	codexHome string
	find      func(native string) (string, error) // a Codex session's rollout
	logf      func(string, ...any)
	debounce  time.Duration

	mu     sync.Mutex
	closed bool
	timers map[string]*time.Timer // by Codex session id: a read is due
	read   map[string]rolloutMark // by Codex session id: what the last read saw
	warned map[string]bool        // log lines already written
}

// rolloutMark is a rollout's path and size at its last read: an unchanged file is not read again.
type rolloutMark struct {
	path string
	size int64
}

func newUsageService(u *usageService) *usageService {
	if u.logf == nil {
		u.logf = func(string, ...any) {}
	}
	if u.debounce <= 0 {
		u.debounce = usageDebounce
	}
	u.timers, u.read, u.warned = map[string]*time.Timer{}, map[string]rolloutMark{}, map[string]bool{}
	return u
}

// Close stops the reads that are due. A read already running finishes.
func (u *usageService) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closed = true
	for id, t := range u.timers {
		t.Stop()
		delete(u.timers, id)
	}
}

// warn logs a line once per gff-hab run.
func (u *usageService) warn(format string, args ...any) {
	key := fmt.Sprintf(format, args...)
	u.mu.Lock()
	first := !u.warned[key]
	if first && len(u.warned) < 256 {
		u.warned[key] = true
	}
	u.mu.Unlock()
	if first {
		u.logf("%s", key)
	}
}

// Hook takes the envelopes that carry usage. It reports whether it consumed env: a status-line
// envelope is usage only and never reaches the agent state. A Codex hook names its session,
// whose rollout is then read after the debounce; the agent state gets the envelope as usual.
func (u *usageService) Hook(env harness.Envelope) (consumed bool) {
	if env.Event == hook.StatusLineEvent {
		if env.Harness == detect.KindClaude {
			u.claude(env)
		}
		return true
	}
	if env.Harness == detect.KindCodex && u.find != nil {
		var p struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(env.Payload, &p) == nil && p.SessionID != "" {
			u.kick(p.SessionID)
		}
	}
	return false
}

// claude reads one status-line input.
func (u *usageService) claude(env harness.Envelope) {
	at := time.UnixMilli(env.T)
	st, err := habusage.ParseClaudeStatus(env.Payload, at, u.host)
	if err != nil {
		u.warn("usage: claude: %v", err)
		return
	}
	u.version("Claude Code", st.Version, habusage.ClaudeTestedMin, habusage.ClaudeTestedMax)
	for _, d := range st.Drift {
		u.warn("usage: claude status line: %s; that window is left out", d)
	}
	u.store.Put(st.Reading)
}

// version logs once a harness version outside the range the parser was tested against.
func (u *usageService) version(name, v, lo, hi string) {
	if v == "" {
		return
	}
	if detect.CompareVersions(v, lo) < 0 || detect.CompareVersions(v, hi) > 0 {
		u.warn("usage: %s %s is outside the tested range %s to %s; readings that do not parse are left out", name, v, lo, hi)
	}
}

// kick reads a Codex session's rollout after the debounce, unless a read is already due.
func (u *usageService) kick(native string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.timers[native] != nil || len(u.timers) >= 256 {
		return
	}
	u.timers[native] = time.AfterFunc(u.debounce, func() {
		u.mu.Lock()
		delete(u.timers, native)
		closed := u.closed
		u.mu.Unlock()
		if !closed {
			u.codex(native)
		}
	})
}

// codex reads the end of a Codex session's rollout and stores its windows.
func (u *usageService) codex(native string) {
	path, err := u.find(native)
	if err != nil {
		return // not written yet; the next hook tries again
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	mark := rolloutMark{path, info.Size()}
	u.mu.Lock()
	if u.read[native] == mark {
		u.mu.Unlock()
		return
	}
	if len(u.read) >= 1024 {
		clear(u.read)
	}
	u.read[native] = mark
	u.mu.Unlock()
	off := max(0, mark.size-usageTail)
	buf := make([]byte, mark.size-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return
	}
	buf = buf[:n]
	if off > 0 { // the first line is cut: start after it
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}
	cu := habusage.ParseCodexRollout(buf, u.host, u.codexHome)
	u.version("Codex", cu.Version, habusage.CodexTestedMin, habusage.CodexTestedMax)
	for _, d := range cu.Drift {
		u.warn("usage: codex rollout: %s; that window is left out", d)
	}
	u.store.Put(cu.Reading)
}
