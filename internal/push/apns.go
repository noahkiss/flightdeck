package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The APNs gateways (Apple's provider API over HTTP/2).
const (
	HostProduction  = "https://api.push.apple.com"
	HostDevelopment = "https://api.sandbox.push.apple.com"
)

// Type is the apns-push-type of a message.
type Type string

const (
	TypeAlert        Type = "alert"
	TypeLiveActivity Type = "liveactivity"
)

// MaxPayload is APNs's limit for an alert or Live Activity payload.
const MaxPayload = 4096

// Message is one push to one token.
type Message struct {
	Token       string // hex device or activity token
	Environment string // development or production
	Type        Type
	Priority    int       // 10 now, 5 when power allows; 0 means 10
	CollapseID  string    // a later push with the same id replaces this one; at most 64 bytes
	Expiration  time.Time // zero: APNs tries once and does not store it
	Payload     []byte
}

// errSign wraps a failure to sign the provider token.
var errSign = errors.New("push provider token")

// Error is APNs's refusal of a message.
type Error struct {
	Status int
	Reason string // APNs's reason string, such as BadDeviceToken
}

func (e *Error) Error() string { return fmt.Sprintf("apns %d %s", e.Status, e.Reason) }

// BadToken reports that the token will never work again: drop it.
func (e *Error) BadToken() bool {
	switch e.Reason {
	case "BadDeviceToken", "Unregistered", "DeviceTokenNotForTopic", "ExpiredToken":
		return true
	}
	return e.Status == http.StatusGone
}

// KeyRejected reports that APNs refuses the key or the topic: push is broken until the config
// is fixed.
func (e *Error) KeyRejected() bool {
	switch e.Reason {
	case "InvalidProviderToken", "MissingProviderToken", "BadTopic", "TopicDisallowed", "BadCertificate", "BadCertificateEnvironment", "Forbidden":
		return true
	}
	return false
}

// RateLimited reports that APNs asks the hab to slow down.
func (e *Error) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// Client sends messages to APNs over HTTP/2 with token authentication.
type Client struct {
	signer *Signer
	keys   *Keyring // nil: the signer's key is fixed
	topic  string
	hosts  map[string]string // environment -> base URL
	http   *http.Client
}

// ClientConfig is what NewClient needs. Hosts and HTTP are for tests; nil means Apple's gateways
// and a fresh HTTP/2 client.
type ClientConfig struct {
	Signer *Signer
	// Keys, when set, gives the signer its key, and is asked to run the key command again when
	// APNs refuses the provider token or a token cannot be signed.
	Keys  *Keyring
	Topic string
	Hosts map[string]string
	HTTP  *http.Client
}

// NewClient returns a client.
func NewClient(cfg ClientConfig) *Client {
	hosts := cfg.Hosts
	if hosts == nil {
		hosts = map[string]string{Production: HostProduction, Development: HostDevelopment}
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				IdleConnTimeout:     5 * time.Minute,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		}
	}
	return &Client{signer: cfg.Signer, keys: cfg.Keys, topic: cfg.Topic, hosts: hosts, http: hc}
}

// Send posts one message. A refusal is an *Error. A provider token APNs calls expired is
// signed again and the message sent once more. With a keyring, a provider token APNs calls
// invalid, or one that cannot be signed, runs the key command again when its backoff allows;
// a new key sends the message once more. Before the first key it returns ErrNoKey.
func (c *Client) Send(ctx context.Context, m Message) error {
	if c.keys != nil {
		k := c.keys.Key()
		if k == nil {
			return ErrNoKey
		}
		c.signer.SetKey(k)
	}
	err := c.send(ctx, m)
	if e, ok := err.(*Error); ok && e.Reason == "ExpiredProviderToken" {
		c.signer.Expire()
		err = c.send(ctx, m)
	}
	if c.keys != nil && keyFailed(err) {
		if changed, _ := c.keys.Load(ctx); changed {
			c.signer.SetKey(c.keys.Key())
			err = c.send(ctx, m)
		}
	}
	return err
}

// keyFailed reports an error a new key may fix: APNs refusing the provider token, or a token
// that could not be signed.
func keyFailed(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*Error); ok {
		return e.Reason == "InvalidProviderToken"
	}
	return errors.Is(err, errSign)
}

func (c *Client) send(ctx context.Context, m Message) error {
	base, ok := c.hosts[m.Environment]
	if !ok {
		return fmt.Errorf("push environment %q is not development or production", m.Environment)
	}
	if len(m.Payload) > MaxPayload {
		return fmt.Errorf("push payload is %d bytes, over the %d limit", len(m.Payload), MaxPayload)
	}
	jwt, err := c.signer.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", errSign, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+m.Token, bytes.NewReader(m.Payload))
	if err != nil {
		return err
	}
	typ := m.Type
	if typ == "" {
		typ = TypeAlert
	}
	topic := c.topic
	if typ == TypeLiveActivity {
		topic += ".push-type.liveactivity"
	}
	prio := m.Priority
	if prio == 0 {
		prio = 10
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", topic)
	req.Header.Set("apns-push-type", string(typ))
	req.Header.Set("apns-priority", strconv.Itoa(prio))
	req.Header.Set("apns-expiration", "0")
	if !m.Expiration.IsZero() {
		req.Header.Set("apns-expiration", strconv.FormatInt(m.Expiration.Unix(), 10))
	}
	if m.CollapseID != "" {
		req.Header.Set("apns-collapse-id", m.CollapseID)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var r struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(body, &r)
	return &Error{Status: resp.StatusCode, Reason: r.Reason}
}
