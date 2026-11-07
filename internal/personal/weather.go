package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// WeatherKit's REST base. Tests point Weather at a fake server.
const WeatherKitBase = "https://weatherkit.apple.com"

// The attribution WeatherKit's terms ask for wherever its data shows (8.3): the Apple Weather
// mark and a link to its data sources.
const (
	WeatherMark = "Apple Weather"
	WeatherLink = "https://developer.apple.com/weatherkit/data-source-attribution/"
)

// Weather kinds a call may ask for (8.3).
const (
	KindCurrent = "current"
	KindHourly  = "hourly"
	KindDaily   = "daily"
	KindAlerts  = "alerts"
)

// WeatherKinds lists them in the order a reply shows them.
var WeatherKinds = []string{KindCurrent, KindHourly, KindDaily, KindAlerts}

var dataSet = map[string]string{KindCurrent: "currentWeather", KindHourly: "forecastHourly", KindDaily: "forecastDaily", KindAlerts: "weatherAlerts"}

// WeatherConfig is [capcom.weather] (8.3, 19.2).
type WeatherConfig struct {
	KeyCommand []string
	KeyID      string
	TeamID     string
	ServiceID  string
	Cache      time.Duration // 0: no cache
}

// Configured reports whether every key the JWT needs is set.
func (c WeatherConfig) Configured() bool {
	return len(c.KeyCommand) > 0 && c.KeyID != "" && c.TeamID != "" && c.ServiceID != ""
}

// Weather calls WeatherKit REST with a JWT gff-hab signs (8.3), and caches each kind per
// location rounded to 2 decimal places.
type Weather struct {
	cfg    WeatherConfig
	key    *Secret
	base   string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	raw json.RawMessage
	at  time.Time
}

// NewWeather returns the client. base "" is WeatherKitBase; client nil is a client with a 15 s
// timeout; now nil is time.Now.
func NewWeather(cfg WeatherConfig, base string, client *http.Client, now func() time.Time) *Weather {
	if base == "" {
		base = WeatherKitBase
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Weather{cfg: cfg, key: NewKeySecret("[capcom.weather] key_command", cfg.KeyCommand, now), base: strings.TrimRight(base, "/"),
		client: client, now: now, cache: map[string]cached{}}
}

// SetCache changes the cache's lifetime (a config read again).
func (w *Weather) SetCache(d time.Duration) {
	w.mu.Lock()
	w.cfg.Cache = d
	w.mu.Unlock()
}

// WeatherQuery is one weather call.
type WeatherQuery struct {
	Lat, Lon float64
	Kinds    []string // empty: every kind
	Country  string   // ISO code; alerts need it
	Lang     string   // "" is en
	Zone     string   // the device's zone for the times shown; "" is UTC
}

// ErrNoWeather means [capcom.weather] is not set up.
var ErrNoWeather = errors.New("weather is not set up")

// Report is a weather call's answer: the raw data sets by kind, for -json, and its text.
type Report struct {
	Data map[string]json.RawMessage `json:"data"`
	Text string                     `json:"text"`
}

// Get returns the kinds asked for. Each kind comes from the cache while it is younger than
// [capcom.weather] cache; the rest come in one request.
func (w *Weather) Get(ctx context.Context, q WeatherQuery) (Report, error) {
	if !w.cfg.Configured() {
		return Report{}, ErrNoWeather
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = WeatherKinds
	}
	for _, k := range kinds {
		if _, ok := dataSet[k]; !ok {
			return Report{}, fmt.Errorf("weather kind %q: want one of %s", k, strings.Join(WeatherKinds, ", "))
		}
	}
	if q.Country == "" {
		// Alerts need a country; without one they are left out, not refused.
		kinds = slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k == KindAlerts })
	}
	lat, lon := round2(q.Lat), round2(q.Lon)
	got := map[string]json.RawMessage{}
	var want []string
	w.mu.Lock()
	life := w.cfg.Cache
	for _, k := range kinds {
		c, ok := w.cache[cacheKey(lat, lon, k)]
		if ok && life > 0 && w.now().Sub(c.at) < life {
			got[k] = c.raw
		} else {
			want = append(want, k)
		}
	}
	w.mu.Unlock()
	if len(want) > 0 {
		fresh, err := w.fetch(ctx, lat, lon, want, q)
		if err != nil {
			return Report{}, err
		}
		w.mu.Lock()
		for k, raw := range fresh {
			got[k] = raw
			if life > 0 {
				w.cache[cacheKey(lat, lon, k)] = cached{raw: raw, at: w.now()}
			}
		}
		w.prune(life)
		w.mu.Unlock()
	}
	return Report{Data: got, Text: weatherText(got, kinds, q.Zone, w.now())}, nil
}

// prune drops cache entries past their life. The caller holds w.mu.
func (w *Weather) prune(life time.Duration) {
	for k, c := range w.cache {
		if life <= 0 || w.now().Sub(c.at) >= life {
			delete(w.cache, k)
		}
	}
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func cacheKey(lat, lon float64, kind string) string { return fmt.Sprintf("%.2f,%.2f,%s", lat, lon, kind) }

func (w *Weather) token(ctx context.Context) (string, error) {
	key, err := w.key.key(ctx)
	if err != nil {
		return "", err
	}
	now := w.now()
	return signJWT(key, map[string]any{"kid": w.cfg.KeyID, "id": w.cfg.TeamID + "." + w.cfg.ServiceID},
		map[string]any{"iss": w.cfg.TeamID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "sub": w.cfg.ServiceID})
}

func (w *Weather) fetch(ctx context.Context, lat, lon float64, kinds []string, q WeatherQuery) (map[string]json.RawMessage, error) {
	tok, err := w.token(ctx)
	if err != nil {
		return nil, err
	}
	lang := q.Lang
	if lang == "" {
		lang = "en"
	}
	sets := make([]string, 0, len(kinds))
	for _, k := range kinds {
		sets = append(sets, dataSet[k])
	}
	v := url.Values{"dataSets": {strings.Join(sets, ",")}}
	if q.Zone != "" {
		v.Set("timezone", q.Zone)
	}
	if q.Country != "" {
		v.Set("countryCode", strings.ToUpper(q.Country))
	}
	u := fmt.Sprintf("%s/api/v1/weather/%s/%.2f/%.2f?%s", w.base, url.PathEscape(lang), lat, lon, v.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("WeatherKit: %v", scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("WeatherKit: %v", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		w.key.Drop()
		return nil, fmt.Errorf("WeatherKit refused the key (%s): check [capcom.weather] key_id, team_id, service_id and that the key has WeatherKit", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WeatherKit answered %s", resp.Status)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("WeatherKit: not JSON: %v", err)
	}
	out := map[string]json.RawMessage{}
	for _, k := range kinds {
		if raw, ok := all[dataSet[k]]; ok {
			out[k] = raw
		} else {
			out[k] = json.RawMessage("null")
		}
	}
	return out, nil
}

// scrub keeps a transport error's text short: a *url.Error names the URL, which holds the
// coordinates; the error kind is enough.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// --- Text ---

type wxCurrent struct {
	AsOf        string  `json:"asOf"`
	Temperature float64 `json:"temperature"`
	Apparent    float64 `json:"temperatureApparent"`
	Condition   string  `json:"conditionCode"`
	Humidity    float64 `json:"humidity"`
	Wind        float64 `json:"windSpeed"`
	UV          float64 `json:"uvIndex"`
}

type wxHour struct {
	Start       string  `json:"forecastStart"`
	Temperature float64 `json:"temperature"`
	Condition   string  `json:"conditionCode"`
	PrecipP     float64 `json:"precipitationChance"`
}

type wxDay struct {
	Start     string  `json:"forecastStart"`
	Condition string  `json:"conditionCode"`
	Max       float64 `json:"temperatureMax"`
	Min       float64 `json:"temperatureMin"`
	PrecipP   float64 `json:"precipitationChance"`
}

type wxAlert struct {
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Effective   string `json:"effectiveTime"`
	Expires     string `json:"expireTime"`
	Source      string `json:"source"`
}

func weatherText(got map[string]json.RawMessage, kinds []string, zone string, now time.Time) string {
	loc := time.UTC
	if zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	at := func(s, layout string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		return t.In(loc).Format(layout)
	}
	var b strings.Builder
	for _, k := range kinds {
		raw, ok := got[k]
		if !ok {
			continue
		}
		switch k {
		case KindCurrent:
			var c wxCurrent
			if json.Unmarshal(raw, &c) != nil || c.Condition == "" {
				b.WriteString("Now: no data\n")
				continue
			}
			fmt.Fprintf(&b, "Now (%s): %s, %.0f°C (feels %.0f°C), humidity %.0f%%, wind %.0f km/h, UV %.0f\n",
				at(c.AsOf, "15:04"), c.Condition, c.Temperature, c.Apparent, c.Humidity*100, c.Wind, c.UV)
		case KindHourly:
			var h struct {
				Hours []wxHour `json:"hours"`
			}
			json.Unmarshal(raw, &h)
			b.WriteString("Next 24 hours:\n")
			n := 0
			for _, x := range h.Hours {
				t, err := time.Parse(time.RFC3339, x.Start)
				if err == nil && t.Before(now.Truncate(time.Hour)) {
					continue
				}
				if n == 24 {
					break
				}
				n++
				fmt.Fprintf(&b, "- %s %s %.0f°C, rain %.0f%%\n", at(x.Start, "Mon 15:04"), x.Condition, x.Temperature, x.PrecipP*100)
			}
			if n == 0 {
				b.WriteString("- no data\n")
			}
		case KindDaily:
			var d struct {
				Days []wxDay `json:"days"`
			}
			json.Unmarshal(raw, &d)
			b.WriteString("Next 7 days:\n")
			for i, x := range d.Days {
				if i == 7 {
					break
				}
				fmt.Fprintf(&b, "- %s %s %.0f to %.0f°C, rain %.0f%%\n", at(x.Start, "Mon 2 Jan"), x.Condition, x.Min, x.Max, x.PrecipP*100)
			}
			if len(d.Days) == 0 {
				b.WriteString("- no data\n")
			}
		case KindAlerts:
			var a struct {
				Alerts []wxAlert `json:"alerts"`
			}
			json.Unmarshal(raw, &a)
			if len(a.Alerts) == 0 {
				b.WriteString("Alerts: none\n")
				continue
			}
			b.WriteString("Alerts:\n")
			for _, x := range a.Alerts {
				fmt.Fprintf(&b, "- %s (%s, %s), %s to %s\n", x.Description, x.Severity, x.Source, at(x.Effective, "Mon 15:04"), at(x.Expires, "Mon 15:04"))
			}
		}
	}
	fmt.Fprintf(&b, "Source: %s (%s). Flightdeck shows this mark with your reply.\n", WeatherMark, WeatherLink)
	return b.String()
}
