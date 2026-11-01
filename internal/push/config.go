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

	"github.com/noahkiss/flightdeck/internal/secretfile"
)

// ConfigFile is the push config's file name in the hab's state directory.
const ConfigFile = "push.json"

// Environments name the two APNs gateways.
const (
	Development = "development"
	Production  = "production"
)

// Config is the push config: where the key file is and what it signs for. It holds a path to
// the key, never the key itself.
type Config struct {
	KeyFile     string `json:"key_file"`              // the .p8 auth key, owner-only
	KeyID       string `json:"key_id"`                // the key's 10-character id
	TeamID      string `json:"team_id"`               // the Apple Developer team id
	Topic       string `json:"topic"`                 // the app's bundle id
	Environment string `json:"environment,omitempty"` // a token's environment when it has none; default production
}

// Env names the variables that override the config file, field by field.
var Env = struct{ KeyFile, KeyID, TeamID, Topic, Environment string }{
	"GFF_HAB_PUSH_KEY_FILE", "GFF_HAB_PUSH_KEY_ID", "GFF_HAB_PUSH_TEAM_ID", "GFF_HAB_PUSH_TOPIC", "GFF_HAB_PUSH_ENVIRONMENT",
}

var appleID = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// LoadConfig reads the config file at path, when it exists, then applies the environment over
// it. It reports configured false, with no error, when neither sets anything: push is not set
// up. A config that is set but incomplete or malformed is an error.
func LoadConfig(path string, getenv func(string) string) (c Config, configured bool, err error) {
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				return Config{}, true, fmt.Errorf("%s: %w", path, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return Config{}, false, err
		}
	}
	for _, f := range []struct {
		dst *string
		env string
	}{{&c.KeyFile, Env.KeyFile}, {&c.KeyID, Env.KeyID}, {&c.TeamID, Env.TeamID}, {&c.Topic, Env.Topic}, {&c.Environment, Env.Environment}} {
		if v := getenv(f.env); v != "" {
			*f.dst = v
		}
	}
	if c == (Config{}) {
		return c, false, nil
	}
	if c.Environment == "" {
		c.Environment = Production
	}
	return c, true, c.Check()
}

// Check validates the fields without reading the key.
func (c Config) Check() error {
	var missing []string
	for _, f := range []struct{ v, name string }{{c.KeyFile, "key_file"}, {c.KeyID, "key_id"}, {c.TeamID, "team_id"}, {c.Topic, "topic"}} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("push config lacks %s", strings.Join(missing, ", "))
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

// maxKeyFile bounds the .p8 file; a real one is about 250 bytes.
const maxKeyFile = 16 << 10

// Key reads and parses the key file. The file must be owner-only (secretfile checks).
func (c Config) Key() (*ecdsa.PrivateKey, error) {
	b, err := secretfile.Read(c.KeyFile, maxKeyFile)
	if err != nil {
		return nil, fmt.Errorf("push key: %w", err)
	}
	k, err := ParseKey(b)
	if err != nil {
		return nil, fmt.Errorf("push key %s: %w", c.KeyFile, err)
	}
	return k, nil
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
