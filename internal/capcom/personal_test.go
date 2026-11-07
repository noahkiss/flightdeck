package capcom

import (
	"strings"
	"testing"
	"time"
)

func TestServiceConfigPersonal(t *testing.T) {
	c, problems, err := LoadServiceConfig(writeConfig(t, `[capcom]
location_stale = "45m"
[capcom.maps]
key_command = ["print-key", "maps"]
key_id = "MK1"
team_id = "TEAM42"
[capcom.weather]
service_id = "com.example.weather"
cache = "0s"
[capcom.places]
google_key_command = ["print-key", "google"]
`))
	if err != nil || len(problems) != 0 {
		t.Fatalf("problems %v err %v", problems, err)
	}
	p := c.Personal
	if p.LocationStale != 45*time.Minute || p.Weather.Cache != 0 || !p.Maps.Configured() || !p.Weather.Configured() ||
		p.Weather.KeyID != "MK1" || strings.Join(p.GoogleKeyCommand, " ") != "print-key google" {
		t.Fatalf("personal %+v", p)
	}
	d, _, _ := LoadServiceConfig(writeConfig(t, "[capcom]\n"))
	if d.Personal.LocationStale != 30*time.Minute || d.Personal.Weather.Cache != 10*time.Minute || d.Personal.Weather.Configured() {
		t.Fatalf("defaults %+v", d.Personal)
	}
	_, problems, _ = LoadServiceConfig(writeConfig(t, `[capcom]
location_stale = "10s"
[capcom.weather]
cache = "48h"
key = "planted"
[capcom.places]
google_key = "planted"
other = 1
`))
	all := strings.Join(problems, "\n")
	for _, want := range []string{"location_stale", "weather cache", "weather key: the config file holds no key; set key_command",
		"places google_key: the config file holds no key; set google_key_command", `places unknown key "other"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no problem %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "planted") {
		t.Errorf("a problem names the value:\n%s", all)
	}
}
