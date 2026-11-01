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

// usageDebounce is how long after a Codex session's last hook its rollout is read: Codex writes
// the token_count event of an answer around the hooks of the same turn, and a burst of hooks is
// one read. usageMaxWait caps how long a steady stream of hooks can put the read off.
const (
	usageDebounce = 2 * time.Second
	usageMaxWait  = 10 * time.Second
)

// usageRetries is how many times a read that ended mid-line is tried again, one debounce apart,
// with no hook: the token_count line was still being written.
const usageRetries = 3

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
	maxWait   time.Duration

	mu     sync.Mutex
	closed bool
	timers map[string]*usageDue   // by Codex session id: a read is due
	read   map[string]rolloutMark // by Codex session id: what the last read saw
	warned map[string]bool        // log lines already written
}

// usageDue is a read that is due: its timer, when the first hook of the burst came, and how many
// reads of this run ended mid-line.
type usageDue struct {
	t     *time.Timer
	first time.Time
	tries int
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
	if u.maxWait <= 0 {
		u.maxWait = usageMaxWait
	}
	u.timers, u.read, u.warned = map[string]*usageDue{}, map[string]rolloutMark{}, map[string]bool{}
	return u
}

// Close stops the reads that are due. A read already running finishes.
func (u *usageService) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closed = true
	for id, d := range u.timers {
		d.t.Stop()
		delete(u.timers, id)
	}
}

// warn logs a line once per gff-hab run. Past 256 distinct lines, a new line is dropped.
func (u *usageService) warn(format string, args ...any) {
	key := fmt.Sprintf(format, args...)
	u.mu.Lock()
	first := !u.warned[key] && len(u.warned) < 256
	if first {
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

// kick reads a Codex session's rollout once its hooks pause for the debounce, and at the latest
// maxWait after the first hook of the burst.
func (u *usageService) kick(native string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.schedule(native, nil)
}

// schedule sets the read of a session's rollout, under u.mu. retry is the run whose read ended
// mid-line, or nil for a hook.
func (u *usageService) schedule(native string, retry *usageDue) {
	if u.closed {
		return
	}
	now := time.Now()
	d := u.timers[native]
	switch {
	case d != nil && d.t.Stop(): // a read is due: put it off, within the max wait
	case d != nil || len(u.timers) < 256: // none due, or it has fired: a new run
		d = &usageDue{first: now}
		if retry != nil {
			d.tries = retry.tries + 1
		}
		u.timers[native] = d
	default:
		return
	}
	wait := max(0, min(u.debounce, d.first.Add(u.maxWait).Sub(now)))
	d.t = time.AfterFunc(wait, func() {
		u.mu.Lock()
		if u.timers[native] == d {
			delete(u.timers, native)
		}
		closed := u.closed
		u.mu.Unlock()
		if closed || u.codex(native) {
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if d.tries < usageRetries && u.timers[native] == nil {
			u.schedule(native, d)
		}
	})
}

// codex reads the end of a Codex session's rollout and stores its windows. It reports false
// when the read ended mid-line, so the last token_count may still be coming; else true. Only a
// read that ended on a newline marks the file as read.
func (u *usageService) codex(native string) (complete bool) {
	path, err := u.find(native)
	if err != nil {
		return true // not written yet; the next hook tries again
	}
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	mark := rolloutMark{path, info.Size()}
	u.mu.Lock()
	seen := u.read[native] == mark
	u.mu.Unlock()
	if seen {
		return true
	}
	off := max(0, mark.size-usageTail)
	buf := make([]byte, mark.size-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return true
	}
	buf = buf[:n]
	complete = n > 0 && buf[n-1] == '\n'
	if complete {
		u.mu.Lock()
		if len(u.read) >= 1024 {
			clear(u.read)
		}
		u.read[native] = mark
		u.mu.Unlock()
	}
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
	return complete
}
