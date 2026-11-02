package pairing

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"rsc.io/qr"
)

func TestLink(t *testing.T) {
	tok := make([]byte, TokenSize)
	for i := range tok {
		tok[i] = byte(i * 17)
	}
	o := Offer{DeviceID: "SHA256:2xvoC5tcGEUi7zeRyp4eoa96OYm8wGB1bhw8Q7elVEc", Endpoints: []string{"192.0.2.10:47213", "[fd7a::1]:47213"}, Token: tok, Label: "dev box&co"}
	l, err := o.Link()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(l)
	if err != nil || u.Scheme != "flightdeck" || u.Host != "pair" {
		t.Fatalf("link %s: %v", l, err)
	}
	q := u.Query()
	if q.Get("v") != "1" || q.Get("id") != o.DeviceID || q.Get("l") != "dev box&co" || len(q["ep"]) != 2 || q["ep"][1] != "[fd7a::1]:47213" {
		t.Fatalf("query %v", q)
	}
	if got, err := DecodeToken(q.Get("t")); err != nil || string(got) != string(tok) || len(q.Get("t")) != 22 {
		t.Fatalf("token %q: %v", q.Get("t"), err)
	}
	for _, bad := range []Offer{
		{DeviceID: "MD5:x", Endpoints: o.Endpoints, Token: tok},
		{DeviceID: o.DeviceID, Token: tok},
		{DeviceID: o.DeviceID, Endpoints: make([]string, 9), Token: tok},
		{DeviceID: o.DeviceID, Endpoints: o.Endpoints, Token: tok[:8]},
	} {
		if _, err := bad.Link(); err == nil {
			t.Errorf("Link accepted %+v", bad)
		}
	}
}

func TestQualifies(t *testing.T) {
	for a, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "172.32.0.1": false, "192.168.1.5": true,
		"100.64.0.1": true, "100.127.255.254": true, "100.128.0.1": false,
		"fd7a:115c:a1e0::1": true, "fc00::1": true, "2001:db8::1": false,
		"127.0.0.1": false, "::1": false, "0.0.0.0": false, "::": false,
		"169.254.1.1": false, "fe80::1": false, "224.0.0.1": false, "8.8.8.8": false,
		"::ffff:10.0.0.1": true,
	} {
		if got := Qualifies(netip.MustParseAddr(a)); got != want {
			t.Errorf("Qualifies(%s) = %v", a, got)
		}
	}
	for a, ok := range map[string]bool{"127.0.0.1": true, "::1": true, "10.0.0.1": true, "0.0.0.0": false, "::": false, "8.8.8.8": false} {
		if err := CheckListen(netip.MustParseAddr(a)); (err == nil) != ok {
			t.Errorf("CheckListen(%s): %v", a, err)
		}
	}
	var many []netip.Addr
	for i := range 12 {
		many = append(many, netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}))
	}
	got := Pick(append([]netip.Addr{netip.MustParseAddr("fd00::5"), netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("10.0.0.0")}, many...))
	if len(got) != MaxEndpoints || got[0] != netip.MustParseAddr("10.0.0.0") {
		t.Fatalf("Pick: %v", got)
	}
	for _, a := range got {
		if !Qualifies(a) {
			t.Fatalf("Pick kept %s", a)
		}
	}
}

// TestQRMatchesTheCode: the half-block rendering holds exactly the code's modules, with the
// quiet zone around them.
func TestQRMatchesTheCode(t *testing.T) {
	text := "flightdeck://pair?v=1&id=SHA256%3A2xvoC5tcGEUi7zeRyp4eoa96OYm8wGB1bhw8Q7elVEc&ep=192.0.2.10%3A47213&t=AAAAAAAAAAAAAAAAAAAAAA&l=devbox"
	out, err := QR(text)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := qr.Encode(text, qr.M)
	n := c.Size + 2*quiet
	rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(rows) != (n+1)/2 {
		t.Fatalf("%d rows for %d modules", len(rows), n)
	}
	for ry, row := range rows {
		row = strings.TrimPrefix(strings.TrimSuffix(row, "\x1b[0m"), "\x1b[30;107m")
		if utf8.RuneCountInString(row) != n {
			t.Fatalf("row %d is %d wide, want %d", ry, utf8.RuneCountInString(row), n)
		}
		x := 0
		for _, r := range row {
			top := r == '█' || r == '▀'
			bottom := r == '█' || r == '▄'
			for dy, got := range []bool{top, bottom} {
				y := 2*ry + dy
				want := c.Black(x-quiet, y-quiet)
				if y >= n {
					want = false
				}
				if got != want {
					t.Fatalf("module %d,%d is %v", x, y, got)
				}
			}
			x++
		}
	}
}

func TestParseRequest(t *testing.T) {
	tok := EncodeToken(make([]byte, TokenSize))
	r, reason, err := ParseRequest([]byte(`{"v":1,"token":"` + tok + `","name":" Owner's  iPad ","kind":"tablet","extra":1}`))
	if err != nil || reason != "" || r.Name != "Owner's iPad" || r.Kind != KindTablet || len(r.Token) != TokenSize {
		t.Fatalf("%+v %q %v", r, reason, err)
	}
	r, _, _ = ParseRequest([]byte(`{"v":1,"token":"` + tok + `","name":"x","kind":"toaster"}`))
	if r.Kind != KindPhone {
		t.Fatalf("kind %q", r.Kind)
	}
	for line, want := range map[string]string{
		``:                                   ReasonRequest,
		`[]`:                                 ReasonRequest,
		`{} {}`:                              ReasonRequest,
		`{"v":"1"}`:                          ReasonRequest,
		`{"token":"` + tok + `","name":"x"}`: ReasonRequest,
		`{"v":3,"token":"` + tok + `"}`:      ReasonVersion,
		`{"v":1,"name":"x"}`:                 ReasonRequest,
		`{"v":1,"token":"abc","name":"x"}`:   ReasonToken,
		`{"v":1,"token":"` + tok + `"}`:      ReasonRequest,
		`{"v":1,"token":"` + tok + `","name":""}`: ReasonRequest,
	} {
		if _, reason, err := ParseRequest([]byte(line)); err == nil || reason != want {
			t.Errorf("%q: reason %q, err %v; want %s", line, reason, err, want)
		}
	}
}

// FuzzParseRequest: ParseRequest reads bytes from any client before the token check. It must
// not panic, and what it accepts must be clean.
func FuzzParseRequest(f *testing.F) {
	tok := EncodeToken(make([]byte, TokenSize))
	f.Add([]byte(`{"v":1,"token":"` + tok + `","name":"phone","kind":"phone"}`))
	f.Add([]byte(`{"v":1,"token":"` + tok + `","name":"‮\u0000x"}`))
	f.Add([]byte(`{"v":2}`))
	f.Add([]byte("\xff\xfe{"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, reason, err := ParseRequest(b)
		if err != nil {
			if reason == "" {
				t.Fatal("an error with no reason")
			}
			return
		}
		if len(r.Token) != TokenSize || r.Name == "" || utf8.RuneCountInString(r.Name) > 64 || !utf8.ValidString(r.Name) {
			t.Fatalf("accepted %+v", r)
		}
		for _, c := range r.Name {
			if c < 0x20 || c == 0x7f || c == 0x202e {
				t.Fatalf("name keeps %U", c)
			}
		}
		if r.Kind != KindPhone && r.Kind != KindTablet && r.Kind != KindDesktop {
			t.Fatalf("kind %q", r.Kind)
		}
	})
}
