package rounds

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func values(set uint64) []int {
	var out []int
	for v := bitsFrom(set, 0); v >= 0; v = bitsFrom(set, v+1) {
		out = append(out, v)
	}
	return out
}

func TestParseCron(t *testing.T) {
	tests := []struct {
		expr                          string
		minute, hour, dom, month, dow []int
	}{
		{"* * * * *", nil, nil, nil, nil, nil},
		{"0 9 * * MON-FRI", []int{0}, []int{9}, nil, nil, []int{1, 2, 3, 4, 5}},
		{"*/15 */6 1,15 JAN,jul *", []int{0, 15, 30, 45}, []int{0, 6, 12, 18}, []int{1, 15}, []int{1, 7}, nil},
		{"5-20/5 0 * * sun", []int{5, 10, 15, 20}, []int{0}, nil, nil, []int{0}},
		{"*/59 5-5 * * *", []int{0, 59}, []int{5}, nil, nil, nil}, // a step as wide as the field; a one-value range
		{"30/10 23 31 dec 7", []int{30, 40, 50}, []int{23}, []int{31}, []int{12}, []int{0}},
		{"  0   0  1 *  0,6 ", []int{0}, []int{0}, []int{1}, nil, []int{0, 6}},
	}
	for _, tt := range tests {
		c, err := ParseCron(tt.expr)
		if err != nil {
			t.Fatalf("%q: %v", tt.expr, err)
		}
		check := func(name string, got uint64, want []int, full []int) {
			if want == nil {
				want = full
			}
			if g := values(got); !reflect.DeepEqual(g, want) {
				t.Errorf("%q %s = %v, want %v", tt.expr, name, g, want)
			}
		}
		check("minute", c.minute, tt.minute, seq(0, 59))
		check("hour", c.hour, tt.hour, seq(0, 23))
		check("dom", c.dom, tt.dom, seq(1, 31))
		check("month", c.mon, tt.month, seq(1, 12))
		check("dow", c.dow, tt.dow, seq(0, 6))
	}
	if c, _ := ParseCron("  0   0  1 *  0,6 "); c.String() != "0 0 1 * 0,6" {
		t.Errorf("String = %q", c.String())
	}
}

func seq(a, b int) []int {
	var out []int
	for v := a; v <= b; v++ {
		out = append(out, v)
	}
	return out
}

func TestParseCronRefused(t *testing.T) {
	tests := []struct{ expr, want string }{
		{"", "want 5 fields"},
		{"* * * *", "want 5 fields"},
		{"0 * * * * *", "want 5 fields"}, // seconds
		{"@reboot", "@ forms"},
		{"@daily", "@ forms"},
		{"60 * * * *", "outside 0-59"},
		{"* 24 * * *", "outside 0-23"},
		{"* * 0 * *", "outside 1-31"},
		{"* * * 13 *", "outside 1-12"},
		{"* * * * 8", "outside 0-7"},
		{"*/0 * * * *", "bad step"},
		{"*/60 * * * *", "larger than the field"},
		{"10-5 * * * *", "runs backwards"},
		{"* * * * FRI-MON", "runs backwards"},
		{"* * * FOO *", "bad value"},
		{"* * * * MON#2", "bad value"},
		{"* * L * *", "bad value"},
		{"* * ? * *", "bad value"},
		{"1,,2 * * * *", "empty list item"},
		{"+5 * * * *", "bad value"},
		{"*/+5 * * * *", "bad step"},
		{"-5 * * * *", "bad value"},
		{"* * * MON *", "bad value"}, // a day name in the month field
	}
	for _, tt := range tests {
		_, err := ParseCron(tt.expr)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.expr, err, tt.want)
		}
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadZone(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// TestCronNext covers the slot rules, the day-field rule and the DST rules of rounds.md 4.1.
func TestCronNext(t *testing.T) {
	tests := []struct {
		name  string
		expr  string
		zone  string
		after string
		want  []string // successive slots, UTC
	}{
		{"every minute", "* * * * *", "UTC", "2026-05-01T10:00:30Z",
			[]string{"2026-05-01T10:01:00Z", "2026-05-01T10:02:00Z"}},
		{"strictly after", "0 9 * * *", "UTC", "2026-05-01T09:00:00Z",
			[]string{"2026-05-02T09:00:00Z"}},
		{"weekdays in a zone", "0 9 * * MON-FRI", "Europe/Paris", "2026-05-01T12:00:00Z", // a Friday
			[]string{"2026-05-04T07:00:00Z", "2026-05-05T07:00:00Z"}},
		{"both day fields restricted: either matches", "0 0 13 * FRI", "UTC", "2026-02-01T00:00:00Z",
			[]string{"2026-02-06T00:00:00Z", "2026-02-13T00:00:00Z", "2026-02-20T00:00:00Z", "2026-02-27T00:00:00Z", "2026-03-06T00:00:00Z"}},
		{"star-led day field: both must match", "0 0 */13 * FRI", "UTC", "2026-01-01T00:00:00Z",
			[]string{"2026-02-27T00:00:00Z"}},
		{"leap day", "0 0 29 2 *", "UTC", "2026-01-01T00:00:00Z",
			[]string{"2028-02-29T00:00:00Z", "2032-02-29T00:00:00Z"}},
		{"day 7 is Sunday", "0 0 * * 7", "UTC", "2026-05-01T00:00:00Z",
			[]string{"2026-05-03T00:00:00Z"}},
		// 29 February on a Sunday: 2100 is not a leap year, so the gap after 2096 is 32 years.
		{"leap day on one weekday across a skipped leap year", "0 0 29 2 */7", "UTC", "2097-01-01T00:00:00Z",
			[]string{"2128-02-29T00:00:00Z"}},

		// Spring forward in New York: 2026-03-08, 02:00 EST becomes 03:00 EDT.
		{"spring gap runs at the first valid minute", "30 2 * * *", "America/New_York", "2026-03-07T17:00:00Z",
			[]string{"2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z"}},
		{"gap slots collapse into one", "*/15 2 * * *", "America/New_York", "2026-03-07T17:00:00Z",
			[]string{"2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z"}},
		{"hourly across the gap", "0 * * * *", "America/New_York", "2026-03-08T06:30:00Z",
			[]string{"2026-03-08T07:00:00Z", "2026-03-08T08:00:00Z"}},
		{"London spring gap", "30 1 * * *", "Europe/London", "2026-03-28T12:00:00Z",
			[]string{"2026-03-29T01:00:00Z", "2026-03-30T00:30:00Z"}},

		// Fall back in New York: 2026-11-01, 02:00 EDT becomes 01:00 EST.
		{"autumn repeat runs once, first occurrence", "30 1 * * *", "America/New_York", "2026-10-31T16:00:00Z",
			[]string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
		{"from inside the second occurrence", "30 1 * * *", "America/New_York", "2026-11-01T06:10:00Z",
			[]string{"2026-11-02T06:30:00Z"}},
		{"hourly skips the repeated hour", "0 * * * *", "America/New_York", "2026-11-01T04:30:00Z",
			[]string{"2026-11-01T05:00:00Z", "2026-11-01T07:00:00Z", "2026-11-01T08:00:00Z"}},

		// Fall back in Auckland, 12 hours from UTC: 2024-04-07, 03:00 NZDT becomes 02:00 NZST.
		{"autumn repeat far from UTC, first occurrence", "30 2 * * *", "Pacific/Auckland", "2024-04-06T00:00:00Z",
			[]string{"2024-04-06T13:30:00Z", "2024-04-07T14:30:00Z"}},

		// Samoa skipped 2011-12-30 entirely.
		{"a skipped day runs at the first minute after it", "0 12 30 12 *", "Pacific/Apia", "2011-12-29T00:00:00Z",
			[]string{"2011-12-30T10:00:00Z", "2012-12-29T22:00:00Z"}},
		{"a midnight slot on the skipped day bridges 24 hours", "0 0 30 12 *", "Pacific/Apia", "2011-12-29T00:00:00Z",
			[]string{"2011-12-30T10:00:00Z", "2012-12-29T10:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Trigger{Kind: TriggerCron, Cron: tt.expr, Zone: tt.zone}.Schedule()
			if err != nil {
				t.Fatal(err)
			}
			got := s.NextN(utc(tt.after), len(tt.want))
			var gs []string
			for _, g := range got {
				if g.Location() != time.UTC {
					t.Errorf("slot %v is not UTC", g)
				}
				gs = append(gs, g.Format(time.RFC3339))
			}
			if !reflect.DeepEqual(gs, tt.want) {
				t.Errorf("slots = %v, want %v", gs, tt.want)
			}
		})
	}
}

func TestInstants(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	tests := []struct {
		civil string
		want  []string
	}{
		{"2026-03-08T02:30:00Z", nil},
		{"2026-03-08T03:00:00Z", []string{"2026-03-08T07:00:00Z"}},
		{"2026-11-01T01:30:00Z", []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"}},
		{"2026-06-01T12:00:00Z", []string{"2026-06-01T16:00:00Z"}},
	}
	for _, tt := range tests {
		var gs []string
		for _, g := range instants(utc(tt.civil), ny) {
			gs = append(gs, g.UTC().Format(time.RFC3339))
		}
		if !reflect.DeepEqual(gs, tt.want) {
			t.Errorf("%s: %v, want %v", tt.civil, gs, tt.want)
		}
	}
}

func TestScheduleRefused(t *testing.T) {
	tests := []struct {
		trig Trigger
		want string
	}{
		{Trigger{Kind: TriggerCron, Cron: "0 0 31 2 *", Zone: "UTC"}, "never runs"},
		{Trigger{Kind: TriggerCron, Cron: "0 0 30 2 *", Zone: "UTC"}, "never runs"},
		{Trigger{Kind: TriggerCron, Cron: "0 0 31 4,6,9,11 *", Zone: "UTC"}, "never runs"},
		{Trigger{Kind: TriggerCron, Cron: "0 0 * * *", Zone: "Local"}, "IANA zone"},
		{Trigger{Kind: TriggerCron, Cron: "0 0 * * *", Zone: "Mars/Olympus"}, "Mars/Olympus"},
		{Trigger{Kind: TriggerCron, Cron: "0 0 * * *"}, "IANA zone"},
		{Trigger{Kind: TriggerOnce}, "no instant"},
		{Trigger{Kind: TriggerEvent}, "not supported yet"},
		{Trigger{Kind: "hourly"}, "unknown trigger"},
	}
	for _, tt := range tests {
		_, err := tt.trig.Schedule()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v: err = %v, want %q", tt.trig, err, tt.want)
		}
	}
	_, err := Trigger{Kind: TriggerCron, Cron: "0 0 31 2 *", Zone: "UTC"}.Schedule()
	if !errors.Is(err, ErrNeverRuns) {
		t.Errorf("err = %v, want ErrNeverRuns", err)
	}
	// Runs, though rarely: either day field matches when both are restricted.
	if _, err := (Trigger{Kind: TriggerCron, Cron: "0 0 31 2 MON", Zone: "UTC"}).Schedule(); err != nil {
		t.Errorf("0 0 31 2 MON: %v", err)
	}
}

func TestOnceAndManual(t *testing.T) {
	at := utc("2026-12-01T09:00:00Z")
	s, err := Trigger{Kind: TriggerOnce, At: &at, Zone: "UTC"}.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Next(at.Add(-time.Second)); !ok || !got.Equal(at) {
		t.Errorf("Next before = %v %v", got, ok)
	}
	if _, ok := s.Next(at); ok {
		t.Error("a once slot ran twice")
	}
	if n := s.NextN(time.Time{}, 3); len(n) != 1 {
		t.Errorf("NextN = %v", n)
	}
	m, _ := Trigger{Kind: TriggerManual}.Schedule()
	if _, ok := m.Next(time.Now()); ok {
		t.Error("a manual trigger has a slot")
	}
}

func TestPresetCron(t *testing.T) {
	ok := map[string]string{
		"1m":    "* * * * *",
		"5m":    "*/5 * * * *",
		"30m":   "*/30 * * * *",
		"60m":   "0 * * * *",
		"1h":    "0 * * * *",
		"6h":    "0 */6 * * *",
		"1440m": "0 0 * * *",
		"24h":   "0 0 * * *",
		"1d":    "0 0 * * *",
	}
	for in, want := range ok {
		got, err := PresetCron(in)
		if err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", in, got, err, want)
		}
		if _, err := ParseCron(got); err != nil {
			t.Errorf("%s compiles to a bad cron: %v", in, err)
		}
	}
	refused := map[string]string{
		"7m":                "nearest presets: 5m, 10m",
		"45m":               "nearest presets: 30m, 1h",
		"5h":                "nearest presets: 4h, 6h",
		"2d":                "nearest presets: 1d",
		"1000d":             "nearest presets: 1d",
		"999h":              "nearest presets: 1d",
		"90s":               "want a number",
		"0m":                "want a number",
		"m":                 "want a number",
		"5":                 "want a number",
		"-5m":               "want a number",
		"+5m":               "want a number",
		" 5m":               "want a number",
		"1001d":             "want a number",
		"9999999999999999m": "want a number",
	}
	for in, want := range refused {
		_, err := PresetCron(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", in, err, want)
		}
	}
}

func TestHostZone(t *testing.T) {
	noLink := func() (string, error) { return "", errors.New("no link") }
	link := func(p string) func() (string, error) { return func() (string, error) { return p, nil } }
	tests := []struct {
		tz       string
		readlink func() (string, error)
		want     string
	}{
		{"Europe/Paris", noLink, "Europe/Paris"},
		{":America/Chicago", noLink, "America/Chicago"},
		{"", link("/usr/share/zoneinfo/Asia/Tokyo"), "Asia/Tokyo"},
		{"", link("/var/db/timezone/zoneinfo/America/New_York"), "America/New_York"},
		{"Not/AZone", link("../usr/share/zoneinfo/Europe/Berlin"), "Europe/Berlin"},
		{"", link("/etc/somewhere"), "UTC"},
		{"", link("./UTC"), "UTC"}, // the time package loads "./UTC"; it is no zone name
		{"./Asia/Tokyo", link("./Europe/Berlin"), "UTC"},
		{"", noLink, "UTC"},
	}
	for _, tt := range tests {
		if got := hostZone(tt.tz, tt.readlink); got != tt.want {
			t.Errorf("hostZone(%q) = %q, want %q", tt.tz, got, tt.want)
		}
	}
	if _, err := LoadZone(HostZone()); err != nil {
		t.Errorf("HostZone() does not load: %v", err)
	}
}
