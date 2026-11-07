package capcom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hab.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServiceConfigDefaults(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "none.toml"), writeConfig(t, "[catalog]\n")} {
		c, problems, err := LoadServiceConfig(path)
		if err != nil || len(problems) != 0 {
			t.Fatalf("%q: %v %v", path, err, problems)
		}
		if c.Engine != EngineNone || c.Auth != AuthOAuthToken || c.Limits != DefaultLimits {
			t.Fatalf("%q: %+v", path, c)
		}
	}
}

func TestServiceConfigValues(t *testing.T) {
	p := writeConfig(t, `[capcom]
engine = "claude"
model = "m-main"
small_model = "m-small"
auth = "api_key"
oauth_token_command = ["print-token", "--plain"]
api_key_command = ["print-key"]
max_turns = 7
max_turn_usd = 1
daily_usd = 3.5
restart_delay = "2s"
crash_limit = 2
crash_window = "1m"
route_backoff_max = "4s"
typing_timeout = "2m"
command_timeout = "3s"
token_retry = "45s"
wake_retry = "1s"
summary_max_chars = 2000
`)
	c, problems, err := LoadServiceConfig(p)
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
	want := Limits{MaxTurns: 7, MaxTurnUSD: 1, DailyUSD: 3.5, RestartDelay: 2 * time.Second, CrashLimit: 2, CrashWindow: time.Minute,
		RouteBackoffMax: 4 * time.Second, TypingTimeout: 2 * time.Minute, CommandTimeout: 3 * time.Second, TokenRetry: 45 * time.Second, WakeRetry: time.Second}
	if c.Limits != want {
		t.Fatalf("limits %+v, want %+v", c.Limits, want)
	}
	if c.Engine != EngineClaude || c.Model != "m-main" || c.SmallModel != "m-small" || c.Auth != AuthAPIKey ||
		strings.Join(c.OAuthCommand, " ") != "print-token --plain" || strings.Join(c.APIKeyCommand, " ") != "print-key" {
		t.Fatalf("%+v", c)
	}
}

func TestServiceConfigBadValues(t *testing.T) {
	p := writeConfig(t, `[capcom]
engine = "gemini"
auth = "password"
oauth_token_command = "print-token"
max_turns = 0
max_turn_usd = 0
daily_usd = -1
restart_delay = "500ms"
crash_limit = 101
crash_window = "25h"
route_backoff_max = "soon"
typing_timeout = 5
command_timeout = "6m"
token_retry = "10s"
wake_retry = "6m"
oauth_token = "planted-value"
colour = "blue"
`)
	c, problems, err := LoadServiceConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine != EngineNone || c.Auth != AuthOAuthToken || c.OAuthCommand != nil || c.Limits != DefaultLimits || !c.CredentialInKey {
		t.Fatalf("%+v", c)
	}
	all := strings.Join(problems, "\n")
	for _, k := range []string{"engine", "auth", "oauth_token_command", "max_turns", "max_turn_usd", "daily_usd", "restart_delay", "crash_limit",
		"crash_window", "route_backoff_max", "typing_timeout", "command_timeout", "token_retry", "wake_retry", "unknown key \"colour\""} {
		if !strings.Contains(all, k) {
			t.Errorf("no problem names %s:\n%s", k, all)
		}
	}
	if !strings.Contains(all, "oauth_token: the config file holds no credential; set oauth_token_command") {
		t.Errorf("the credential key's line does not name the command key:\n%s", all)
	}
	if strings.Contains(all, "planted-value") {
		t.Fatal("a problem line holds the credential value")
	}
}

func TestOnce(t *testing.T) {
	var got []string
	o := NewOnce(func(f string, a ...any) { got = append(got, a[0].(string)) })
	o.Log("a")
	o.Log("b")
	o.Log("a")
	if strings.Join(got, ",") != "a,b" {
		t.Fatal(got)
	}
}

// The example file's [capcom] keys parse with no problem.
func TestServiceConfigExample(t *testing.T) {
	_, problems, err := LoadServiceConfig(filepath.Join("..", "..", "docs", "examples", "hab.toml"))
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
}
