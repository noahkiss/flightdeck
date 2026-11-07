package personal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The services' bases. Tests point Places at fake servers.
const (
	AppleMapsBase = "https://maps-api.apple.com"
	GooglePlaces  = "https://places.googleapis.com"
)

// Place sources (8.4).
const (
	SourceApple  = "apple"
	SourceGoogle = "google"
)

// MapsConfig is [capcom.maps]: the Apple Maps key (8.4).
type MapsConfig struct {
	KeyCommand []string
	KeyID      string
	TeamID     string
}

// Configured reports whether every key the Maps token needs is set.
func (c MapsConfig) Configured() bool {
	return len(c.KeyCommand) > 0 && c.KeyID != "" && c.TeamID != ""
}

// Place is one place a search or a lookup found.
type Place struct {
	ID       string   `json:"id"` // apple:<id> or google:<id>
	Name     string   `json:"name"`
	Address  string   `json:"address,omitempty"`
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	Category string   `json:"category,omitempty"`
	Distance float64  `json:"distance_m,omitempty"` // from the search's centre; 0 with none
	Link     string   `json:"link"`
	Rating   float64  `json:"rating,omitempty"`  // google
	Ratings  int      `json:"ratings,omitempty"` // google: how many
	OpenNow  *bool    `json:"open_now,omitempty"`
	Hours    []string `json:"hours,omitempty"`   // details
	Reviews  []string `json:"reviews,omitempty"` // google details
	Phone    string   `json:"phone,omitempty"`
	Website  string   `json:"website,omitempty"`
}

// PlaceQuery is one places_search call.
type PlaceQuery struct {
	Query    string
	Near     string   // a place name to geocode
	Lat, Lon *float64 // the centre
	Radius   float64  // metres; 0: none
	Source   string   // "" is apple
	Lang     string
}

// Places answers places_search and place_details from Apple Maps, and from Google with the
// user's own key (8.4, decision 4).
type Places struct {
	maps   MapsConfig
	key    *Secret // the Maps key
	google *Secret // nil: no Google key
	apple  string
	gbase  string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	access  string
	expires time.Time
}

// NewPlaces returns the client. googleCmd empty means no Google key. Bases "" are the real
// services; client nil has a 15 s timeout; now nil is time.Now.
func NewPlaces(maps MapsConfig, googleCmd []string, appleBase, googleBase string, client *http.Client, now func() time.Time) *Places {
	if appleBase == "" {
		appleBase = AppleMapsBase
	}
	if googleBase == "" {
		googleBase = GooglePlaces
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	p := &Places{maps: maps, key: NewKeySecret("[capcom.maps] key_command", maps.KeyCommand, now), apple: strings.TrimRight(appleBase, "/"),
		gbase: strings.TrimRight(googleBase, "/"), client: client, now: now}
	if len(googleCmd) > 0 {
		p.google = NewTokenSecret("[capcom.places] google_key_command", googleCmd, now)
	}
	return p
}

// ErrNoMaps means [capcom.maps] is not set up.
var ErrNoMaps = errors.New("places are not set up")

// ErrNoGoogle means no Google key is set up: source google is refused and Apple answers.
var ErrNoGoogle = errors.New("no Google key")

// HasGoogle reports whether a Google key command is set.
func (p *Places) HasGoogle() bool { return p.google != nil }

// Search runs places_search. A source of google without a Google key is ErrNoGoogle; the caller
// answers from Apple and says so.
func (p *Places) Search(ctx context.Context, q PlaceQuery) ([]Place, error) {
	switch q.Source {
	case "", SourceApple:
	case SourceGoogle:
		if p.google == nil {
			return nil, ErrNoGoogle
		}
	default:
		return nil, fmt.Errorf("source %q: want apple or google", q.Source)
	}
	if strings.TrimSpace(q.Query) == "" {
		return nil, errors.New("places_search needs a query")
	}
	lat, lon, centre := 0.0, 0.0, false
	if q.Lat != nil && q.Lon != nil {
		lat, lon, centre = *q.Lat, *q.Lon, true
	}
	if q.Near != "" {
		// Apple geocodes the name for either source: one geocoder, one answer.
		if !p.maps.Configured() {
			return nil, ErrNoMaps
		}
		g, err := p.geocode(ctx, q.Near, q.Lang)
		if err != nil {
			return nil, err
		}
		lat, lon, centre = g.Lat, g.Lon, true
	}
	var out []Place
	var err error
	if q.Source == SourceGoogle {
		out, err = p.googleSearch(ctx, q, lat, lon, centre)
	} else {
		if !p.maps.Configured() {
			return nil, ErrNoMaps
		}
		out, err = p.appleSearch(ctx, q, lat, lon, centre)
	}
	if err != nil {
		return nil, err
	}
	if centre {
		for i := range out {
			out[i].Distance = math.Round(distance(lat, lon, out[i].Lat, out[i].Lon))
		}
	}
	if centre && q.Radius > 0 {
		kept := out[:0]
		for _, x := range out {
			if x.Distance <= q.Radius {
				kept = append(kept, x)
			}
		}
		out = kept
	}
	return out, nil
}

// Details runs place_details for an id a search returned.
func (p *Places) Details(ctx context.Context, id, lang string) (Place, error) {
	src, raw, ok := strings.Cut(id, ":")
	if !ok || raw == "" {
		return Place{}, fmt.Errorf("place id %q: want apple:<id> or google:<id>, as places_search returns", id)
	}
	switch src {
	case SourceApple:
		if !p.maps.Configured() {
			return Place{}, ErrNoMaps
		}
		return p.appleDetails(ctx, raw, lang)
	case SourceGoogle:
		if p.google == nil {
			return Place{}, ErrNoGoogle
		}
		return p.googleDetails(ctx, raw, lang)
	}
	return Place{}, fmt.Errorf("place id %q: want apple:<id> or google:<id>", id)
}

// distance is the great-circle distance in metres.
func distance(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dlat, dlon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// --- Apple Maps Server API ---

// accessToken trades a signed Maps token for an access token, kept until a minute before it
// expires.
func (p *Places) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.access != "" && p.now().Before(p.expires) {
		return p.access, nil
	}
	key, err := p.key.key(ctx)
	if err != nil {
		return "", err
	}
	now := p.now()
	jwt, err := signJWT(key, map[string]any{"kid": p.maps.KeyID, "typ": "JWT"},
		map[string]any{"iss": p.maps.TeamID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	if err != nil {
		return "", err
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		Expires     int    `json:"expiresInSeconds"`
	}
	if err := p.get(ctx, p.apple+"/v1/token", map[string]string{"Authorization": "Bearer " + jwt}, &tok, "Apple Maps"); err != nil {
		var se statusErr
		if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
			p.key.Drop()
			return "", fmt.Errorf("Apple Maps refused the key (%s): check [capcom.maps] key_id, team_id and that the key has Maps", se.status)
		}
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("Apple Maps gave no access token")
	}
	life := time.Duration(max(tok.Expires, 120)) * time.Second
	p.access, p.expires = tok.AccessToken, now.Add(life-time.Minute)
	return p.access, nil
}

type statusErr struct {
	code   int
	status string
	what   string
}

func (e statusErr) Error() string { return fmt.Sprintf("%s answered %s", e.what, e.status) }

func (p *Places) get(ctx context.Context, u string, hdr map[string]string, out any, what string) error {
	return p.do(ctx, http.MethodGet, u, nil, hdr, out, what)
}

func (p *Places) do(ctx context.Context, method, u string, body []byte, hdr map[string]string, out any, what string) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %v", what, scrub(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		return statusErr{resp.StatusCode, resp.Status, what}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: not JSON: %v", what, err)
	}
	return nil
}

func (p *Places) apple1(ctx context.Context, path string, v url.Values, out any) error {
	tok, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	err = p.get(ctx, p.apple+path+"?"+v.Encode(), map[string]string{"Authorization": "Bearer " + tok}, out, "Apple Maps")
	var se statusErr
	if errors.As(err, &se) && se.code == http.StatusUnauthorized {
		p.mu.Lock()
		p.access = "" // an access token that expired early: the next call trades again
		p.mu.Unlock()
	}
	return err
}

type applePlace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Coordinate struct {
		Lat float64 `json:"latitude"`
		Lon float64 `json:"longitude"`
	} `json:"coordinate"`
	Address  []string `json:"formattedAddressLines"`
	Category string   `json:"poiCategory"`
	Phone    string   `json:"telephone"`
	URL      string   `json:"url"`
}

func (a applePlace) place() Place {
	pl := Place{Name: a.Name, Address: strings.Join(a.Address, ", "), Lat: a.Coordinate.Lat, Lon: a.Coordinate.Lon,
		Category: a.Category, Phone: a.Phone, Website: a.URL}
	if a.ID != "" {
		pl.ID = SourceApple + ":" + a.ID
		pl.Link = "https://maps.apple.com/place?place-id=" + url.QueryEscape(a.ID)
	} else {
		pl.Link = fmt.Sprintf("https://maps.apple.com/?q=%s&ll=%.6f,%.6f", url.QueryEscape(a.Name), pl.Lat, pl.Lon)
	}
	if pl.Name == "" && len(a.Address) > 0 {
		pl.Name = a.Address[0]
	}
	return pl
}

func lang(l string) string {
	if l == "" {
		return "en-US"
	}
	return l
}

func (p *Places) geocode(ctx context.Context, q, l string) (Place, error) {
	var r struct {
		Results []applePlace `json:"results"`
	}
	if err := p.apple1(ctx, "/v1/geocode", url.Values{"q": {q}, "lang": {lang(l)}}, &r); err != nil {
		return Place{}, err
	}
	if len(r.Results) == 0 {
		return Place{}, fmt.Errorf("Apple Maps found no place named %q", q)
	}
	return r.Results[0].place(), nil
}

// ReverseGeocode names a coordinate (8.1: a fix that came with no place name).
func (p *Places) ReverseGeocode(ctx context.Context, lat, lon float64, l string) (Place, error) {
	if !p.maps.Configured() {
		return Place{}, ErrNoMaps
	}
	var r struct {
		Results []applePlace `json:"results"`
	}
	if err := p.apple1(ctx, "/v1/reverseGeocode", url.Values{"loc": {fmt.Sprintf("%.6f,%.6f", lat, lon)}, "lang": {lang(l)}}, &r); err != nil {
		return Place{}, err
	}
	if len(r.Results) == 0 {
		return Place{}, errors.New("Apple Maps found no name for the place")
	}
	return r.Results[0].place(), nil
}

func (p *Places) appleSearch(ctx context.Context, q PlaceQuery, lat, lon float64, centre bool) ([]Place, error) {
	v := url.Values{"q": {q.Query}, "lang": {lang(q.Lang)}}
	if centre {
		v.Set("searchLocation", fmt.Sprintf("%.6f,%.6f", lat, lon))
		if q.Radius > 0 {
			// north, east, south, west around the centre
			dlat := q.Radius / 111320
			dlon := q.Radius / (111320 * math.Max(math.Cos(lat*math.Pi/180), 0.01))
			v.Set("searchRegion", fmt.Sprintf("%.6f,%.6f,%.6f,%.6f", lat+dlat, lon+dlon, lat-dlat, lon-dlon))
			v.Del("searchLocation")
		}
	}
	var r struct {
		Results []applePlace `json:"results"`
	}
	if err := p.apple1(ctx, "/v1/search", v, &r); err != nil {
		return nil, err
	}
	out := make([]Place, 0, len(r.Results))
	for _, x := range r.Results {
		out = append(out, x.place())
	}
	return out, nil
}

func (p *Places) appleDetails(ctx context.Context, id, l string) (Place, error) {
	tok, err := p.accessToken(ctx)
	if err != nil {
		return Place{}, err
	}
	var a applePlace
	if err := p.get(ctx, p.apple+"/v1/place/"+url.PathEscape(id)+"?lang="+url.QueryEscape(lang(l)),
		map[string]string{"Authorization": "Bearer " + tok}, &a, "Apple Maps"); err != nil {
		return Place{}, err
	}
	if a.ID == "" {
		a.ID = id
	}
	return a.place(), nil
}

// --- Google Places API (New), only with the user's own key ---

type googlePlace struct {
	ID          string `json:"id"`
	DisplayName struct {
		Text string `json:"text"`
	} `json:"displayName"`
	Address  string `json:"formattedAddress"`
	Location struct {
		Lat float64 `json:"latitude"`
		Lon float64 `json:"longitude"`
	} `json:"location"`
	Rating  float64 `json:"rating"`
	Ratings int     `json:"userRatingCount"`
	Type    string  `json:"primaryType"`
	MapsURI string  `json:"googleMapsUri"`
	Website string  `json:"websiteUri"`
	Phone   string  `json:"internationalPhoneNumber"`
	OpenNow *struct {
		OpenNow *bool `json:"openNow"`
	} `json:"currentOpeningHours"`
	Regular *struct {
		Weekday []string `json:"weekdayDescriptions"`
	} `json:"regularOpeningHours"`
	Reviews []struct {
		Rating float64 `json:"rating"`
		Text   struct {
			Text string `json:"text"`
		} `json:"text"`
	} `json:"reviews"`
}

func (g googlePlace) place() Place {
	pl := Place{ID: SourceGoogle + ":" + g.ID, Name: g.DisplayName.Text, Address: g.Address, Lat: g.Location.Lat, Lon: g.Location.Lon,
		Category: g.Type, Link: g.MapsURI, Rating: g.Rating, Ratings: g.Ratings, Website: g.Website, Phone: g.Phone}
	if g.OpenNow != nil {
		pl.OpenNow = g.OpenNow.OpenNow
	}
	if g.Regular != nil {
		pl.Hours = g.Regular.Weekday
	}
	for i, r := range g.Reviews {
		if i == 3 {
			break
		}
		t := strings.Join(strings.Fields(r.Text.Text), " ")
		if len([]rune(t)) > 300 {
			t = string([]rune(t)[:299]) + "…"
		}
		pl.Reviews = append(pl.Reviews, fmt.Sprintf("%.0f/5: %s", r.Rating, t))
	}
	if pl.Link == "" {
		pl.Link = "https://www.google.com/maps/search/?api=1&query_place_id=" + url.QueryEscape(g.ID) + "&query=" + url.QueryEscape(pl.Name)
	}
	return pl
}

const googleSearchFields = "places.id,places.displayName,places.formattedAddress,places.location,places.rating,places.userRatingCount,places.primaryType,places.googleMapsUri,places.currentOpeningHours.openNow"
const googleDetailFields = "id,displayName,formattedAddress,location,rating,userRatingCount,primaryType,googleMapsUri,websiteUri,internationalPhoneNumber,currentOpeningHours.openNow,regularOpeningHours.weekdayDescriptions,reviews"

func (p *Places) googleKey(ctx context.Context) (string, error) { return p.google.token(ctx) }

func (p *Places) googleCall(ctx context.Context, method, u string, body []byte, fields string, out any) error {
	key, err := p.googleKey(ctx)
	if err != nil {
		return err
	}
	hdr := map[string]string{"X-Goog-Api-Key": key, "X-Goog-FieldMask": fields}
	if body != nil {
		hdr["Content-Type"] = "application/json"
	}
	err = p.do(ctx, method, u, body, hdr, out, "Google Places")
	var se statusErr
	if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
		p.google.Drop()
		return fmt.Errorf("Google Places refused the key (%s): check [capcom.places] google_key_command", se.status)
	}
	return err
}

func (p *Places) googleSearch(ctx context.Context, q PlaceQuery, lat, lon float64, centre bool) ([]Place, error) {
	req := map[string]any{"textQuery": q.Query}
	if q.Lang != "" {
		req["languageCode"] = q.Lang
	}
	if centre {
		r := q.Radius
		if r <= 0 {
			r = 5000
		}
		req["locationBias"] = map[string]any{"circle": map[string]any{"center": map[string]float64{"latitude": lat, "longitude": lon}, "radius": math.Min(r, 50000)}}
	}
	body, _ := json.Marshal(req)
	var r struct {
		Places []googlePlace `json:"places"`
	}
	if err := p.googleCall(ctx, http.MethodPost, p.gbase+"/v1/places:searchText", body, googleSearchFields, &r); err != nil {
		return nil, err
	}
	out := make([]Place, 0, len(r.Places))
	for _, x := range r.Places {
		out = append(out, x.place())
	}
	return out, nil
}

func (p *Places) googleDetails(ctx context.Context, id, l string) (Place, error) {
	u := p.gbase + "/v1/places/" + url.PathEscape(id)
	if l != "" {
		u += "?languageCode=" + url.QueryEscape(l)
	}
	var g googlePlace
	if err := p.googleCall(ctx, http.MethodGet, u, nil, googleDetailFields, &g); err != nil {
		return Place{}, err
	}
	if g.ID == "" {
		g.ID = id
	}
	return g.place(), nil
}

// PlacesText is a list of places as the model reads it.
func PlacesText(ps []Place) string {
	if len(ps) == 0 {
		return "No places found."
	}
	var b strings.Builder
	for i, x := range ps {
		fmt.Fprintf(&b, "%d. %s", i+1, x.Name)
		if x.Category != "" {
			fmt.Fprintf(&b, " (%s)", x.Category)
		}
		b.WriteString("\n")
		placeLines(&b, x)
	}
	return b.String()
}

// PlaceText is one place's details.
func PlaceText(x Place) string {
	var b strings.Builder
	b.WriteString(x.Name)
	if x.Category != "" {
		fmt.Fprintf(&b, " (%s)", x.Category)
	}
	b.WriteString("\n")
	placeLines(&b, x)
	for _, h := range x.Hours {
		fmt.Fprintf(&b, "   hours: %s\n", h)
	}
	for _, r := range x.Reviews {
		fmt.Fprintf(&b, "   review %s\n", r)
	}
	return b.String()
}

func placeLines(b *strings.Builder, x Place) {
	if x.Address != "" {
		fmt.Fprintf(b, "   %s\n", x.Address)
	}
	var parts []string
	if x.Distance > 0 {
		if x.Distance < 1000 {
			parts = append(parts, fmt.Sprintf("%.0f m away", x.Distance))
		} else {
			parts = append(parts, fmt.Sprintf("%.1f km away", x.Distance/1000))
		}
	}
	if x.Rating > 0 {
		parts = append(parts, fmt.Sprintf("rated %.1f (%d)", x.Rating, x.Ratings))
	}
	if x.OpenNow != nil {
		if *x.OpenNow {
			parts = append(parts, "open now")
		} else {
			parts = append(parts, "closed now")
		}
	}
	parts = append(parts, fmt.Sprintf("at %.5f,%.5f", x.Lat, x.Lon))
	fmt.Fprintf(b, "   %s\n", strings.Join(parts, ", "))
	if x.Phone != "" {
		fmt.Fprintf(b, "   phone %s\n", x.Phone)
	}
	if x.Website != "" {
		fmt.Fprintf(b, "   web %s\n", x.Website)
	}
	fmt.Fprintf(b, "   map %s\n   id %s\n", x.Link, x.ID)
}
