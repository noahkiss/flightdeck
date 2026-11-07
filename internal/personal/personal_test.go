package personal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// fakeRun stands in for the key command: it prints out and counts its runs.
func fakeRun(out []byte, runs *atomic.Int32) func(context.Context, []string, func([]byte) error) error {
	return func(_ context.Context, _ []string, use func([]byte) error) error {
		runs.Add(1)
		return use(append([]byte{}, out...))
	}
}

func jwtParts(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		t.Fatalf("token %q is no JWT", tok)
	}
	var h, c map[string]any
	for i, dst := range []*map[string]any{&h, &c} {
		b, err := base64.RawURLEncoding.DecodeString(p[i])
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(b, dst)
	}
	return h, c
}

func TestStoreNewestFixAndZoneOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	s, err := OpenStore(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, why := s.Location(); why != ReasonNothingYet {
		t.Fatalf("empty store: why %q", why)
	}
	put := func(dev string, p api.DeviceContextPutParams) {
		t.Helper()
		if err := s.Put(dev, p); err != nil {
			t.Fatal(err)
		}
	}
	put("phone", api.DeviceContextPutParams{TZ: "Europe/Berlin", Location: &api.DeviceLocation{Lat: 52.5, Lon: 13.4, Accuracy: 30, Time: "2026-11-07T11:50:00Z", Place: "Mitte, Berlin"}})
	put("pad", api.DeviceContextPutParams{Location: &api.DeviceLocation{Lat: 48.1, Lon: 11.5, Time: "2026-11-07T11:00:00Z", Place: "Munich"}})
	if f, _ := s.Location(); f.Place != "Mitte, Berlin" || f.Device != "phone" {
		t.Fatalf("an older fix from another device won: %+v", f)
	}
	put("pad", api.DeviceContextPutParams{Location: &api.DeviceLocation{Lat: 48.1, Lon: 11.5, Time: "2026-11-07T11:55:00Z", Place: "Munich"}})
	if f, _ := s.Location(); f.Device != "pad" {
		t.Fatalf("the newest fix did not win: %+v", f)
	}
	put("pad", api.DeviceContextPutParams{LocationOff: ReasonOff})
	if _, why := s.Location(); why != ReasonOff {
		t.Fatalf("off kept the fix: why %q", why)
	}
	if err := s.Put("phone", api.DeviceContextPutParams{TZ: "Mars/Olympus"}); !errors.Is(err, ErrBadZone) {
		t.Fatalf("a bad zone: %v", err)
	}
	// The folder holds the zone file only, and it names no place.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != DeviceFile {
		t.Fatalf("files: %v", ents)
	}
	b, _ := os.ReadFile(filepath.Join(dir, DeviceFile))
	if strings.Contains(string(b), "52.5") || strings.Contains(string(b), "Mitte") || !strings.Contains(string(b), "Europe/Berlin") {
		t.Fatalf("device.json: %s", b)
	}
	// A restart: the zone is back, the location is gone.
	s2, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Zone().Zone != "Europe/Berlin" {
		t.Fatalf("zone after restart: %+v", s2.Zone())
	}
	if _, why := s2.Location(); why != ReasonNothingYet {
		t.Fatalf("location after restart: %q", why)
	}
	if _, why := s2.Calendar(); why != ReasonNothingYet {
		t.Fatalf("calendar after restart: %q", why)
	}
}

func TestLocationTextAndToolLine(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	f := Fix{DeviceLocation: api.DeviceLocation{Lat: 52.52001, Lon: 13.40495, Accuracy: 20, Place: "Mitte, Berlin"}, Device: "phone", At: now.Add(-45 * time.Minute)}
	txt := LocationText(f, "", now, 30*time.Minute)
	if !strings.Contains(txt, "Mitte, Berlin") || !strings.Contains(txt, "52.52001") || !strings.Contains(txt, "45 min ago") || !strings.Contains(txt, "older than 30 minutes") {
		t.Fatalf("text: %s", txt)
	}
	if l := ToolLine(f, ""); l != "Mitte, Berlin" || strings.Contains(l, "52") {
		t.Fatalf("tool line: %q", l)
	}
	if txt := LocationText(Fix{}, ReasonNoPermission, now, 0); !strings.Contains(txt, "no location permission") {
		t.Fatalf("no permission: %s", txt)
	}
}

func TestCalendarText(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c := Calendar{Device: "phone", At: now.Add(-time.Hour), DeviceCalendar: api.DeviceCalendar{To: "2026-11-10T00:00:00Z", Events: []api.DeviceEvent{
		{Title: "Dentist", Start: "2026-11-07T15:00:00Z", End: "2026-11-07T16:00:00Z", Location: "Main St"},
		{Title: "Trip", Start: "2026-11-09T00:00:00Z", End: "2026-11-10T00:00:00Z", AllDay: true},
	}}}
	txt := CalendarText(c, "", now, now.Add(24*time.Hour), "", now, nil)
	if !strings.Contains(txt, "Dentist") || strings.Contains(txt, "Trip") {
		t.Fatalf("window: %s", txt)
	}
	txt = CalendarText(c, "", now, now.Add(10*24*time.Hour), "trip", now, nil)
	if strings.Contains(txt, "Dentist") || !strings.Contains(txt, "all day: Trip") || !strings.Contains(txt, "covers events up to") {
		t.Fatalf("query: %s", txt)
	}
}

// fakeWeatherKit answers the weather path and records each request.
func fakeWeatherKit(t *testing.T, hits *atomic.Int32, auths *[]string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*auths = append(*auths, r.Header.Get("Authorization"))
		if !strings.HasPrefix(r.URL.Path, "/api/v1/weather/en/") {
			http.NotFound(w, r)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		out := map[string]any{}
		for _, ds := range strings.Split(r.URL.Query().Get("dataSets"), ",") {
			switch ds {
			case "currentWeather":
				out[ds] = map[string]any{"asOf": "2026-11-07T12:00:00Z", "temperature": 11.2, "temperatureApparent": 9.8, "conditionCode": "Drizzle", "humidity": 0.81, "windSpeed": 14, "uvIndex": 1}
			case "forecastDaily":
				out[ds] = map[string]any{"days": []any{map[string]any{"forecastStart": "2026-11-07T00:00:00Z", "conditionCode": "Rain", "temperatureMax": 12, "temperatureMin": 6, "precipitationChance": 0.7}}}
			case "forecastHourly":
				out[ds] = map[string]any{"hours": []any{map[string]any{"forecastStart": "2026-11-07T13:00:00Z", "conditionCode": "Rain", "temperature": 11, "precipitationChance": 0.6}}}
			case "weatherAlerts":
				out[ds] = map[string]any{"alerts": []any{}}
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestWeatherCacheAndJWT(t *testing.T) {
	var hits, runs atomic.Int32
	var auths []string
	srv := fakeWeatherKit(t, &hits, &auths, 0)
	defer srv.Close()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cfg := WeatherConfig{KeyCommand: []string{"print-key"}, KeyID: "KID123", TeamID: "TEAM42", ServiceID: "com.example.weather", Cache: 10 * time.Minute}
	w := NewWeather(cfg, srv.URL, srv.Client(), clock)
	w.key.run = fakeRun(testKey(t), &runs)
	q := WeatherQuery{Lat: 52.52001, Lon: 13.40495, Kinds: []string{KindCurrent, KindDaily}}
	r, err := w.Get(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "Drizzle") || !strings.Contains(r.Text, WeatherMark) {
		t.Fatalf("text: %s", r.Text)
	}
	h, c := jwtParts(t, strings.TrimPrefix(auths[0], "Bearer "))
	if h["alg"] != "ES256" || h["kid"] != "KID123" || h["id"] != "TEAM42.com.example.weather" || c["iss"] != "TEAM42" || c["sub"] != "com.example.weather" {
		t.Fatalf("jwt header %v claims %v", h, c)
	}
	// A nearby fix rounds to the same key: the cache answers.
	q.Lat, q.Lon = 52.5249, 13.4011
	if _, err := w.Get(context.Background(), q); err != nil || hits.Load() != 1 {
		t.Fatalf("cached call reached the endpoint: hits %d err %v", hits.Load(), err)
	}
	// A kind not cached yet goes out alone.
	q.Kinds = []string{KindCurrent, KindHourly}
	if _, err := w.Get(context.Background(), q); err != nil || hits.Load() != 2 {
		t.Fatalf("hits %d err %v", hits.Load(), err)
	}
	// Past the cache's life: out again.
	now = now.Add(11 * time.Minute)
	if _, err := w.Get(context.Background(), q); err != nil || hits.Load() != 3 {
		t.Fatalf("expired: hits %d err %v", hits.Load(), err)
	}
	// cache 0: every call reaches the endpoint.
	w.SetCache(0)
	for range 3 {
		if _, err := w.Get(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 6 {
		t.Fatalf("cache 0: hits %d, want 6", hits.Load())
	}
	if runs.Load() != 1 {
		t.Fatalf("key command runs %d, want 1 (kept in memory)", runs.Load())
	}
}

func TestWeatherAlertsNeedCountry(t *testing.T) {
	var hits, runs atomic.Int32
	var auths []string
	var sets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		sets = append(sets, r.URL.Query().Get("dataSets")+"|"+r.URL.Query().Get("countryCode"))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_ = auths
	w := NewWeather(WeatherConfig{KeyCommand: []string{"k"}, KeyID: "k", TeamID: "t", ServiceID: "s"}, srv.URL, srv.Client(), nil)
	w.key.run = fakeRun(testKey(t), &runs)
	w.Get(context.Background(), WeatherQuery{Lat: 1, Lon: 2})
	w.Get(context.Background(), WeatherQuery{Lat: 1, Lon: 2, Country: "de"})
	if strings.Contains(sets[0], "weatherAlerts") || !strings.Contains(sets[1], "weatherAlerts") || !strings.HasSuffix(sets[1], "|DE") {
		t.Fatalf("data sets: %q", sets)
	}
}

func TestWeatherNotSetUpAndRefusedKey(t *testing.T) {
	if _, err := NewWeather(WeatherConfig{}, "", nil, nil).Get(context.Background(), WeatherQuery{}); !errors.Is(err, ErrNoWeather) {
		t.Fatalf("no config: %v", err)
	}
	var hits, runs atomic.Int32
	var auths []string
	srv := fakeWeatherKit(t, &hits, &auths, http.StatusUnauthorized)
	defer srv.Close()
	key := testKey(t)
	w := NewWeather(WeatherConfig{KeyCommand: []string{"k"}, KeyID: "k", TeamID: "t", ServiceID: "s"}, srv.URL, srv.Client(), nil)
	w.key.run = fakeRun(key, &runs)
	_, err := w.Get(context.Background(), WeatherQuery{Lat: 1, Lon: 2})
	if err == nil || !strings.Contains(err.Error(), "refused the key") || strings.Contains(err.Error(), string(key[30:60])) {
		t.Fatalf("refused: %v", err)
	}
	w.Get(context.Background(), WeatherQuery{Lat: 1, Lon: 2})
	if runs.Load() != 2 {
		t.Fatalf("a refused key was not read again: runs %d", runs.Load())
	}
}

func TestKeyCommandFailureNamesNoOutput(t *testing.T) {
	s := NewKeySecret("[capcom.weather] key_command", []string{"k"}, nil)
	s.run = func(_ context.Context, _ []string, use func([]byte) error) error {
		return use([]byte("not-a-key-SECRETVALUE"))
	}
	_, err := s.key(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("err %v", err)
	}
}

// fakeMaps is an Apple Maps Server API and a Google Places API in one server.
func fakeMaps(t *testing.T, calls *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/token":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"accessToken":"acc-1","expiresInSeconds":1800}`))
		case "/v1/geocode":
			w.Write([]byte(`{"results":[{"name":"Kreuzberg","coordinate":{"latitude":52.4986,"longitude":13.4030}}]}`))
		case "/v1/search":
			if r.Header.Get("Authorization") != "Bearer acc-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"results":[{"id":"I1","name":"Green Bowl","coordinate":{"latitude":52.4990,"longitude":13.4040},"formattedAddressLines":["Oranienstr. 1","Berlin"],"poiCategory":"Restaurant"},
				{"id":"I2","name":"Far Diner","coordinate":{"latitude":52.6,"longitude":13.6},"poiCategory":"Restaurant"}]}`))
		case "/v1/places:searchText":
			if r.Header.Get("X-Goog-Api-Key") != "gkey" || !strings.Contains(r.Header.Get("X-Goog-FieldMask"), "places.rating") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"textQuery":"vegetarian"`) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"places":[{"id":"G1","displayName":{"text":"Leaf"},"formattedAddress":"Somewhere 2","location":{"latitude":52.5,"longitude":13.4},"rating":4.6,"userRatingCount":210,"primaryType":"vegetarian_restaurant","googleMapsUri":"https://maps.google.com/?cid=1","currentOpeningHours":{"openNow":true}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPlacesAppleAndGoogle(t *testing.T) {
	var calls []string
	srv := fakeMaps(t, &calls)
	defer srv.Close()
	var runs atomic.Int32
	maps := MapsConfig{KeyCommand: []string{"k"}, KeyID: "MK1", TeamID: "TEAM42"}
	p := NewPlaces(maps, nil, srv.URL, srv.URL, srv.Client(), nil)
	p.key.run = fakeRun(testKey(t), &runs)
	ctx := context.Background()

	if _, err := p.Search(ctx, PlaceQuery{Query: "vegetarian", Source: SourceGoogle}); !errors.Is(err, ErrNoGoogle) {
		t.Fatalf("google with no key: %v", err)
	}
	out, err := p.Search(ctx, PlaceQuery{Query: "vegetarian", Near: "Kreuzberg", Radius: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "Green Bowl" || out[0].ID != "apple:I1" || out[0].Distance <= 0 || !strings.Contains(out[0].Link, "maps.apple.com") {
		t.Fatalf("apple results: %+v", out)
	}
	if txt := PlacesText(out); !strings.Contains(txt, "Oranienstr. 1, Berlin") || !strings.Contains(txt, "m away") {
		t.Fatalf("text: %s", txt)
	}
	// One token for both calls.
	tokens := 0
	for _, c := range calls {
		if c == "GET /v1/token" {
			tokens++
		}
	}
	if tokens != 1 {
		t.Fatalf("token trades %d: %v", tokens, calls)
	}

	var gruns atomic.Int32
	g := NewPlaces(maps, []string{"print-google-key"}, srv.URL, srv.URL, srv.Client(), nil)
	g.key.run = fakeRun(testKey(t), &runs)
	g.google.run = fakeRun([]byte("gkey\n"), &gruns)
	lat, lon := 52.5, 13.4
	out, err = g.Search(ctx, PlaceQuery{Query: "vegetarian", Lat: &lat, Lon: &lon, Source: SourceGoogle})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "google:G1" || out[0].Rating != 4.6 || out[0].OpenNow == nil || !*out[0].OpenNow {
		t.Fatalf("google results: %+v", out)
	}
	if _, err := g.Details(ctx, "nope", ""); err == nil {
		t.Fatal("a bad id was taken")
	}
}

func TestPlacesNotSetUp(t *testing.T) {
	p := NewPlaces(MapsConfig{}, nil, "", "", nil, nil)
	if _, err := p.Search(context.Background(), PlaceQuery{Query: "x"}); !errors.Is(err, ErrNoMaps) {
		t.Fatalf("no maps: %v", err)
	}
}
