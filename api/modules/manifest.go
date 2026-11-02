package modules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// ManifestFile is the manifest's file name in a module folder (modules.md 5).
const ManifestFile = "manifest.toml"

// BridgeVersion is the newest bridge version this hab provides (modules.md 7, 9).
const BridgeVersion = 1

// NewerBridge is the error ParseManifest returns for a manifest whose bridge is above
// BridgeVersion. It comes before every other check: a newer manifest may hold fields and words
// this hab does not know, and the person needs "needs a newer hab", not a list of them.
type NewerBridge struct {
	Bridge int64 // the version the manifest names
}

func (e *NewerBridge) Error() string {
	return fmt.Sprintf("the module needs bridge %d; this hab has %d: it needs a newer hab", e.Bridge, BridgeVersion)
}

// Manifest is a module's manifest.toml (modules.md 5). A new manifest field comes with a new
// bridge version, so an older hab refuses a manifest it cannot read in full.
type Manifest struct {
	ID       string    `toml:"id"`
	Name     string    `toml:"name"`
	Summary  string    `toml:"summary"`
	Version  string    `toml:"version"`
	Blocks   int64     `toml:"blocks"`
	Bridge   int64     `toml:"bridge"`
	Powers   []string  `toml:"powers"`
	Places   []string  `toml:"places"`
	Refresh  int64     `toml:"refresh"`
	Events   []string  `toml:"events"`
	Settings []Setting `toml:"settings"`
}

// Setting is one field of the module's settings form.
type Setting struct {
	Key     string   `toml:"key"`
	Title   string   `toml:"title"`
	Kind    string   `toml:"kind"`
	Default any      `toml:"default"`
	Choices []string `toml:"choices"`
}

// Manifest words and limits. internal/apigen writes them into manifest.schema.json.
var (
	Powers       = []string{"act", "exec", "fetch"}
	Places       = []string{"board", "host", "session", "sheet"}
	SettingKinds = []string{"text", "number", "toggle", "choice"}
)

const (
	ModuleIDPattern   = `^[a-z0-9]+(-[a-z0-9]+)*$`
	ModuleIDMax       = 64
	NameMax           = 60
	SummaryMax        = 200
	SemverPattern     = `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`
	EventPattern      = `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`
	SettingKeyPattern = `^[a-z][a-z0-9_]*$`
	SettingKeyMax     = 40
	// MinRefresh is the shortest refresh interval; a smaller value is raised to it.
	MinRefresh = 5
)

var (
	moduleIDRE   = regexp.MustCompile(ModuleIDPattern)
	semverRE     = regexp.MustCompile(SemverPattern)
	eventRE      = regexp.MustCompile(EventPattern)
	settingKeyRE = regexp.MustCompile(SettingKeyPattern)
)

var manifestKeys = []string{"id", "name", "summary", "version", "blocks", "bridge", "powers", "places", "refresh", "events", "settings"}
var settingKeys = []string{"key", "title", "kind", "default", "choices"}

// ParseManifest reads and checks a manifest.toml. It reads bridge first: above BridgeVersion it
// returns *NewerBridge and checks nothing else. Otherwise it returns Problems for a manifest that
// breaks manifest.schema.json or a rule the schema cannot state (unique setting keys, a choice
// default among its choices). Refresh below MinRefresh is raised, not refused.
func ParseManifest(data []byte) (*Manifest, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, Problems{{"", "not TOML: " + err.Error()}}
	}
	if b, ok := raw["bridge"].(int64); ok && b > BridgeVersion {
		return nil, &NewerBridge{Bridge: b}
	}
	var ps Problems
	fail := func(path, format string, args ...any) {
		ps = append(ps, Problem{path, fmt.Sprintf(format, args...)})
	}
	for _, k := range sortedKeys(raw) {
		if !contains(manifestKeys, k) {
			fail(k, "a manifest has no field %q", k)
		}
	}
	var m Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		// A field of the wrong TOML type.
		return nil, append(ps, Problem{"", err.Error()})
	}
	for _, k := range []string{"id", "name", "version", "blocks", "bridge", "places"} {
		if _, ok := raw[k]; !ok {
			fail("", "a manifest needs %s", k)
		}
	}
	text := func(path, s string, min, max int) {
		if n := utf8.RuneCountInString(s); n < min || n > max {
			fail(path, "%d characters; %d to %d allowed", n, min, max)
		}
	}
	if _, ok := raw["id"]; ok {
		if !moduleIDRE.MatchString(m.ID) || len(m.ID) > ModuleIDMax {
			fail("id", "an id is lowercase letters and digits joined by single hyphens, at most %d", ModuleIDMax)
		}
	}
	if _, ok := raw["name"]; ok {
		text("name", m.Name, 1, NameMax)
	}
	text("summary", m.Summary, 0, SummaryMax)
	if _, ok := raw["version"]; ok && !semverRE.MatchString(m.Version) {
		fail("version", "not a semver version")
	}
	if _, ok := raw["blocks"]; ok && m.Blocks < 1 {
		fail("blocks", "a vocabulary version is 1 or more")
	}
	if _, ok := raw["bridge"]; ok && m.Bridge < 1 {
		fail("bridge", "a bridge version is 1 or more")
	}
	words := func(path string, list, known []string, min int) {
		seen := map[string]bool{}
		for i, w := range list {
			if !contains(known, w) {
				fail(fmt.Sprintf("%s/%d", path, i), "not one of %s", strings.Join(known, ", "))
			}
			if seen[w] {
				fail(fmt.Sprintf("%s/%d", path, i), "%q appears twice", w)
			}
			seen[w] = true
		}
		if _, ok := raw[path]; ok && len(list) < min {
			fail(path, "needs at least %d", min)
		}
	}
	words("powers", m.Powers, Powers, 0)
	words("places", m.Places, Places, 1)
	if _, ok := raw["refresh"]; ok {
		if m.Refresh < 1 {
			fail("refresh", "refresh is a whole number of seconds, 1 or more")
		} else if m.Refresh < MinRefresh {
			m.Refresh = MinRefresh
		}
	}
	seenEvent := map[string]bool{}
	for i, e := range m.Events {
		if !eventRE.MatchString(e) {
			fail(fmt.Sprintf("events/%d", i), "not an event type, such as agent.state")
		}
		if seenEvent[e] {
			fail(fmt.Sprintf("events/%d", i), "%q appears twice", e)
		}
		seenEvent[e] = true
	}
	rawSettings, _ := raw["settings"].([]map[string]any)
	seenKey := map[string]bool{}
	for i, s := range m.Settings {
		p := fmt.Sprintf("settings/%d", i)
		var r map[string]any
		if i < len(rawSettings) {
			r = rawSettings[i]
		}
		for _, k := range sortedKeys(r) {
			if !contains(settingKeys, k) {
				fail(p+"/"+k, "a setting has no field %q", k)
			}
		}
		for _, k := range []string{"key", "title", "kind"} {
			if _, ok := r[k]; !ok {
				fail(p, "a setting needs %s", k)
			}
		}
		if _, ok := r["key"]; ok && (!settingKeyRE.MatchString(s.Key) || len(s.Key) > SettingKeyMax) {
			fail(p+"/key", "a key is lowercase letters, digits and underscores, at most %d", SettingKeyMax)
		}
		if seenKey[s.Key] {
			fail(p+"/key", "key %q appears twice", s.Key)
		}
		seenKey[s.Key] = true
		if _, ok := r["title"]; ok {
			text(p+"/title", s.Title, 1, NameMax)
		}
		if _, ok := r["kind"]; ok && !contains(SettingKinds, s.Kind) {
			fail(p+"/kind", "not one of %s", strings.Join(SettingKinds, ", "))
		}
		if s.Choices != nil && s.Kind != "choice" {
			fail(p+"/choices", "only a choice setting has choices")
		}
		if s.Kind == "choice" && len(s.Choices) == 0 {
			fail(p, "a choice setting needs choices")
		}
		if s.Default == nil {
			continue
		}
		ok := true
		switch s.Kind {
		case "text", "choice":
			_, ok = s.Default.(string)
		case "number":
			switch s.Default.(type) {
			case int64, float64:
			default:
				ok = false
			}
		case "toggle":
			_, ok = s.Default.(bool)
		}
		if !ok {
			fail(p+"/default", "the default does not fit kind %s", s.Kind)
		} else if d, isStr := s.Default.(string); isStr && s.Kind == "choice" && len(s.Choices) > 0 && !contains(s.Choices, d) {
			fail(p+"/default", "%q is not one of the choices", d)
		}
	}
	if len(ps) > 0 {
		return nil, ps
	}
	return &m, nil
}
