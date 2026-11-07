package personal

import (
	"fmt"
	"strings"
	"time"
)

// DefaultLocationStale is [capcom] location_stale (19.2): a fix older than this is named as
// old.
const DefaultLocationStale = 30 * time.Minute

// Age says how old a time is, in plain words.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

func durText(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", max(int(d/time.Minute), 1))
	}
	return fmt.Sprintf("%g hours", d.Hours())
}

// LocationText is device_location's answer (8.2): the place name, coordinates, accuracy, the
// fix's age and the device; or why there is none.
func LocationText(f Fix, why string, now time.Time, stale time.Duration) string {
	if why != "" {
		switch why {
		case ReasonOff:
			return "No location shared: location sharing is off in the Flightdeck app (capcom.share_location)."
		case ReasonNoPermission:
			return "No location shared: the Flightdeck app has no location permission."
		}
		return "No location shared: " + why + "."
	}
	place := f.Place
	if place == "" {
		place = "an unnamed place"
	}
	age := now.Sub(f.At)
	var b strings.Builder
	fmt.Fprintf(&b, "%s, at %.5f,%.5f within %.0f m, %s, from %s.", place, f.Lat, f.Lon, f.Accuracy, Age(age), f.Device)
	if f.Country != "" {
		fmt.Fprintf(&b, " Country %s.", f.Country)
	}
	if stale > 0 && age > stale {
		fmt.Fprintf(&b, " This fix is older than %s: say how old it is in your answer.", durText(stale))
	}
	return b.String()
}

// ToolLine is the tool line's text for device_location (8.1): the place name, never the
// coordinates.
func ToolLine(f Fix, why string) string {
	if why != "" {
		return "no location shared"
	}
	if f.Place == "" {
		return "an unnamed place"
	}
	return f.Place
}

// CalendarText is calendar_read's answer: the snapshot's events between from and to whose
// title, location or calendar holds query, any case.
func CalendarText(c Calendar, why string, from, to time.Time, query string, now time.Time, loc *time.Location) string {
	if why != "" {
		switch why {
		case ReasonOff:
			return "No calendar shared: calendar sharing is off in the Flightdeck app (capcom.share_calendar)."
		case ReasonNoPermission:
			return "No calendar shared: the Flightdeck app has no calendar permission."
		}
		return "No calendar shared: " + why + "."
	}
	if loc == nil {
		loc = time.UTC
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var b strings.Builder
	fmt.Fprintf(&b, "Calendar snapshot from %s, %s.\n", c.Device, Age(now.Sub(c.At)))
	if cf, err := time.Parse(time.RFC3339, c.To); err == nil && to.After(cf) {
		fmt.Fprintf(&b, "The snapshot covers events up to %s only.\n", cf.In(loc).Format("Mon 2 Jan 15:04"))
	}
	n := 0
	for _, e := range c.Events {
		s, err1 := time.Parse(time.RFC3339, e.Start)
		en, err2 := time.Parse(time.RFC3339, e.End)
		if err1 != nil || err2 != nil || !en.After(from) || !s.Before(to) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Title+" "+e.Location+" "+e.Calendar), q) {
			continue
		}
		n++
		if e.AllDay {
			fmt.Fprintf(&b, "- %s all day: %s", s.In(loc).Format("Mon 2 Jan"), e.Title)
		} else {
			fmt.Fprintf(&b, "- %s to %s: %s", s.In(loc).Format("Mon 2 Jan 15:04"), en.In(loc).Format("15:04"), e.Title)
		}
		if e.Location != "" {
			fmt.Fprintf(&b, " at %s", e.Location)
		}
		if e.Calendar != "" {
			fmt.Fprintf(&b, " [%s]", e.Calendar)
		}
		b.WriteString("\n")
	}
	if n == 0 {
		b.WriteString("No events in that window.\n")
	}
	return b.String()
}
