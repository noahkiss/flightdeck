// Package push sends Flightdeck pushes from gff-hab straight to APNs with the user's own key
// (app.md 8). It holds the push config, the ES256 provider token, the HTTP/2 APNs client, the
// store of device and Live Activity tokens, the banner text fitted to the word budgets
// (app.md 16.2), and the notifier that turns agent state into pushes under the presence tiers.
// It uses the standard library only.
package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ConfigFile is the push config's file name in the hab's state directory.
const ConfigFile = "push.json"

// Environments name the two APNs gateways.
const (
	Development = "development"
	Production  = "production"
)

// Config is the push config: the command that prints the key, and what the key signs for. It
// holds the command, never the key itself.
type Config struct {
	KeyCommand  []string `json:"key_command"`           // argv of a command that prints the .p8 key; no shell
	KeyID       string   `json:"key_id"`                // the key's 10-character id
	TeamID      string   `json:"team_id"`               // the Apple Developer team id
	Topic       string   `json:"topic"`                 // the app's bundle id
	Environment string   `json:"environment,omitempty"` // a token's environment when it has none; default production
}

// Env names the variables that override the config file, field by field. KeyCommand's value is
// a JSON array of strings, the argv. KeyFile is no longer read: a value there is refused.
var Env = struct{ KeyCommand, KeyID, TeamID, Topic, Environment, KeyFile string }{
	"GFF_HAB_PUSH_KEY_COMMAND", "GFF_HAB_PUSH_KEY_ID", "GFF_HAB_PUSH_TEAM_ID", "GFF_HAB_PUSH_TOPIC", "GFF_HAB_PUSH_ENVIRONMENT",
	"GFF_HAB_PUSH_KEY_FILE",
}

// errKeyFile refuses the retired key_file source.
var errKeyFile = errors.New("push key_file is no longer read: set key_command to a command that prints the key (docs/push.md)")

var appleID = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// LoadConfig reads the config file at path, when it exists, then applies the environment over
// it. It reports configured false, with no error, when neither sets anything: push is not set
// up. A config that is set but incomplete or malformed is an error. It never runs the key
// command.
func LoadConfig(path string, getenv func(string) string) (c Config, configured bool, err error) {
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			var fc struct {
				Config
				KeyFile json.RawMessage `json:"key_file"`
			}
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&fc); err != nil {
				return Config{}, true, fmt.Errorf("%s: %w", path, err)
			}
			if fc.KeyFile != nil {
				return Config{}, true, fmt.Errorf("%s: %w", path, errKeyFile)
			}
			c = fc.Config
		case !errors.Is(err, os.ErrNotExist):
			return Config{}, false, err
		}
	}
	if getenv(Env.KeyFile) != "" {
		return Config{}, true, fmt.Errorf("$%s: %w", Env.KeyFile, errKeyFile)
	}
	if v := getenv(Env.KeyCommand); v != "" {
		var argv []string
		if err := json.Unmarshal([]byte(v), &argv); err != nil {
			return Config{}, true, fmt.Errorf("$%s is not a JSON array of strings, such as [\"pass\", \"show\", \"apns-key\"]", Env.KeyCommand)
		}
		c.KeyCommand = argv
	}
	for _, f := range []struct {
		dst *string
		env string
	}{{&c.KeyID, Env.KeyID}, {&c.TeamID, Env.TeamID}, {&c.Topic, Env.Topic}, {&c.Environment, Env.Environment}} {
		if v := getenv(f.env); v != "" {
			*f.dst = v
		}
	}
	if c.KeyCommand == nil && c.KeyID == "" && c.TeamID == "" && c.Topic == "" && c.Environment == "" {
		return c, false, nil
	}
	if c.Environment == "" {
		c.Environment = Production
	}
	return c, true, c.Check()
}

// Check validates the fields without running the key command.
func (c Config) Check() error {
	var missing []string
	if len(c.KeyCommand) == 0 {
		missing = append(missing, "key_command")
	}
	for _, f := range []struct{ v, name string }{{c.KeyID, "key_id"}, {c.TeamID, "team_id"}, {c.Topic, "topic"}} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("push config lacks %s", strings.Join(missing, ", "))
	}
	if c.KeyCommand[0] == "" {
		return errors.New("push key_command names no program")
	}
	if !appleID.MatchString(c.KeyID) {
		return fmt.Errorf("push key_id %q is not 10 capital letters and digits", c.KeyID)
	}
	if !appleID.MatchString(c.TeamID) {
		return fmt.Errorf("push team_id %q is not 10 capital letters and digits", c.TeamID)
	}
	if strings.ContainsAny(c.Topic, " /\t\n") || !strings.Contains(c.Topic, ".") {
		return fmt.Errorf("push topic %q is not a bundle id", c.Topic)
	}
	if c.Environment != Development && c.Environment != Production {
		return fmt.Errorf("push environment %q is not development or production", c.Environment)
	}
	return nil
}

// ParseKey parses an APNs auth key: a PEM "PRIVATE KEY" block holding a PKCS #8 P-256 key.
func ParseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, errors.New("not a P-256 key")
	}
	return ek, nil
}
