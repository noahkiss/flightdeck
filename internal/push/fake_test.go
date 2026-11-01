package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test values only: no real team, key or app.
const (
	testKeyID  = "KEY0000001"
	testTeamID = "TEAM000001"
	testTopic  = "dev.example.flightdeck"
)

// fakeAPNs is an HTTP/2 stand-in for the APNs provider API. It checks the provider token
// against the test key and records each request. Never Apple.
type fakeAPNs struct {
	t   *testing.T
	srv *httptest.Server
	pub *ecdsa.PublicKey

	mu     sync.Mutex
	reqs   []fakeReq
	status map[string]fakeReply // token -> reply; default 200
	once   map[string]fakeReply // token -> reply for the next request only
}

type fakeReply struct {
	status int
	reason string
}

type fakeReq struct {
	Env     string
	Proto   int
	Token   string
	Header  http.Header
	Body    map[string]any
	JWTIat  int64
	JWTKid  string
	JWTIss  string
	JWTGood bool
}

func newFakeAPNs(t *testing.T, pub *ecdsa.PublicKey, env string) *fakeAPNs {
	f := &fakeAPNs{t: t, pub: pub, status: map[string]fakeReply{}, once: map[string]fakeReply{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handle(env, w, r)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

func (f *fakeAPNs) handle(env string, w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.URL.Path, "/3/device/")
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	req := fakeReq{Env: env, Proto: r.ProtoMajor, Token: tok, Header: r.Header.Clone(), Body: body}
	req.JWTKid, req.JWTIss, req.JWTIat, req.JWTGood = checkJWT(f.pub, strings.TrimPrefix(r.Header.Get("authorization"), "bearer "))
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	reply, ok := f.once[tok]
	if ok {
		delete(f.once, tok)
	} else {
		reply, ok = f.status[tok]
	}
	f.mu.Unlock()
	if !req.JWTGood {
		reply, ok = fakeReply{403, "InvalidProviderToken"}, true
	}
	if !ok || reply.status == 200 {
		w.Header().Set("apns-id", "00000000-0000-0000-0000-000000000000")
		w.WriteHeader(200)
		return
	}
	w.WriteHeader(reply.status)
	fmt.Fprintf(w, `{"reason":%q}`, reply.reason)
}

func (f *fakeAPNs) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

func (f *fakeAPNs) reply(token string, status int, reason string) {
	f.mu.Lock()
	f.status[token] = fakeReply{status, reason}
	f.mu.Unlock()
}

func (f *fakeAPNs) replyOnce(token string, status int, reason string) {
	f.mu.Lock()
	f.once[token] = fakeReply{status, reason}
	f.mu.Unlock()
}

// checkJWT verifies an ES256 provider token and returns its fields.
func checkJWT(pub *ecdsa.PublicKey, tok string) (kid, iss string, iat int64, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return
	}
	enc := base64.RawURLEncoding
	hb, err1 := enc.DecodeString(parts[0])
	cb, err2 := enc.DecodeString(parts[1])
	sig, err3 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(sig) != 64 {
		return
	}
	var h struct{ Alg, Kid string }
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
	}
	if json.Unmarshal(hb, &h) != nil || json.Unmarshal(cb, &c) != nil || h.Alg != "ES256" {
		return
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return h.Kid, c.Iss, c.Iat, ecdsa.Verify(pub, sum[:], r, s)
}

func testKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testRig is a client against two fake gateways, one per environment.
type testRig struct {
	key        *ecdsa.PrivateKey
	prod, dev  *fakeAPNs
	client     *Client
	clock      *clock
	signer     *Signer
	httpClient *http.Client
}

func newRig(t *testing.T) *testRig {
	k, _ := testKey(t)
	r := &testRig{key: k, clock: newClock()}
	r.prod = newFakeAPNs(t, &k.PublicKey, Production)
	r.dev = newFakeAPNs(t, &k.PublicKey, Development)
	r.signer = NewSigner(k, testKeyID, testTeamID, r.clock.Now)
	// One client trusts both test servers' certificates.
	pool := x509.NewCertPool()
	pool.AddCert(r.prod.srv.Certificate())
	pool.AddCert(r.dev.srv.Certificate())
	hc := r.prod.srv.Client()
	tr := hc.Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.RootCAs = pool
	tr.ForceAttemptHTTP2 = true
	r.httpClient = &http.Client{Transport: tr, Timeout: 10 * time.Second}
	r.client = NewClient(ClientConfig{Signer: r.signer, Topic: testTopic, HTTP: r.httpClient,
		Hosts: map[string]string{Production: r.prod.srv.URL, Development: r.dev.srv.URL}})
	return r
}
