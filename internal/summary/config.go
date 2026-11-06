// Package summary asks an OpenAI-compatible endpoint for a short line that says what an agent
// asks to do, for approval banners (docs/specs/app.md 16.5). It is off unless the hab's config
// file has a [summaries] section with a base_url and a model.
package summary

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults and bounds (app.md 16.5).
const (
	// DefaultWait is how long a banner waits for its summary, from when the request arrived.
	DefaultWait = 1500 * time.Millisecond
	minWait     = 200 * time.Millisecond
	maxWait     = 5 * time.Second
	// CallTimeout bounds one HTTP call; a summary later than the banner's wait can still
	// replace the banner or name the answered line.
	CallTimeout = 10 * time.Second
)

// Thinking settings, as the config file names them.
const (
	ThinkingOff     = "off"     // reasoning_effort "none", the default
	ThinkingDefault = "default" // no reasoning_effort: the endpoint's own default
)

var thinkingLevels = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true}

// Settings is the [summaries] section of the hab's config file.
type Settings struct {
	BaseURL    string
	Model      string
	KeyCommand []string // nil: no Authorization header
	Thinking   string   // ThinkingOff, ThinkingDefault or a reasoning level
	Wait       time.Duration
}

// On reports whether summaries are set up.
func (s Settings) On() bool { return s.BaseURL != "" && s.Model != "" }

type fileConfig struct {
	Summaries *struct {
		BaseURL    string   `toml:"base_url"`
		Model      string   `toml:"model"`
		KeyCommand []string `toml:"key_command"`
		Thinking   string   `toml:"thinking"`
		Timeout    string   `toml:"timeout"`
	} `toml:"summaries"`
}

// LoadSettings reads the [summaries] section of the hab's config file. A missing file or
// section gives Settings with On false. A section with neither base_url nor model is off too;
// one with only one of them, or a bad value, is an error.
func LoadSettings(path string) (Settings, error) {
	var s Settings
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var fc fileConfig
	if _, err := toml.Decode(string(b), &fc); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	c := fc.Summaries
	if c == nil {
		return s, nil
	}
	bad := func(format string, a ...any) (Settings, error) {
		return Settings{}, fmt.Errorf("%s: summaries: "+format, append([]any{path}, a...)...)
	}
	c.BaseURL, c.Model = strings.TrimSpace(c.BaseURL), strings.TrimSpace(c.Model)
	switch {
	case c.BaseURL == "" && c.Model == "":
		return s, nil
	case c.BaseURL == "":
		return bad("model is set but base_url is not")
	case c.Model == "":
		return bad("base_url is set but model is not")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return bad("base_url %q is not an http or https URL", c.BaseURL)
	}
	if u.User != nil {
		return bad("base_url must not hold a user or password; use key_command")
	}
	if c.KeyCommand != nil && (len(c.KeyCommand) == 0 || c.KeyCommand[0] == "") {
		return bad("key_command names no program")
	}
	th := strings.ToLower(strings.TrimSpace(c.Thinking))
	switch {
	case th == "":
		th = ThinkingOff
	case th == ThinkingOff || th == ThinkingDefault || thinkingLevels[th]:
	default:
		return bad("thinking %q is not off, minimal, low, medium, high or default", c.Thinking)
	}
	wait := DefaultWait
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d < minWait || d > maxWait {
			return bad("timeout %q is not a duration between %v and %v", c.Timeout, minWait, maxWait)
		}
		wait = d
	}
	return Settings{BaseURL: c.BaseURL, Model: c.Model, KeyCommand: c.KeyCommand, Thinking: th, Wait: wait}, nil
}

// PlainWarning is the warning to log for a base_url that sends text in the clear across a
// network, or "" when the URL is https or names a loopback or private address.
func (s Settings) PlainWarning() string {
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Scheme != "http" {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)) {
		return ""
	}
	return fmt.Sprintf("summaries: base_url is http to %s: request text crosses the network in the clear", host)
}

// cgnat is the shared address range tailnets use.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
