package push

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"
)

// tokenLife is how long one provider token is reused. APNs refuses a token older than an hour
// and one refreshed more often than every 20 minutes.
const tokenLife = 40 * time.Minute

// Signer makes the APNs provider token: an ES256 JWT with the key id in its header and the team
// id and issue time in its claims. It reuses one token for tokenLife. Safe for concurrent use.
type Signer struct {
	key    *ecdsa.PrivateKey // the key of the cached token, or the fixed key
	keys   *Keyring          // when set, the key comes from here at each Token
	keyID  string
	teamID string
	now    func() time.Time

	mu  sync.Mutex
	tok string
	iat time.Time
}

// NewSigner returns a signer. now may be nil.
func NewSigner(key *ecdsa.PrivateKey, keyID, teamID string, now func() time.Time) *Signer {
	if now == nil {
		now = time.Now
	}
	return &Signer{key: key, keyID: keyID, teamID: teamID, now: now}
}

// Token returns the current provider token, signing a new one when the last is tokenLife old.
func (s *Signer) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys != nil {
		// Read under mu, so a send that read the key earlier can never put an older one back.
		if k := s.keys.Key(); k != s.key {
			s.key, s.tok = k, ""
		}
	}
	now := s.now()
	if s.tok != "" && now.Sub(s.iat) < tokenLife && !now.Before(s.iat) {
		return s.tok, nil
	}
	tok, err := s.sign(now)
	if err != nil {
		return "", err
	}
	s.tok, s.iat = tok, now
	return tok, nil
}

// NewKeyringSigner returns a signer that takes its key from keys at each Token: a new key there
// drops the cached token. With no key yet, Token returns ErrNoKey. now may be nil.
func NewKeyringSigner(keys *Keyring, keyID, teamID string, now func() time.Time) *Signer {
	s := NewSigner(nil, keyID, teamID, now)
	s.keys = keys
	return s
}

// Expire drops the cached token, after APNs called it expired.
func (s *Signer) Expire() {
	s.mu.Lock()
	s.tok = ""
	s.mu.Unlock()
}

func (s *Signer) sign(now time.Time) (string, error) {
	if s.key == nil {
		return "", ErrNoKey
	}
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.keyID})
	c, _ := json.Marshal(map[string]any{"iss": s.teamID, "iat": now.Unix()})
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return "", err
	}
	// JWS ES256: r and s, each left-padded to 32 bytes.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return unsigned + "." + enc.EncodeToString(sig), nil
}
