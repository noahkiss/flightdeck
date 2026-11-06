package summary

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/push"
)

// Cache bounds (app.md 16.5, Caching).
const (
	cacheSize = 256
	cacheTTL  = time.Hour
)

// Key command bounds: an API key is one line, well under 4 KiB.
const maxKeyOutput = 4 << 10

// Key command backoff, as push's (docs/push.md): after a failed run the next waits 30 s,
// doubling to 30 minutes; after a good run, a re-run on a refused key waits 5 minutes.
const (
	keyRetryFirst = 30 * time.Second
	keyRetryMax   = 30 * time.Minute
	keyRerunAfter = 5 * time.Minute
)

// ErrNoKey reports that the key command has not printed a key yet.
var ErrNoKey = errors.New("summaries: the key command has not printed a key")

// Options are a Service's seams. All may be zero.
type Options struct {
	HTTP *http.Client // nil: a client with CallTimeout
	Now  func() time.Time
	Logf func(string, ...any)
	// KeyRun replaces running key_command; for tests.
	KeyRun func(ctx context.Context) (string, error)
}

// Service asks the endpoint for summaries, with a cache, the API key and one log line per
// streak of failures. Safe for concurrent use.
type Service struct {
	s    Settings
	hc   *http.Client
	now  func() time.Time
	logf func(string, ...any)

	ctx  context.Context
	stop context.CancelFunc

	keyRun func(ctx context.Context) (string, error)

	mu       sync.Mutex
	key      string
	keyBusy  bool
	keyNext  time.Time
	keyWait  time.Duration
	keyFails bool
	cache    map[[32]byte]entry
	failing  bool
}

type entry struct {
	line string
	at   time.Time
}

// New returns a service for s, which must be On, and starts loading the key in the
// background when s has a key_command.
func New(s Settings, o Options) *Service {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: CallTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if s.Wait <= 0 {
		s.Wait = DefaultWait
	}
	v := &Service{s: s, hc: o.HTTP, now: o.Now, logf: o.Logf, keyRun: o.KeyRun, cache: map[[32]byte]entry{}}
	v.ctx, v.stop = context.WithCancel(context.Background())
	if v.keyRun == nil && s.KeyCommand != nil {
		argv := s.KeyCommand
		v.keyRun = func(ctx context.Context) (string, error) {
			var key string
			err := push.RunSecret(ctx, "summaries key_command", argv, 0, maxKeyOutput, func(out []byte) error {
				for _, l := range strings.Split(string(out), "\n") {
					if l = strings.TrimSpace(l); l != "" {
						key = strings.Clone(l)
						return nil
					}
				}
				return errors.New("printed no key")
			})
			if err != nil {
				// The command can put its secret anywhere on stderr, including short tokens.
				return "", errors.New("summaries key_command failed")
			}
			return key, nil
		}
	}
	if v.keyRun != nil {
		v.mu.Lock()
		v.loadKeyLocked()
		v.mu.Unlock()
	}
	return v
}

// Wait is how long a banner waits for its summary.
func (v *Service) Wait() time.Duration { return v.s.Wait }

// Close ends a key command run that is on; later summaries fail at once.
func (v *Service) Close() { v.stop() }

// loadKeyLocked starts a key command run when none is on and the backoff allows. Called with
// mu held.
func (v *Service) loadKeyLocked() {
	if v.keyBusy || v.ctx.Err() != nil || v.now().Before(v.keyNext) {
		return
	}
	v.keyBusy = true
	go func() {
		key, err := v.keyRun(v.ctx)
		v.mu.Lock()
		defer v.mu.Unlock()
		v.keyBusy = false
		if v.ctx.Err() != nil {
			return
		}
		if err != nil || key == "" {
			if err == nil {
				err = errors.New("summaries key_command printed no key")
			}
			v.keyWait = min(max(2*v.keyWait, keyRetryFirst), keyRetryMax)
			v.keyNext = v.now().Add(v.keyWait)
			if !v.keyFails {
				v.keyFails = true
				v.logf("summaries are off: %v; trying again in %v, backing off to %v", err, v.keyWait, keyRetryMax)
			}
			return
		}
		if v.keyFails || v.key == "" {
			v.logf("summaries are on: model %s", v.s.Model)
		}
		v.key, v.keyFails, v.keyWait = key, false, 0
		v.keyNext = v.now().Add(keyRerunAfter)
	}()
}

// Summarize returns the summary for in, from the cache or one call to the endpoint. The call
// ends at ctx or CallTimeout, whichever comes first. A failure is not cached.
func (v *Service) Summarize(ctx context.Context, in Input) (string, error) {
	user, err := json.Marshal(in.bounded())
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(append([]byte(v.s.Model+"\x00"), user...))
	v.mu.Lock()
	if e, ok := v.cache[h]; ok && v.now().Sub(e.at) < cacheTTL {
		v.mu.Unlock()
		return e.line, nil
	}
	key := v.key
	if v.keyRun != nil && key == "" {
		v.loadKeyLocked()
		v.mu.Unlock()
		return "", ErrNoKey
	}
	v.mu.Unlock()
	if v.ctx.Err() != nil {
		return "", v.ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	stop := context.AfterFunc(v.ctx, cancel)
	defer stop()
	line, err := call(ctx, v.hc, v.s, key, user)

	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err // the caller gave up, or the hab closes: not the endpoint's fault
		}
		// A refused key: run the key command again (within its backoff), as push does on
		// InvalidProviderToken. The key held stays until a new one is printed.
		var se *StatusError
		if errors.As(err, &se) && (se.Code == 401 || se.Code == 403) && v.keyRun != nil {
			v.loadKeyLocked()
		}
		if !v.failing {
			v.failing = true
			v.logf("summaries: %v; banners use their plain text until it works again", describe(err))
		}
		return "", err
	}
	if v.failing {
		v.failing = false
		v.logf("summaries: the endpoint answers again")
	}
	v.putLocked(h, line)
	return line, nil
}

// describe names an error for the log.
func describe(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the endpoint did not answer within " + CallTimeout.String()
	}
	var se *StatusError
	if errors.As(err, &se) && se.Code == 400 {
		return err.Error() + ` (an endpoint that refuses reasoning_effort or max_tokens: try thinking = "default")`
	}
	return err.Error()
}

// putLocked caches a line, dropping expired entries and then the oldest past cacheSize.
func (v *Service) putLocked(h [32]byte, line string) {
	now := v.now()
	if len(v.cache) >= cacheSize {
		var oldest [32]byte
		var at time.Time
		for k, e := range v.cache {
			if now.Sub(e.at) >= cacheTTL {
				delete(v.cache, k)
				continue
			}
			if at.IsZero() || e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		if len(v.cache) >= cacheSize {
			delete(v.cache, oldest)
		}
	}
	v.cache[h] = entry{line: line, at: now}
}
