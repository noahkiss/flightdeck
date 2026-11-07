package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/capcomthread"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/personal"
)

// Capcom's personal tools on the home hab (capcom-service.md 8): the device context the app
// sends, and the route methods the engine's MCP tools call. A route call from any pane is
// refused (E12): only the Capcom engine's token reaches them.

// capcomPersonal is the personal tools' state on the home hab.
type capcomPersonal struct {
	dev    *personal.Store
	config func() capcom.ServiceConfig
	turn   func() string // the engine's running turn; "" when idle
	thread *capcomthread.Store
	now    func() time.Time

	mu       sync.Mutex
	wx       *personal.Weather
	wxSig    string
	places   *personal.Places
	plSig    string
	wxTurns  []string // turns that used weather, newest last (8.3)
	maxTurns int
}

func newCapcomPersonal(dir string, config func() capcom.ServiceConfig, turn func() string, th *capcomthread.Store) (*capcomPersonal, error) {
	dev, err := personal.OpenStore(dir, nil)
	if err != nil {
		return nil, err
	}
	return &capcomPersonal{dev: dev, config: config, turn: turn, thread: th, now: time.Now, maxTurns: 16}, nil
}

// weather returns the WeatherKit client for the current config, made again when it changed.
func (p *capcomPersonal) weather() *personal.Weather {
	cfg := p.config().Personal.Weather
	sig := fmt.Sprint(cfg.KeyCommand, cfg.KeyID, cfg.TeamID, cfg.ServiceID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wx == nil || p.wxSig != sig {
		p.wx, p.wxSig = personal.NewWeather(cfg, weatherBaseSeam, nil, nil), sig
	}
	p.wx.SetCache(cfg.Cache)
	return p.wx
}

func (p *capcomPersonal) placesClient() *personal.Places {
	cfg := p.config().Personal
	sig := fmt.Sprint(cfg.Maps.KeyCommand, cfg.Maps.KeyID, cfg.Maps.TeamID, cfg.GoogleKeyCommand)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.places == nil || p.plSig != sig {
		p.places, p.plSig = personal.NewPlaces(cfg.Maps, cfg.GoogleKeyCommand, mapsBaseSeam, googleBaseSeam, nil, nil), sig
	}
	return p.places
}

// usedWeather marks the running turn: its replies carry the Apple Weather mark (8.3).
func (p *capcomPersonal) usedWeather() {
	t := p.turn()
	if t == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.wxTurns, t) {
		p.wxTurns = append(p.wxTurns, t)
		if len(p.wxTurns) > p.maxTurns {
			p.wxTurns = p.wxTurns[1:]
		}
	}
}

// attribution is the link's Attribution hook: a reply of a turn that used weather shows the
// mark.
func (p *capcomPersonal) attribution(turn string) []capcomthread.Attribution {
	p.mu.Lock()
	defer p.mu.Unlock()
	if turn != "" && slices.Contains(p.wxTurns, turn) {
		return []capcomthread.Attribution{{Source: "weather", Text: personal.WeatherMark, URL: personal.WeatherLink}}
	}
	return nil
}

// coords matches a decimal pair ("52.5,13.4", "52.52 13.40") or a lone number with 2 or more
// decimals: what a tool line must never show (8.1). A version such as 1.2.3 has no 2-decimal
// part and stays.
var coords = regexp.MustCompile(`-?\d{1,3}\.\d+(\s*[, ]\s*-?\d{1,3}\.\d+)?|-?\d{1,3}\.\d{2,}`)

// toolSummary is the link's ToolSummary hook (8.1, 8.2): device_location's line names the place
// the store holds; every tool line, a web search's too, loses coordinates.
func (p *capcomPersonal) toolSummary(name, summary string) string {
	short := strings.TrimPrefix(strings.TrimPrefix(name, "mcp__flightdeck__"), "mcp__flightdeck.")
	if short == "device_location" {
		f, why := p.dev.Location()
		return "device_location: " + personal.ToolLine(f, why)
	}
	return scrubCoords(summary)
}

var twoDecimals = regexp.MustCompile(`\.\d{2,}`)

func scrubCoords(s string) string {
	return strings.TrimSpace(coords.ReplaceAllStringFunc(s, func(m string) string {
		if !strings.ContainsAny(m, ", ") && !twoDecimals.MatchString(m) {
			return m // one number with one decimal, such as 2.5 hours
		}
		return "(a position)"
	}))
}


// personalResult is a personal route call's answer: its text for the model, and the data for
// -json.
type personalResult struct {
	Text string `json:"text"`
	Data any    `json:"data,omitempty"`
}

type weatherParams struct {
	Lat     *float64 `json:"lat"`
	Lon     *float64 `json:"lon"`
	Kinds   string   `json:"kinds,omitempty"` // comma separated
	Country string   `json:"country,omitempty"`
	Place   string   `json:"place,omitempty"` // the tool line's place name
}

type calendarReadParams struct {
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Query string `json:"query,omitempty"`
}

type placesParams struct {
	Query  string   `json:"query"`
	Near   string   `json:"near,omitempty"`
	Lat    *float64 `json:"lat,omitempty"`
	Lon    *float64 `json:"lon,omitempty"`
	Radius float64  `json:"radius,omitempty"`
	Source string   `json:"source,omitempty"`
}

type placeParams struct {
	ID string `json:"id"`
}

type cardParams struct {
	Title    string `json:"title"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	AllDay   bool   `json:"all_day,omitempty"`
	Due      string `json:"due,omitempty"`
	Location string `json:"location,omitempty"`
	Notes    string `json:"notes,omitempty"`
	URL      string `json:"url,omitempty"`
}

// hook is the route handler of the personal methods.
func (p *capcomPersonal) hook(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
	if pane != capcomRoute {
		return nil, nil, control.Errorf(api.ErrorKindDenied, "%s is a Capcom personal tool: only the Capcom service may read the user's location, calendar and places (capcom-service.md 8, E12)", method)
	}
	dec := func(v any) error {
		if err := json.Unmarshal(params, v); err != nil {
			return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err)
		}
		return nil
	}
	switch method {
	case "personal.location":
		f, why := p.dev.Location()
		res := personalResult{Text: personal.LocationText(f, why, p.now(), p.config().Personal.LocationStale)}
		if why == "" {
			res.Data = map[string]any{"place": f.Place, "lat": f.Lat, "lon": f.Lon, "accuracy": f.Accuracy, "time": f.Time, "device": f.Device, "country": f.Country}
		}
		return res, nil, nil
	case "personal.calendar":
		var q calendarReadParams
		if err := dec(&q); err != nil {
			return nil, nil, err
		}
		return p.calendarRead(q)
	case "personal.weather":
		var q weatherParams
		if err := dec(&q); err != nil {
			return nil, nil, err
		}
		return p.weatherCall(ctx, q)
	case "personal.places":
		var q placesParams
		if err := dec(&q); err != nil {
			return nil, nil, err
		}
		return p.placesSearch(ctx, q)
	case "personal.place":
		var q placeParams
		if err := dec(&q); err != nil {
			return nil, nil, err
		}
		pl, err := p.placesClient().Details(ctx, q.ID, "")
		if err != nil {
			return nil, nil, p.placesErr(err)
		}
		return personalResult{Text: personal.PlaceText(pl), Data: pl}, nil, nil
	case "personal.calendar_add", "personal.reminder_add":
		var q cardParams
		if err := dec(&q); err != nil {
			return nil, nil, err
		}
		return p.addCard(method == "personal.reminder_add", q)
	}
	return nil, nil, control.Errorf(api.ErrorKindInvalid, "no personal method %q", method)
}

func (p *capcomPersonal) zone() *time.Location {
	if z := p.dev.Zone().Zone; z != "" {
		if l, err := time.LoadLocation(z); err == nil {
			return l
		}
	}
	return time.Local
}

func (p *capcomPersonal) calendarRead(q calendarReadParams) (any, func(error), error) {
	loc := p.zone()
	now := p.now().In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from, to := day, day.AddDate(0, 0, 2) // today and tomorrow
	parse := func(s string, dst *time.Time) error {
		if s == "" {
			return nil
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			*dst = t
			return nil
		}
		if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
			*dst = t
			return nil
		}
		return control.Errorf(api.ErrorKindInvalid, "calendar_read: %q is not RFC 3339 or YYYY-MM-DD", s)
	}
	if err := parse(q.From, &from); err != nil {
		return nil, nil, err
	}
	if err := parse(q.To, &to); err != nil {
		return nil, nil, err
	}
	if q.To != "" && len(q.To) == len(time.DateOnly) {
		to = to.AddDate(0, 0, 1) // a date names the whole day
	}
	c, why := p.dev.Calendar()
	return personalResult{Text: personal.CalendarText(c, why, from, to, q.Query, p.now(), loc)}, nil, nil
}

func (p *capcomPersonal) weatherCall(ctx context.Context, q weatherParams) (any, func(error), error) {
	if q.Lat == nil || q.Lon == nil {
		return nil, nil, control.Errorf(api.ErrorKindInvalid, "weather needs lat and lon: take them from device_location or places_search")
	}
	var kinds []string
	for _, k := range strings.Split(q.Kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds = append(kinds, k)
		}
	}
	country := q.Country
	if f, why := p.dev.Location(); country == "" && why == "" && math.Abs(f.Lat-*q.Lat) < 0.5 && math.Abs(f.Lon-*q.Lon) < 0.5 {
		country = f.Country // near the held fix: its country, so alerts can come
	}
	w := p.weather()
	r, err := w.Get(ctx, personal.WeatherQuery{Lat: *q.Lat, Lon: *q.Lon, Kinds: kinds, Country: country, Zone: p.dev.Zone().Zone})
	if errors.Is(err, personal.ErrNoWeather) {
		return nil, nil, control.Errorf(api.ErrorKindUnavailable, "weather is not set up on this hab: tell the user to set [capcom.weather] key_command, key_id, team_id and service_id in the hab's config file, with a WeatherKit key from their Apple Developer account (capcom-service.md 8.3)")
	}
	if err != nil {
		return nil, nil, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	p.usedWeather()
	return personalResult{Text: r.Text, Data: r.Data}, nil, nil
}

func (p *capcomPersonal) placesErr(err error) error {
	switch {
	case errors.Is(err, personal.ErrNoMaps):
		return control.Errorf(api.ErrorKindUnavailable, "places are not set up on this hab: tell the user to set [capcom.maps] key_command, key_id and team_id in the hab's config file, with a Maps key from their Apple Developer account (capcom-service.md 8.4)")
	case errors.Is(err, personal.ErrNoGoogle):
		return control.Errorf(api.ErrorKindUnavailable, "no Google key on this hab: [capcom.places] google_key_command is not set")
	}
	var ce *control.Error
	if errors.As(err, &ce) {
		return err
	}
	return control.Errorf(api.ErrorKindUnavailable, "%v", err)
}

// placesSearch runs places_search. Without a Google key, source google is refused and Apple
// answers, and the text says so (8.4).
func (p *capcomPersonal) placesSearch(ctx context.Context, q placesParams) (any, func(error), error) {
	pl := p.placesClient()
	pq := personal.PlaceQuery{Query: q.Query, Near: q.Near, Lat: q.Lat, Lon: q.Lon, Radius: q.Radius, Source: q.Source}
	note := ""
	if q.Source == personal.SourceGoogle && !pl.HasGoogle() {
		note = "No Google key on this hab: source google is refused, and Apple Maps answered.\n"
		pq.Source = personal.SourceApple
	}
	out, err := pl.Search(ctx, pq)
	if err != nil {
		return nil, nil, p.placesErr(err)
	}
	return personalResult{Text: note + personal.PlacesText(out), Data: out}, nil, nil
}

// addCard adds a calendar or reminder card to the thread and returns at once (8.5, E14). The
// phone writes nothing until the user taps Add.
func (p *capcomPersonal) addCard(reminder bool, q cardParams) (any, func(error), error) {
	what := "calendar_add"
	if reminder {
		what = "reminder_add"
	}
	q.Title = strings.TrimSpace(q.Title)
	if q.Title == "" || len(q.Title) > 500 || len(q.Notes) > 8000 || len(q.Location) > 1000 || len(q.URL) > 2000 {
		return nil, nil, control.Errorf(api.ErrorKindInvalid, "%s needs a title (at most 500 bytes); notes at most 8000, location 1000, url 2000", what)
	}
	if q.URL != "" && !strings.HasPrefix(q.URL, "https://") && !strings.HasPrefix(q.URL, "http://") {
		return nil, nil, control.Errorf(api.ErrorKindInvalid, "%s: url must be http or https", what)
	}
	loc := p.zone()
	ev := &capcomthread.CardEvent{Title: q.Title, Location: q.Location, Notes: q.Notes, URL: q.URL, AllDay: q.AllDay, TZ: p.dev.Zone().Zone}
	var line string
	if reminder {
		if q.Due != "" {
			t, err := cardTime(q.Due, false, loc)
			if err != nil {
				return nil, nil, control.Errorf(api.ErrorKindInvalid, "reminder_add: due %v", err)
			}
			ev.Due = q.Due
			line = fmt.Sprintf("Remind: %s, %s", q.Title, t.In(loc).Format("Mon 2 Jan 15:04"))
		} else {
			line = "Remind: " + q.Title
		}
	} else {
		s, err := cardTime(q.Start, q.AllDay, loc)
		if err != nil {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "calendar_add: start %v", err)
		}
		end := q.End
		if end == "" {
			if q.AllDay {
				end = q.Start
			} else {
				end = s.Add(time.Hour).Format(time.RFC3339)
			}
		}
		e, err := cardTime(end, q.AllDay, loc)
		if err != nil {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "calendar_add: end %v", err)
		}
		if e.Before(s) || (!q.AllDay && !e.After(s)) {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "calendar_add: end is not after start")
		}
		ev.Start, ev.End = q.Start, end
		if q.AllDay {
			line = fmt.Sprintf("Add to calendar: %s, %s all day", q.Title, s.Format("Mon 2 Jan"))
		} else {
			line = fmt.Sprintf("Add to calendar: %s, %s to %s", q.Title, s.In(loc).Format("Mon 2 Jan 15:04"), e.In(loc).Format("15:04"))
		}
	}
	if q.Location != "" {
		line += ", at " + q.Location
	}
	typ := capcomthread.CardCalendar
	if reminder {
		typ = capcomthread.CardReminder
	}
	turn := p.turn()
	it, _, err := p.thread.Add(capcomthread.Item{Kind: capcomthread.KindCard, Author: capcomthread.Author{Kind: capcomthread.AuthorCapcom},
		Turn: turn, Text: line, Card: &capcomthread.Card{Type: typ, Choices: []string{capcomthread.ChoiceAdd, capcomthread.ChoiceDismiss}, Event: ev}})
	if err != nil {
		return nil, nil, control.Errorf(api.ErrorKindUnavailable, "store the card: %v", err)
	}
	return personalResult{Text: fmt.Sprintf("Card %s is in the thread: %s. Nothing is added until the user taps Add; their answer reaches you later as a note. Do not say it was added.", it.ID, line),
		Data: map[string]string{"card": it.ID}}, nil, nil
}

// cardTime reads a card's time: RFC 3339, or a date for an all-day event or a reminder's due.
func cardTime(s string, allDay bool, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("is missing")
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t, nil
	}
	if allDay {
		return time.Time{}, fmt.Errorf("%q: an all-day event takes a date, YYYY-MM-DD", s)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339 or YYYY-MM-DD", s)
	}
	return t, nil
}

// cardEnded reports whether a calendar card's event has ended, so Add is refused ("Past",
// 8.5). A reminder never ends.
func cardEnded(ev *capcomthread.CardEvent, typ string, now time.Time) bool {
	if ev == nil || typ != capcomthread.CardCalendar || ev.End == "" {
		return false
	}
	loc := time.UTC
	if ev.TZ != "" {
		if l, err := time.LoadLocation(ev.TZ); err == nil {
			loc = l
		}
	}
	e, err := cardTime(ev.End, ev.AllDay, loc)
	if err != nil {
		return false
	}
	if ev.AllDay {
		e = e.AddDate(0, 0, 1)
	}
	return !now.Before(e)
}

// DeviceContext serves device.context.put (8.1): the latest value of each field, in memory
// only; the time zone is saved. client is the device's label, which each value carries.
func (e *capcomEngine) DeviceContext(client string, p api.DeviceContextPutParams) error {
	for _, r := range []string{p.LocationOff, p.CalendarOff} {
		if r != "" && r != personal.ReasonOff && r != personal.ReasonNoPermission {
			return control.Errorf(api.ErrorKindInvalid, "device.context.put: %q is not off or no_permission", r)
		}
	}
	if p.Location != nil && p.LocationOff != "" || p.Calendar != nil && p.CalendarOff != "" {
		return control.Errorf(api.ErrorKindInvalid, "device.context.put: a value and its _off reason together")
	}
	if p.Calendar != nil && len(p.Calendar.Events) > 2000 {
		return control.Errorf(api.ErrorKindInvalid, "device.context.put: at most 2000 calendar events")
	}
	if err := e.personal.dev.Put(client, p); err != nil {
		return control.Errorf(api.ErrorKindInvalid, "device.context.put: %v", err)
	}
	return nil
}

// answerEventCard answers a calendar or reminder card (8.5): the first answer wins across
// devices; the model gets the outcome as a note. The device that taps Add writes the event; the
// hab writes nothing.
func (e *capcomEngine) answerEventCard(it capcomthread.Item, client, choice string) (api.CapcomCardAnswered, error) {
	if it.Answer != nil {
		return api.CapcomCardAnswered{Item: apiItem(it)}, control.Errorf(api.ErrorKindConflict, "card %s was answered already (%s on %s)", it.ID, it.Answer.Choice, it.Answer.By)
	}
	if choice == capcomthread.ChoiceAdd && cardEnded(it.Card.Event, it.Card.Type, time.Now()) {
		return api.CapcomCardAnswered{Item: apiItem(it)}, control.Errorf(api.ErrorKindInvalid, "card %s: the event has ended", it.ID)
	}
	done, err := e.thread.AnswerCard(it.ID, choice, client)
	if errors.Is(err, capcomthread.ErrConflict) {
		return api.CapcomCardAnswered{Item: apiItem(done)}, control.Errorf(api.ErrorKindConflict, "card %s was answered already", it.ID)
	}
	if err != nil {
		return api.CapcomCardAnswered{}, err
	}
	title := ""
	if it.Card.Event != nil {
		title = it.Card.Event.Title
	}
	where := "calendar"
	if it.Card.Type == capcomthread.CardReminder {
		where = "reminders"
	}
	text := fmt.Sprintf("The user dismissed the card %s (%q); nothing was added.", it.ID, title)
	if choice == capcomthread.ChoiceAdd {
		text = fmt.Sprintf("The user added %q to their %s on %s (card %s).", title, where, client, it.ID)
	}
	e.addNote(capcomthread.Item{Kind: capcomthread.KindNote, Author: capcomthread.Author{Kind: capcomthread.AuthorFlightdeck}, Text: text, Model: true})
	return api.CapcomCardAnswered{Item: apiItem(done)}, nil
}
