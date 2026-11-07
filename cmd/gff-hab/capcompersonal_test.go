package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/personal"
)

// fakeApple stands in for WeatherKit and the Apple Maps Server API (never the real services).
func fakeApple(t *testing.T, weatherHits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/weather/"):
			weatherHits.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"currentWeather":{"asOf":"2026-11-07T12:00:00Z","temperature":11,"temperatureApparent":9,"conditionCode":"Drizzle","humidity":0.8,"windSpeed":12,"uvIndex":1}}`))
		case r.URL.Path == "/v1/token":
			w.Write([]byte(`{"accessToken":"acc","expiresInSeconds":1800}`))
		case r.URL.Path == "/v1/search":
			w.Write([]byte(`{"results":[{"id":"I1","name":"Green Bowl","coordinate":{"latitude":52.5201,"longitude":13.4051},"formattedAddressLines":["Oranienstr. 1"],"poiCategory":"Restaurant"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeTestKey(t *testing.T, dir string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p := filepath.Join(dir, "key.p8")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// replyTo sends text to Capcom and returns the stub's reply to it.
func replyTo(t *testing.T, e *serviceEnv, c interface {
	Call(context.Context, string, any, any) error
}, text string) api.CapcomItem {
	t.Helper()
	run(t, "capcom", "say", "-socket", e.sock, text)
	var got api.CapcomItem
	waitUntil(t, "the reply to "+text, func() bool {
		var p api.CapcomThreadPage
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if c.Call(ctx, "capcom.thread.page", api.CapcomThreadPageParams{}, &p) != nil {
			return false
		}
		for i, it := range p.Items {
			if it.Author.Kind == "user" && it.Text == text {
				// Newest first: the reply is above the message.
				for j := i - 1; j >= 0; j-- {
					if p.Items[j].Kind == "message" && p.Items[j].Author.Kind == "capcom" {
						got = p.Items[j]
						return true
					}
				}
			}
		}
		return false
	})
	return got
}

// The personal tools end to end (capcom-service.md 8, 18 A5) with the stub engine calling the
// route as gff-hab mcp does, and fake Apple services.
func TestCapcomPersonalTools(t *testing.T) {
	var hits atomic.Int32
	srv := fakeApple(t, &hits)
	keyDir := shortDir(t)
	key := writeTestKey(t, keyDir)
	e := newServiceEnv(t, `location_stale = "30m"
[capcom.maps]
key_command = ["/bin/cat", "`+key+`"]
key_id = "MK1"
team_id = "TEAM42"
[capcom.weather]
service_id = "com.example.weather"
cache = "0s"
`)
	e.env = append(e.env, "GFF_TEST_WEATHERKIT="+srv.URL, "GFF_TEST_APPLE_MAPS="+srv.URL)
	e.serve()
	c := e.control()
	if w := welcome(t, e.sock); !strings.Contains(strings.Join(w.Capabilities, " "), "device-context") {
		t.Fatalf("capabilities %v", w.Capabilities)
	}
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	waitUntil(t, "the engine connected", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})

	// Nothing shared yet.
	if r := replyTo(t, e, c, `personal device_location personal.location {}`); !strings.Contains(r.Text, personal.ReasonNothingYet) {
		t.Fatalf("before any put: %q", r.Text)
	}
	fix := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := callJSON[api.Empty](t, c, "device.context.put", api.DeviceContextPutParams{TZ: "Europe/Berlin",
		Location: &api.DeviceLocation{Lat: 52.52001, Lon: 13.40495, Accuracy: 25, Time: fix, Place: "Mitte, Berlin", Country: "DE"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := callJSON[api.Empty](t, c, "device.context.put", api.DeviceContextPutParams{TZ: "Mars/Olympus"}); errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("a bad zone: %v", err)
	}

	// A question with no place in it: no location reaches the engine, and no tool line.
	if r := replyTo(t, e, c, "what is two plus two"); r.Text != "echo: what is two plus two" {
		t.Fatalf("plain reply %q", r.Text)
	}
	if strings.Contains(e.stubLog(), "52.52") || strings.Contains(e.stubLog(), "Mitte") {
		t.Fatalf("the location reached the engine unasked:\n%s", e.stubLog())
	}

	// One read: the place, coordinates and age for the model; the tool line names the place.
	r := replyTo(t, e, c, `personal device_location personal.location {"n":1}`)
	if !strings.Contains(r.Text, "Mitte, Berlin") || !strings.Contains(r.Text, "52.52001") || !strings.Contains(r.Text, "from test") {
		t.Fatalf("location reply %q", r.Text)
	}
	lines := toolLines(t, c)
	if len(lines) == 0 || lines[len(lines)-1] != "mcp__flightdeck__device_location device_location: Mitte, Berlin" {
		t.Fatalf("tool lines %q", lines)
	}

	// Weather: cache 0 makes every call reach the fake endpoint; the reply carries the mark; the
	// tool line loses the coordinates the stub put in its summary.
	for range 2 {
		r = replyTo(t, e, c, `personal weather personal.weather {"lat":52.52001,"lon":13.40495,"kinds":"current"}`)
	}
	if hits.Load() != 2 {
		t.Fatalf("WeatherKit hits %d, want 2 with cache 0", hits.Load())
	}
	if !strings.Contains(r.Text, "Drizzle") || len(r.Attribution) != 1 || r.Attribution[0].Text != personal.WeatherMark || r.Attribution[0].URL != personal.WeatherLink {
		t.Fatalf("weather reply %q attribution %+v", r.Text, r.Attribution)
	}
	for _, l := range toolLines(t, c) {
		if strings.Contains(l, "52.52") || strings.Contains(l, "13.40") {
			t.Fatalf("a tool line shows coordinates: %q", l)
		}
	}
	if plain := replyTo(t, e, c, "thanks"); len(plain.Attribution) != 0 {
		t.Fatalf("a turn with no weather carries the mark: %+v", plain.Attribution)
	}

	// No Google key: source google is refused and Apple answers.
	r = replyTo(t, e, c, `personal places_search personal.places {"query":"vegetarian","source":"google","lat":52.52,"lon":13.405}`)
	if !strings.Contains(r.Text, "No Google key") || !strings.Contains(r.Text, "Green Bowl") || !strings.Contains(r.Text, "apple:I1") {
		t.Fatalf("places reply %q", r.Text)
	}

	// A calendar card: nothing is written without a tap; the first answer wins; the model gets
	// the outcome as a note.
	start := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	replyTo(t, e, c, `personal calendar_add personal.calendar_add {"title":"Dinner at Green Bowl","start":"`+start+`","location":"Oranienstr. 1"}`)
	card := lastCard(t, c, "calendar")
	if card.Answer != nil || card.Card.Event == nil || card.Card.Event.Title != "Dinner at Green Bowl" || card.Card.Event.TZ != "Europe/Berlin" ||
		strings.Join(card.Card.Choices, ",") != "add,dismiss" {
		t.Fatalf("card %+v", card)
	}
	res, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: card.ID, Choice: "add"})
	if err != nil || res.Item.Answer == nil || res.Item.Answer.By != "test" {
		t.Fatalf("add: %+v %v", res, err)
	}
	if _, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: card.ID, Choice: "dismiss"}); errKind(err) != api.ErrorKindConflict {
		t.Fatalf("a second answer: %v", err)
	}
	waitUntil(t, "the added note reaches the engine", func() bool { return strings.Contains(e.stubLog(), "The user added") })

	// A card whose event has ended: Add is refused.
	past := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	replyTo(t, e, c, `personal calendar_add personal.calendar_add {"title":"Gone","start":"`+past+`"}`)
	if _, err := callJSON[api.CapcomCardAnswered](t, c, "capcom.card.answer", api.CapcomCardAnswerParams{Item: lastCard(t, c, "calendar").ID, Choice: "add"}); errKind(err) != api.ErrorKindInvalid {
		t.Fatalf("add on a past card: %v", err)
	}

	// A pane's route is refused (E12).
	addr := startSender(t, e.habEnv)
	var out map[string]any
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hook.Call(ctx, addr, "personal.location", map[string]any{}, &out); err == nil || !strings.Contains(err.Error(), "only the Capcom service") {
		t.Fatalf("a pane's personal.location: %v %v", err, out)
	}

	// After a gff-hab restart the location is gone and only the time zone was saved.
	e.restart(false)
	c = e.control()
	waitUntil(t, "the engine connected again", func() bool {
		st, _ := callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	if r := replyTo(t, e, c, `personal device_location personal.location {"again":1}`); !strings.Contains(r.Text, personal.ReasonNothingYet) {
		t.Fatalf("after restart: %q", r.Text)
	}
	b, err := os.ReadFile(filepath.Join(e.state, "capcom", personal.DeviceFile))
	if err != nil || !strings.Contains(string(b), "Europe/Berlin") {
		t.Fatalf("device.json %q %v", b, err)
	}
	// No file but the thread (which holds the stub's echo of tool results) names the position.
	thread := filepath.Join(e.state, "capcom", "thread")
	filepath.WalkDir(e.state, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, thread) {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && (strings.Contains(string(b), "52.52001") || strings.Contains(string(b), "Mitte, Berlin")) {
			t.Errorf("%s holds the location", p)
		}
		return nil
	})
}

// toolLines lists the thread's tool calls, oldest first, as "name summary".
func toolLines(t *testing.T, c interface {
	Call(context.Context, string, any, any) error
}) []string {
	t.Helper()
	var p api.CapcomThreadPage
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Call(ctx, "capcom.thread.page", api.CapcomThreadPageParams{}, &p); err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(p.Items) - 1; i >= 0; i-- {
		for _, tc := range p.Items[i].Tools {
			out = append(out, tc.Name+" "+tc.Summary)
		}
	}
	return out
}

func lastCard(t *testing.T, c interface {
	Call(context.Context, string, any, any) error
}, typ string) api.CapcomItem {
	t.Helper()
	var p api.CapcomThreadPage
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Call(ctx, "capcom.thread.page", api.CapcomThreadPageParams{}, &p); err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		if it.Kind == "card" && it.Card != nil && it.Card.Type == typ {
			return it
		}
	}
	b, _ := json.Marshal(p.Items)
	t.Fatalf("no %s card in %s", typ, b)
	return api.CapcomItem{}
}
