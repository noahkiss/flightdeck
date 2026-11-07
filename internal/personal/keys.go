package personal

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/internal/push"
)

// Secret runs a key command and keeps what it printed in memory only (8.3, 8.4, E13): never in
// a file, a log or an error. One run at a time; after a failure the next run waits retry.
// Safe for concurrent use.
type Secret struct {
	what  string // the config key, for errors: "[capcom.weather] key_command"
	argv  []string
	parse func([]byte) (any, error)
	retry time.Duration
	now   func() time.Time
	run   func(ctx context.Context, argv []string, use func([]byte) error) error // nil: push.RunSecret

	mu      sync.Mutex
	val     any
	next    time.Time
	lastErr error
}

// NewKeySecret is a Secret whose command prints a .p8 key (an Apple Developer key).
func NewKeySecret(what string, argv []string, now func() time.Time) *Secret {
	return &Secret{what: what, argv: argv, now: now, retry: 30 * time.Second, parse: func(b []byte) (any, error) {
		k, err := push.ParseKey(b)
		if err != nil {
			return nil, fmt.Errorf("printed no .p8 key: %v", err)
		}
		return k, nil
	}}
}

// NewTokenSecret is a Secret whose command prints one line: an API key.
func NewTokenSecret(what string, argv []string, now func() time.Time) *Secret {
	return &Secret{what: what, argv: argv, now: now, retry: 30 * time.Second, parse: func(b []byte) (any, error) {
		s := strings.TrimSpace(string(b))
		if s == "" || strings.ContainsAny(s, " \t\r\n") {
			return nil, errors.New("printed no key: want one line with no spaces")
		}
		return s, nil
	}}
}

// get returns the value, running the command when none is held and the retry allows.
func (s *Secret) get(ctx context.Context) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.val != nil {
		return s.val, nil
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if now().Before(s.next) && s.lastErr != nil {
		return nil, s.lastErr
	}
	run := s.run
	if run == nil {
		run = func(ctx context.Context, argv []string, use func([]byte) error) error {
			return push.RunSecret(ctx, s.what, argv, 0, 0, use)
		}
	}
	var v any
	err := run(ctx, s.argv, func(b []byte) error {
		var err error
		v, err = s.parse(b)
		return err
	})
	if err != nil {
		s.lastErr, s.next = err, now().Add(s.retry)
		return nil, err
	}
	s.val, s.lastErr = v, nil
	return v, nil
}

// Drop forgets the held value, so the next call runs the command again (a key the service
// refused).
func (s *Secret) Drop() {
	s.mu.Lock()
	s.val = nil
	s.mu.Unlock()
}

func (s *Secret) key(ctx context.Context) (*ecdsa.PrivateKey, error) {
	v, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	return v.(*ecdsa.PrivateKey), nil
}

func (s *Secret) token(ctx context.Context) (string, error) {
	v, err := s.get(ctx)
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// signJWT makes an ES256 JWT with the header and claims given.
func signJWT(key *ecdsa.PrivateKey, header, claims map[string]any) (string, error) {
	enc := base64.RawURLEncoding
	header["alg"] = "ES256"
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	r, ss, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return unsigned + "." + enc.EncodeToString(sig), nil
}
