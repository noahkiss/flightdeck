package api

// --- Device context and Capcom's personal tools (capcom-service.md 8) ---

// DeviceLocation is one location fix from the phone (capcom-service.md 8.1).
type DeviceLocation struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Accuracy float64 `json:"accuracy" doc:"Horizontal accuracy in metres."`
	Time     string  `json:"time" doc:"When the phone took the fix, RFC 3339."`
	Place    string  `json:"place,omitempty" doc:"The place name from the phone's reverse geocoder, such as a neighbourhood and city."`
	Country  string  `json:"country,omitempty" doc:"The ISO 3166-1 alpha-2 country code from the reverse geocoder: weather alerts need it."`
}

// DeviceEvent is one calendar event in a device's snapshot (capcom-service.md 8.1).
type DeviceEvent struct {
	Title    string `json:"title"`
	Start    string `json:"start" doc:"RFC 3339."`
	End      string `json:"end" doc:"RFC 3339."`
	AllDay   bool   `json:"all_day,omitempty"`
	Location string `json:"location,omitempty"`
	Calendar string `json:"calendar,omitempty" doc:"The calendar's title."`
}

// DeviceCalendar is a device's calendar snapshot: the next capcom.calendar_days days of events
// from the calendars the user picked.
type DeviceCalendar struct {
	Time   string        `json:"time" doc:"When the phone read the calendars, RFC 3339."`
	From   string        `json:"from" doc:"The start of the window the snapshot covers, RFC 3339."`
	To     string        `json:"to" doc:"The end of the window the snapshot covers, RFC 3339."`
	Events []DeviceEvent `json:"events"`
}

// DeviceContextPutParams is the latest device context from one device (capcom-service.md 8.1).
// A field left out keeps the value the hab holds; a reason in location_off or calendar_off
// drops the held value.
type DeviceContextPutParams struct {
	TZ          string          `json:"tz,omitempty" doc:"The device's IANA time zone, such as Europe/Berlin."`
	Location    *DeviceLocation `json:"location,omitempty"`
	LocationOff string          `json:"location_off,omitempty" doc:"Why the device shares no location: off (the capcom.share_location preference) or no_permission. The hab drops the fix this device sent."`
	Calendar    *DeviceCalendar `json:"calendar,omitempty"`
	CalendarOff string          `json:"calendar_off,omitempty" doc:"Why the device shares no calendar: off or no_permission. The hab drops the snapshot this device sent."`
}

// CapcomCardEvent is what a calendar or reminder card would add (capcom-service.md 8.5).
type CapcomCardEvent struct {
	Title    string `json:"title"`
	Start    string `json:"start,omitempty" doc:"calendar: RFC 3339, or a date (YYYY-MM-DD) for an all-day event."`
	End      string `json:"end,omitempty" doc:"calendar: RFC 3339, or a date (YYYY-MM-DD) for an all-day event, the last day included."`
	AllDay   bool   `json:"all_day,omitempty"`
	Due      string `json:"due,omitempty" doc:"reminder: RFC 3339, or a date (YYYY-MM-DD)."`
	Location string `json:"location,omitempty"`
	Notes    string `json:"notes,omitempty"`
	URL      string `json:"url,omitempty"`
	TZ       string `json:"tz,omitempty" doc:"The time zone the times are meant in, from the device context; absent: the times carry their offset."`
}

// CapcomAttribution is a data source's mark that must show with a reply that used it
// (capcom-service.md 8.3).
type CapcomAttribution struct {
	Source string `json:"source" doc:"weather: Apple Weather."`
	Text   string `json:"text" doc:"The mark's text, such as Apple Weather."`
	URL    string `json:"url" doc:"The data sources page the mark links to."`
}
