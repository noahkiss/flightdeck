package push

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSignerJWT: the provider token is ES256 with the key id and team id, signed by the key,
// reused for 40 minutes, then signed again; Expire forces a new one.
func TestSignerJWT(t *testing.T) {
	k, _ := testKey(t)
	c := newClock()
	s := NewSigner(k, testKeyID, testTeamID, c.Now)
	tok, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	kid, iss, iat, ok := checkJWT(&k.PublicKey, tok)
	if !ok || kid != testKeyID || iss != testTeamID || iat != c.Now().Unix() {
		t.Fatalf("token: kid %q iss %q iat %d valid %v", kid, iss, iat, ok)
	}
	c.Add(39 * time.Minute)
	if again, _ := s.Token(); again != tok {
		t.Fatal("a token under 40 minutes old was not reused")
	}
	c.Add(2 * time.Minute)
	fresh, _ := s.Token()
	if fresh == tok {
		t.Fatal("a token 41 minutes old was reused")
	}
	if _, _, iat, _ := checkJWT(&k.PublicKey, fresh); iat != c.Now().Unix() {
		t.Fatalf("fresh token iat %d", iat)
	}
	s.Expire()
	if after, _ := s.Token(); after == fresh {
		t.Fatal("Expire kept the token")
	}
}

// TestClientSend: HTTP/2, the headers APNs needs, the topic per push type, and the gateway
// per environment.
func TestClientSend(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tok := strings.Repeat("ab", 32)
	if err := r.client.Send(ctx, Message{Token: tok, Environment: Production, Type: TypeAlert, CollapseID: "r-1", Payload: []byte(`{"aps":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := r.client.Send(ctx, Message{Token: tok, Environment: Development, Type: TypeLiveActivity, Priority: 5, Payload: []byte(`{"aps":{}}`)}); err != nil {
		t.Fatal(err)
	}
	p, d := r.prod.requests(), r.dev.requests()
	if len(p) != 1 || len(d) != 1 {
		t.Fatalf("production got %d, development %d", len(p), len(d))
	}
	a := p[0]
	if a.Proto != 2 {
		t.Errorf("HTTP/%d, want 2", a.Proto)
	}
	if !a.JWTGood || a.Token != tok {
		t.Errorf("jwt valid %v, token %q", a.JWTGood, a.Token)
	}
	for h, want := range map[string]string{"apns-topic": testTopic, "apns-push-type": "alert", "apns-priority": "10", "apns-collapse-id": "r-1", "apns-expiration": "0"} {
		if got := a.Header.Get(h); got != want {
			t.Errorf("alert %s = %q, want %q", h, got, want)
		}
	}
	la := d[0]
	for h, want := range map[string]string{"apns-topic": testTopic + ".push-type.liveactivity", "apns-push-type": "liveactivity", "apns-priority": "5"} {
		if got := la.Header.Get(h); got != want {
			t.Errorf("live activity %s = %q, want %q", h, got, want)
		}
	}
	if la.Header.Get("apns-collapse-id") != "" {
		t.Error("a message with no collapse id sent one")
	}
}

// TestClientErrors: APNs's refusals map to bad token, refused key and rate limit; an expired
// provider token is signed again and the message sent once more.
func TestClientErrors(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gone, bad, limited, exp := strings.Repeat("01", 32), strings.Repeat("02", 32), strings.Repeat("03", 32), strings.Repeat("04", 32)
	r.prod.reply(gone, 410, "Unregistered")
	r.prod.reply(bad, 400, "BadDeviceToken")
	r.prod.reply(limited, 429, "TooManyRequests")
	r.prod.replyOnce(exp, 403, "ExpiredProviderToken")
	send := func(tok string) *Error {
		err := r.client.Send(ctx, Message{Token: tok, Environment: Production, Payload: []byte(`{}`)})
		var e *Error
		if err != nil && !errors.As(err, &e) {
			t.Fatalf("%s: %v", tok, err)
		}
		return e
	}
	if e := send(gone); e == nil || !e.BadToken() {
		t.Errorf("410 Unregistered: %v", e)
	}
	if e := send(bad); e == nil || !e.BadToken() {
		t.Errorf("400 BadDeviceToken: %v", e)
	}
	if e := send(limited); e == nil || !e.RateLimited() || e.BadToken() {
		t.Errorf("429: %v", e)
	}
	before := len(r.prod.requests())
	if e := send(exp); e != nil {
		t.Errorf("expired provider token was not retried: %v", e)
	}
	if n := len(r.prod.requests()) - before; n != 2 {
		t.Errorf("expired provider token: %d requests, want 2", n)
	}
	// A key APNs does not know: every request is refused as InvalidProviderToken.
	other, _ := testKey(t)
	c := NewClient(ClientConfig{Signer: NewSigner(other, testKeyID, testTeamID, nil), Topic: testTopic, HTTP: r.httpClient,
		Hosts: map[string]string{Production: r.prod.srv.URL}})
	err := c.Send(ctx, Message{Token: gone, Environment: Production, Payload: []byte(`{}`)})
	var e *Error
	if !errors.As(err, &e) || !e.KeyRejected() {
		t.Errorf("unknown key: %v", err)
	}
	if err := r.client.Send(ctx, Message{Token: gone, Environment: "staging", Payload: []byte(`{}`)}); err == nil {
		t.Error("an unknown environment was sent")
	}
	if err := r.client.Send(ctx, Message{Token: gone, Environment: Production, Payload: make([]byte, MaxPayload+1)}); err == nil {
		t.Error("an oversized payload was sent")
	}
}

// TestLoadConfig: nothing set is "not set up"; the environment overrides the file, the key
// command as a JSON array; a partial config, a bad id, an empty program, a key command that is
// not a JSON array and the retired key_file (file or environment) are errors, the last naming
// key_command.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	none := func(string) string { return "" }
	if _, ok, err := LoadConfig(filepath.Join(dir, ConfigFile), none); ok || err != nil {
		t.Fatalf("no config: configured %v, %v", ok, err)
	}
	file := filepath.Join(dir, ConfigFile)
	os.WriteFile(file, []byte(`{"key_command":["pass","show","apns key"],"key_id":"`+testKeyID+`","team_id":"`+testTeamID+`","topic":"`+testTopic+`"}`), 0o600)
	c, ok, err := LoadConfig(file, none)
	if !ok || err != nil {
		t.Fatalf("file config: %v %v", ok, err)
	}
	if c.Environment != Production || !slices.Equal(c.KeyCommand, []string{"pass", "show", "apns key"}) {
		t.Errorf("file config: %+v", c)
	}
	env := map[string]string{Env.Environment: Development, Env.Topic: "dev.example.other", Env.KeyCommand: `["/bin/cat", "/dev/null"]`}
	c, _, err = LoadConfig(file, func(k string) string { return env[k] })
	if err != nil || c.Environment != Development || c.Topic != "dev.example.other" || c.KeyID != testKeyID || !slices.Equal(c.KeyCommand, []string{"/bin/cat", "/dev/null"}) {
		t.Fatalf("env over file: %+v %v", c, err)
	}
	env[Env.KeyCommand] = "pass show apns-key"
	if _, _, err := LoadConfig(file, func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "JSON array") {
		t.Errorf("a key command that is not JSON: %v", err)
	}
	if _, ok, err := LoadConfig("", func(k string) string { return map[string]string{Env.KeyID: testKeyID}[k] }); !ok || err == nil || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("partial config: configured %v, err %v", ok, err)
	}
	bad := map[string]string{Env.KeyCommand: `["x"]`, Env.KeyID: "short", Env.TeamID: testTeamID, Env.Topic: testTopic}
	if _, _, err := LoadConfig("", func(k string) string { return bad[k] }); err == nil {
		t.Error("a malformed key id passed")
	}
	bad = map[string]string{Env.KeyCommand: `[""]`, Env.KeyID: testKeyID, Env.TeamID: testTeamID, Env.Topic: testTopic}
	if _, _, err := LoadConfig("", func(k string) string { return bad[k] }); err == nil {
		t.Error("an empty program passed")
	}
	bad = map[string]string{Env.KeyFile: "/x/AuthKey.p8", Env.KeyCommand: `["x"]`, Env.KeyID: testKeyID, Env.TeamID: testTeamID, Env.Topic: testTopic}
	if _, ok, err := LoadConfig("", func(k string) string { return bad[k] }); !ok || err == nil || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("key file in the environment: %v %v", ok, err)
	}
	os.WriteFile(file, []byte(`{"key_file":"/x/AuthKey.p8","key_id":"`+testKeyID+`","team_id":"`+testTeamID+`","topic":"`+testTopic+`"}`), 0o600)
	if _, ok, err := LoadConfig(file, none); !ok || err == nil || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("key_file in the file: %v %v", ok, err)
	}
	os.WriteFile(file, []byte(`{"key_command":["x"],"unknown":1}`), 0o600)
	if _, _, err := LoadConfig(file, none); err == nil {
		t.Error("an unknown field passed")
	}
}

// TestStore: tokens persist owner-only, a device tracks one activity, unregister and drop
// remove tokens, and an empty device goes.
func TestStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "push")
	s, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b, la1, la2 := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32), strings.Repeat("dd", 32)
	s.Register("dev1", a, Production, []string{KindPermission}, false)
	s.RegisterActivity("dev1", "s1", la1)
	s.RegisterActivity("dev1", "s2", la2)
	s.Register("dev2", b, Development, []string{KindDone}, true)
	fi, err := os.Stat(filepath.Join(dir, TokensFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("tokens file: %v %v", fi, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("push dir mode %v", di.Mode().Perm())
	}
	s2, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	devs := s2.Devices()
	if len(devs) != 2 || devs[0].Activity == nil || devs[0].Activity.Session != "s2" || devs[0].Activity.Token != la2 || !devs[1].HideCommands {
		t.Fatalf("reopened: %+v", devs)
	}
	if ok, _ := s2.Unregister("dev1", la2); !ok {
		t.Error("unregister of the activity token")
	}
	if ok, _ := s2.Drop(b); !ok {
		t.Error("drop")
	}
	devs = s2.Devices()
	if len(devs) != 1 || devs[0].ID != "dev1" || devs[0].Activity != nil {
		t.Fatalf("after drop: %+v", devs)
	}
	if !ValidToken(a) || ValidToken("xyz") || ValidToken("../../etc") {
		t.Error("ValidToken")
	}
}
