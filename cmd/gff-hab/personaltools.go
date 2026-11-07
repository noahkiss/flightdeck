package main

import (
	"context"
	"strings"
)

// personalTools are Capcom's personal tools (capcom-service.md 8): `gff-hab mcp -capcom` lists
// them, as the Capcom service's folder runs it; a pane's `gff-hab mcp` does not. Each calls a
// personal.* method on the route, which the hab serves only to the Capcom engine's token (E12).
var personalTools = []kitTool{
	{Name: "device_location", Doc: "Read where the user is: the place name, coordinates, accuracy, the fix's age and the device, from the latest fix the user's phone shared. Call it only when the question depends on where the user is (nearby places, local weather, how far, the local time). Each call shows in the turn's tool line with the place name. When the fix is older than the hab's location_stale, say how old it is.",
		run: personalRun("personal.location", nil)},
	{Name: "calendar_read", Doc: "Read the user's calendar from the latest snapshot their phone shared: events in a window (default today and tomorrow), with the snapshot's age. The phone shares the next days it is set to; a window past them says so.",
		Args: []toolArg{{Name: "from", Type: "string", Doc: "RFC 3339 time, or a date YYYY-MM-DD. Absent: the start of today."},
			{Name: "to", Type: "string", Doc: "RFC 3339 time, or a date YYYY-MM-DD (the whole day). Absent: the end of tomorrow."},
			{Name: "query", Type: "string", Doc: "Only events whose title, location or calendar holds this, any case."}},
		run: personalRun("personal.calendar", nil)},
	{Name: "weather", Doc: "Weather from Apple Weather for a place: current conditions, the next 24 hours, the next 7 days and alerts. Take lat and lon from device_location or places_search. Flightdeck shows the Apple Weather mark with your reply; do not write it yourself.",
		Args: []toolArg{{Name: "lat", Type: "number", Doc: "Latitude.", Required: true}, {Name: "lon", Type: "number", Doc: "Longitude.", Required: true},
			{Name: "kinds", Type: "string", Doc: "Comma separated: current, hourly, daily, alerts. Absent: all."},
			{Name: "country", Type: "string", Doc: "The ISO country code, from device_location: alerts need it."},
			{Name: "place", Type: "string", Doc: "The place's name, for the tool line the user sees, such as Kreuzberg. Never coordinates."}},
		run: personalRun("personal.weather", map[string]string{"lat": "number", "lon": "number"})},
	{Name: "places_search", Doc: "Search places with Apple Maps: name, address, coordinates, category, distance from the centre and a Maps link for each. With source google and the user's own Google key, Google Places answers with ratings and open now; without that key, Apple answers and the result says so. Rank picks by the capcom guides you hold and name the preference you used.",
		Args: []toolArg{{Name: "query", Type: "string", Doc: "What to look for, such as vegetarian restaurant.", Required: true},
			{Name: "near", Type: "string", Doc: "A place name to search around, such as Kreuzberg, Berlin."},
			{Name: "lat", Type: "number", Doc: "The centre's latitude, such as from device_location."},
			{Name: "lon", Type: "number", Doc: "The centre's longitude."},
			{Name: "radius", Type: "number", Doc: "Metres from the centre: only places inside it."},
			{Name: "source", Type: "string", Doc: "apple (the default) or google."}},
		run: personalRun("personal.places", map[string]string{"lat": "number", "lon": "number", "radius": "number"})},
	{Name: "place_details", Doc: "Read one place by the id places_search returned: address, phone, website and Maps link; from Google also opening hours and a few reviews.",
		Args: []toolArg{{Name: "id", Type: "string", Doc: "apple:<id> or google:<id>, from places_search.", Required: true}},
		run:  personalRun("personal.place", nil)},
	{Name: "calendar_add", Action: true, Doc: "Offer the user a calendar event as a card in the thread, with an Add button. Nothing is written until the user taps Add on a device, which writes it to that device's calendar. Returns at once with the card id; the answer reaches you later as a note. Never say the event was added before that note.",
		Args: []toolArg{{Name: "title", Type: "string", Doc: "The event's title.", Required: true},
			{Name: "start", Type: "string", Doc: "RFC 3339 with the offset of the user's time zone; a date YYYY-MM-DD with all_day.", Required: true},
			{Name: "end", Type: "string", Doc: "RFC 3339; a date with all_day (the last day). Absent: an hour after start, or the same day."},
			{Name: "all_day", Type: "boolean", Doc: "An all-day event."},
			{Name: "location", Type: "string", Doc: "Where: a place's name and address."},
			{Name: "notes", Type: "string", Doc: "Notes for the event."},
			{Name: "url", Type: "string", Doc: "A link, such as the place's Maps link."}},
		run: personalRun("personal.calendar_add", map[string]string{"all_day": "boolean"})},
	{Name: "reminder_add", Action: true, Doc: "Offer the user a reminder as a card in the thread, with an Add button. Nothing is written until the user taps Add. Returns at once with the card id; the answer reaches you later as a note.",
		Args: []toolArg{{Name: "title", Type: "string", Doc: "What to remember.", Required: true},
			{Name: "due", Type: "string", Doc: "RFC 3339, or a date YYYY-MM-DD."},
			{Name: "notes", Type: "string", Doc: "Notes."}},
		run: personalRun("personal.reminder_add", nil)},
}

// personalRules are the manual's rules for the personal tools (capcom-service.md 8.2).
const personalRules = `Personal tools (Capcom only):
- Call device_location only when the question depends on where the user is. A question with no place in it needs no location.
- When the fix is older than the tool says, tell the user how old it is.
- Weather, places and the user's calendar are read tools. calendar_add and reminder_add only offer a card: the user taps Add, and you learn the answer later as a note.`

// personalRun calls one personal method with the tool's arguments; typed names the arguments
// that are not strings.
func personalRun(method string, typed map[string]string) func(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	return func(ctx context.Context, c caller, a toolArgs) (any, string, error) {
		params := map[string]any{}
		for k := range a {
			switch typed[k] {
			case "number":
				if f, ok := a.float(k); ok {
					params[k] = f
				}
			case "boolean":
				params[k] = a.boolean(k)
			default:
				if s := strings.TrimSpace(a.str(k)); s != "" {
					params[k] = s
				}
			}
		}
		var res personalResult
		if err := c.Call(ctx, method, params, &res); err != nil {
			return nil, "", err
		}
		return res, res.Text, nil
	}
}
