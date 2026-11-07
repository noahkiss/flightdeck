package capcom

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// The engines that can run the Capcom service (capcom-service.md 3.5).
const (
	EngineNone   = ""       // the pane Capcom runs (12.4, until A8)
	EngineClaude = "claude" // the Claude Agent SDK engine
	EngineCodex  = "codex"  // the Codex SDK engine (A6)
)

// The logins an engine takes (capcom-service.md 10.2). The Claude engine takes oauth_token (its
// default) or api_key; the Codex engine takes login (its default: the user's own `codex login`)
// or api_key.
const (
	AuthOAuthToken = "oauth_token"
	AuthAPIKey     = "api_key"
	AuthLogin      = "login"
)

// ServiceCapability is the capability a hab that runs the Capcom service sends
// (capcom-service.md 7.4).
const ServiceCapability = "capcom-thread"

// Limits are the [capcom] limit keys of capcom-service.md 19.2 that A2 and A4 read, with their
// defaults applied.
type Limits struct {
	MaxTurns        int
	MaxTurnUSD      float64
	DailyUSD        float64 // 0: none
	RestartDelay    time.Duration
	CrashLimit      int
	CrashWindow     time.Duration
	RouteBackoffMax time.Duration
	TypingTimeout   time.Duration
	CommandTimeout  time.Duration
	TokenRetry      time.Duration
	WakeRetry       time.Duration

	// The context manager's keys (A4, 6).
	SummaryMaxChars   int
	SummaryInputChars int
	SummaryQuiet      time.Duration
	ReseedAt          float64 // a share of the model's window, 0 to 1
	ReseedEarlyAt     float64 // the size trigger for an engine with no compaction events (4.1)
	ReseedQuiet       time.Duration
	ReseedQuietAt     float64
	SeedMessages      int
	SeedMessagesChars int
	SeedLogbookBytes  int
	PrecompactTimeout time.Duration
}

// DefaultLimits are 19.2's defaults.
var DefaultLimits = Limits{
	MaxTurns:        50,
	MaxTurnUSD:      2,
	RestartDelay:    5 * time.Second,
	CrashLimit:      5,
	CrashWindow:     10 * time.Minute,
	RouteBackoffMax: 30 * time.Second,
	TypingTimeout:   10 * time.Minute,
	CommandTimeout:  30 * time.Second,
	TokenRetry:      5 * time.Minute,
	WakeRetry:       10 * time.Second,

	SummaryMaxChars:   6000,
	SummaryInputChars: 64000,
	SummaryQuiet:      10 * time.Minute,
	ReseedAt:          0.60,
	ReseedEarlyAt:     0.45,
	ReseedQuiet:       60 * time.Minute,
	ReseedQuietAt:     0.25,
	SeedMessages:      6,
	SeedMessagesChars: 8000,
	SeedLogbookBytes:  8 << 10,
	PrecompactTimeout: 30 * time.Second,
}

// ServiceConfig is the service half of the [capcom] section (capcom-service.md 3.5, 19.2).
type ServiceConfig struct {
	Engine          string   // EngineNone, EngineClaude or EngineCodex
	Model           string   // "": the SDK's default
	SmallModel      string   // "": the provider's smallest current model
	Auth            string   // AuthOAuthToken or AuthAPIKey (claude); AuthLogin or AuthAPIKey (codex)
	OAuthCommand    []string // an argv that prints the OAuth token
	APIKeyCommand   []string // an argv that prints an API key
	Limits          Limits
	CredentialInKey bool // the file holds an oauth_token or api_key key: refused
}

// laterKeys are [capcom] keys later stages read (19.2, 8). They are known, so they are not
// logged as unknown.
var laterKeys = []string{"harness", "location_stale",
	"pace_daily_share", "pace_event_stop", "pace_small_at", "pace_wait_at", "weather", "maps", "places"}

type durRange struct {
	key      string
	dst      *time.Duration
	min, max time.Duration
}

type intRange struct {
	key      string
	dst      *int
	min, max int
}

// LoadServiceConfig reads the service keys of the [capcom] section. A missing file or section
// gives the defaults. Each bad value is one problem: it is left out and its default runs. An
// unknown key is a problem and is ignored. A file that does not parse is an error, with the
// defaults.
func LoadServiceConfig(path string) (ServiceConfig, []string, error) {
	c := ServiceConfig{Auth: AuthOAuthToken, Limits: DefaultLimits}
	if path == "" {
		return c, nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	var f struct {
		Capcom map[string]any `toml:"capcom"`
	}
	if _, err := toml.Decode(string(b), &f); err != nil {
		return c, nil, fmt.Errorf("%s: %v", path, err)
	}
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: [capcom] ", path)+fmt.Sprintf(format, args...))
	}
	sec := f.Capcom
	str := func(key string) (string, bool) {
		v, ok := sec[key]
		if !ok {
			return "", false
		}
		s, isStr := v.(string)
		if !isStr {
			bad("%s: want a string; using the default", key)
			return "", false
		}
		return s, true
	}
	if s, ok := str("engine"); ok {
		switch s {
		case EngineClaude, EngineCodex:
			c.Engine = s
		default:
			bad("engine %q: want %q or %q; the pane Capcom runs", s, EngineClaude, EngineCodex)
		}
	}
	if s, ok := str("model"); ok {
		c.Model = s
	}
	if s, ok := str("small_model"); ok {
		c.SmallModel = s
	}
	// The login: the engine's default unless auth names one it takes.
	if c.Engine == EngineCodex {
		c.Auth = AuthLogin
	}
	if s, ok := str("auth"); ok {
		switch {
		case s == AuthAPIKey, c.Engine == EngineCodex && s == AuthLogin, c.Engine != EngineCodex && s == AuthOAuthToken:
			c.Auth = s
		case c.Engine == EngineCodex:
			bad("auth %q: the codex engine takes %q or %q; using %s", s, AuthLogin, AuthAPIKey, AuthLogin)
		default:
			bad("auth %q: want %q or %q; using %s", s, AuthOAuthToken, AuthAPIKey, AuthOAuthToken)
		}
	}
	argv := func(key string) []string {
		v, ok := sec[key]
		if !ok {
			return nil
		}
		list, isList := v.([]any)
		out := make([]string, 0, len(list))
		for _, e := range list {
			s, isStr := e.(string)
			if !isStr {
				isList = false
				break
			}
			out = append(out, s)
		}
		if !isList || len(out) == 0 || out[0] == "" {
			bad("%s: want an argv, a list of strings such as [\"program\", \"arg\"]; no shell runs it", key)
			return nil
		}
		return out
	}
	c.OAuthCommand = argv("oauth_token_command")
	c.APIKeyCommand = argv("api_key_command")
	for _, k := range []string{"oauth_token", "api_key"} {
		if _, ok := sec[k]; ok {
			c.CredentialInKey = true
			bad("%s: the config file holds no credential; set %s_command to an argv that prints it", k, k)
		}
	}
	l := &c.Limits
	for _, r := range []intRange{
		{"max_turns", &l.MaxTurns, 1, 500},
		{"crash_limit", &l.CrashLimit, 1, 100},
		{"summary_max_chars", &l.SummaryMaxChars, 1000, 50000},
		{"summary_input_chars", &l.SummaryInputChars, 8000, 400000},
		{"seed_messages", &l.SeedMessages, 0, 50},
		{"seed_messages_chars", &l.SeedMessagesChars, 0, 64000},
		{"seed_logbook_bytes", &l.SeedLogbookBytes, 0, 64 << 10},
	} {
		v, ok := sec[r.key]
		if !ok {
			continue
		}
		n, isInt := v.(int64)
		if !isInt || n < int64(r.min) || n > int64(r.max) {
			bad("%s %v: want a whole number from %d to %d; using %d", r.key, v, r.min, r.max, *r.dst)
			continue
		}
		*r.dst = int(n)
	}
	for _, r := range []durRange{
		{"restart_delay", &l.RestartDelay, time.Second, 10 * time.Minute},
		{"crash_window", &l.CrashWindow, time.Minute, 24 * time.Hour},
		{"route_backoff_max", &l.RouteBackoffMax, time.Second, 10 * time.Minute},
		{"typing_timeout", &l.TypingTimeout, time.Minute, time.Hour},
		{"command_timeout", &l.CommandTimeout, time.Second, 5 * time.Minute},
		{"token_retry", &l.TokenRetry, 30 * time.Second, 24 * time.Hour},
		{"wake_retry", &l.WakeRetry, time.Second, 5 * time.Minute},
		{"summary_quiet", &l.SummaryQuiet, time.Minute, 24 * time.Hour},
		{"reseed_quiet", &l.ReseedQuiet, 10 * time.Minute, 24 * time.Hour},
		{"precompact_timeout", &l.PrecompactTimeout, 5 * time.Second, 5 * time.Minute},
	} {
		v, ok := sec[r.key]
		if !ok {
			continue
		}
		s, isStr := v.(string)
		d, err := time.ParseDuration(s)
		if !isStr || err != nil || d < r.min || d > r.max {
			bad("%s %v: want a duration such as \"%s\" from %s to %s; using %s", r.key, v, r.min, r.min, r.max, *r.dst)
			continue
		}
		*r.dst = d
	}
	for _, k := range []struct {
		key  string
		dst  *float64
		none bool
	}{{"max_turn_usd", &l.MaxTurnUSD, false}, {"daily_usd", &l.DailyUSD, true}} {
		v, ok := sec[k.key]
		if !ok {
			continue
		}
		var x float64
		switch n := v.(type) {
		case float64:
			x = n
		case int64:
			x = float64(n)
		default:
			x = math.NaN()
		}
		if !(x > 0) || math.IsInf(x, 0) {
			bad("%s %v: want a dollar amount above 0; using the default", k.key, v)
			continue
		}
		*k.dst = x
	}
	// The two shares of the model's window (6.2): a percent, as 60 or "60%". reseed_quiet_at
	// may not pass reseed_at.
	share := func(key string, dst *float64, lo, hi float64) {
		v, ok := sec[key]
		if !ok {
			return
		}
		x := math.NaN()
		switch n := v.(type) {
		case int64:
			x = float64(n)
		case float64:
			x = n
		case string:
			if t, cut := strings.CutSuffix(strings.TrimSpace(n), "%"); cut {
				if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
					x = f
				}
			}
		}
		if !(x >= lo && x <= hi) {
			bad("%s %v: want a percent from %g to %g, such as %g or \"%g%%\"; using %g%%", key, v, lo, hi, *dst*100, *dst*100, *dst*100)
			return
		}
		*dst = x / 100
	}
	share("reseed_at", &l.ReseedAt, 20, 90)
	share("reseed_quiet_at", &l.ReseedQuietAt, 0, l.ReseedAt*100)
	l.ReseedQuietAt = min(l.ReseedQuietAt, l.ReseedAt) // the default 25% under a reseed_at of 20%
	share("reseed_early_at", &l.ReseedEarlyAt, 10, l.ReseedAt*100)
	l.ReseedEarlyAt = min(l.ReseedEarlyAt, l.ReseedAt)
	known := map[string]bool{"engine": true, "model": true, "small_model": true, "auth": true, "oauth_token_command": true,
		"api_key_command": true, "oauth_token": true, "api_key": true, "max_turns": true, "max_turn_usd": true, "daily_usd": true,
		"restart_delay": true, "crash_limit": true, "crash_window": true, "route_backoff_max": true, "typing_timeout": true,
		"command_timeout": true, "token_retry": true, "wake_retry": true,
		"summary_max_chars": true, "summary_input_chars": true, "summary_quiet": true, "reseed_at": true, "reseed_quiet": true,
		"reseed_quiet_at": true, "reseed_early_at": true, "seed_messages": true, "seed_messages_chars": true, "seed_logbook_bytes": true, "precompact_timeout": true}
	for _, k := range laterKeys {
		known[k] = true
	}
	var unknown []string
	for k := range sec {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		bad("unknown key %q; ignored", k)
	}
	return c, problems, nil
}

// Once logs each distinct line once: a config read again at every engine start does not
// repeat its problems (capcom-service.md 3.5, 19).
type Once struct {
	mu   sync.Mutex
	seen map[string]bool
	logf func(string, ...any)
}

// NewOnce returns a logger that passes each distinct line to logf once.
func NewOnce(logf func(string, ...any)) *Once { return &Once{seen: map[string]bool{}, logf: logf} }

// Log logs line unless it was logged before.
func (o *Once) Log(line string) {
	o.mu.Lock()
	first := !o.seen[line]
	if len(o.seen) > 1000 {
		clear(o.seen)
	}
	o.seen[line] = true
	o.mu.Unlock()
	if first && o.logf != nil {
		o.logf("%s", strings.TrimSpace(line))
	}
}
