package diagbundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/diaglog"
)

// The fixture's made-up identity and secrets. Every value is invented: example hosts,
// documentation and private ranges, fake keys.
const (
	fxHome     = "/home/user"
	fxHost     = "build-box.example.com"
	fxUser     = "jfixture"
	fxPeer     = "studio-peer"
	fxPeerHost = "studio.example.org"
	fxDevice   = "fakedev0001"
	fxProject  = "orchard-notes"
	fxDocIP    = "192.0.2.44"
	fxDocIP2   = "198.51.100.7"
	fxPrivIP   = "10.20.30.40"
	fxCGNAT    = "100.101.102.103"
	fxTail6    = "fd7a:115c:a1e0::77"
	fxDoc6     = "2001:db8::1234"
	fxTailName = "studio.fixture-tail.ts.net"
	fxOther    = "/home/other/secret-plans/list.txt"
	fxEmail    = "jfixture@example.net"
	fxURLUser  = "urluser"
	fxURLPass  = "urlpass99"
	fxQuery    = "qtoken77"
	fxPEMBody  = "FAKEPEMBODYLINEFORTHEFIXTUREONLY"
	fxSSH      = "AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFake"
	fxFinger   = "SHA256:FakeFingerprintFakeFingerprintFakeFinger1"
	fxOpRef    = "op:" + "//Fixture Box/fixture-item/field" // split, so no scan reads it as a reference
	fxToken    = "tok3n-value-xyz"
	fxBearer   = "bearervalue123"
	fxBlob     = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	fxStderr   = "only-in-hab-stderr-q8w7e6"
	fxEngine   = "only-in-engine-log-z1x2c3"
	fxOld      = "only-before-the-window-k4j5"
	fxNudge    = "a nudge text of the owner's own"
	fxRepo     = "github.com/acme/secret-repo"
	fxAlias    = "acme-work"
	fxRound    = "check-acme-invoices"
	fxLong     = "only-in-a-long-line-p0o9"
	// A pane id is a UUID: the blob rule keeps it (an id Flightdeck minted).
	fxPane = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
)

// pemBegin is a PEM block's first line, built so no secret scanner reads the fixture as a key.
func pemBegin(kind string) string { return "-----" + "BEGIN " + kind + " PRIVATE " + "KEY-----" }

// leaks are the strings no bundle file may hold, in any case.
var leaks = []string{fxHome, fxHost, "build-box", fxUser, fxPeer, fxPeerHost, fxDevice, fxProject, fxDocIP, fxDocIP2,
	fxPrivIP, fxCGNAT, fxTail6, fxDoc6, fxTailName, "/home/other", "secret-plans", fxEmail, fxURLUser, fxURLPass, fxQuery,
	fxPEMBody, fxSSH, fxFinger, "op://", "Fixture Box", fxToken, fxBearer, fxBlob, fxStderr, fxEngine, fxOld, fxNudge,
	"secret-repo", fxAlias, fxRound, fxLong}

func fxIdentity(state string) Identity {
	return Identity{Home: fxHome, State: state, Names: []string{fxHost, "build-box", fxUser, fxPeer, fxPeerHost, fxDevice, fxProject}}
}

// seedState writes a state folder that holds each kind of value a bundle must not carry.
func seedState(t *testing.T, now time.Time) (state, habToml, peersToml string) {
	t.Helper()
	root := t.TempDir()
	state = filepath.Join(root, "state")
	ts := func(d time.Duration) string { return now.Add(-d).UTC().Format("2006-01-02T15:04:05.000Z") }
	local := func(d time.Duration) string { return now.Add(-d).Local().Format("2006/01/02 15:04:05") }
	files := map[string]string{
		"logs/hab.log": strings.Join([]string{
			`{"t":"` + ts(72*time.Hour) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"` + fxOld + `"}`,
			`{"t":"` + ts(time.Hour) + `","lvl":"warn","msg":"pin failed","proc":"hab","epoch":"0011223344556677","comp":"peers","label":"` + fxPeer + `","err":"dial ` + fxPeerHost + ` at ` + fxDocIP + `:7300 failed"}`,
			`{"t":"` + ts(time.Hour) + `","lvl":"info","msg":"peer pinned","proc":"hab","comp":"peers","label":"` + fxPeer + `","device":"` + fxDevice + `"}`,
			`{"t":"` + ts(50*time.Minute) + `","lvl":"info","msg":"trust answered","proc":"hab","comp":"trust","path":"` + fxHome + `/develop/` + fxProject + `","pane":"` + fxPane + `","count":3}`,
			`{"t":"` + ts(40*time.Minute) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"host ` + fxHost + ` user ` + fxUser + ` at ` + fxPrivIP + ` and ` + fxCGNAT + ` and ` + fxTail6 + ` and ` + fxDoc6 + `"}`,
			`{"t":"` + ts(30*time.Minute) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"dial ` + fxTailName + ` for ` + fxOther + ` mail ` + fxEmail + `"}`,
			`{"t":"` + ts(20*time.Minute) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"fetch https://` + fxURLUser + `:` + fxURLPass + `@git.example.com/repo?access=` + fxQuery + `#frag"}`,
			`{"t":"` + ts(10*time.Minute) + `","lvl":"error","msg":"legacy","proc":"hab","comp":"legacy","text":"key ssh-ed25519 ` + fxSSH + ` fp ` + fxFinger + ` ref ` + fxOpRef + `"}`,
			`{"t":"` + ts(5*time.Minute) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"token=` + fxToken + ` Authorization: Bearer ` + fxBearer + ` digest ` + fxBlob + `"}`,
			`{"t":"` + ts(4*time.Minute) + `","lvl":"info","msg":"legacy","proc":"hab","comp":"legacy","text":"` + strings.Repeat("x", MaxLine) + fxLong + `"}`,
			`{"t":"` + ts(4*time.Minute) + `","lvl":"info","msg":"cut sh`, // a line a crash cut short
		}, "\n") + "\n",
		"logs/gff-hab-holder.one.log": "gff-hab-holder: " + local(time.Hour) + " pane " + fxPane + ": spawned in " + fxHome + "/develop/" + fxProject + "\n" +
			"goroutine 1 [running]:\n\t" + fxHome + "/go/src/main.go:12\n",
		// A holder's file named for its instance, which holds a known name (review L5).
		"logs/gff-hab-holder." + fxUser + "-box.log": "gff-hab-holder: " + local(time.Hour) + " started\n",
		"logs/hab.stderr":                "gff-hab: " + local(time.Hour) + " warn peers: pin failed label=" + fxPeer + " " + fxStderr + "\n",
		"capcom/engine.log":              "sdk: " + fxEngine + "\n",
		"crash/hab-0011223344556677.txt": "panic: link to " + fxHost + " failed\n" + pemBegin("OPENSSH") + "\n" + fxPEMBody + "\n" + fxPEMBody + "\n-----END OPENSSH PRIVATE KEY-----\ngoroutine 7\n",
		"health.jsonl": `{"t":"` + ts(time.Hour) + `","proc":"hab","kind":"crash","cause":"panic: dial ` + fxDocIP2 + `","run":"0011223344556677","crash_file":"hab-0011223344556677.txt"}` + "\nnot json\n" +
			strings.Repeat("y", MaxLine+10) + fxLong + "\n",
		"tallies/" + now.Local().Format("2006-01-02") + ".json": `{"calls":{"session.list":{"app":3},"` + fxHost + `":{"app":1}}}` + "\n",
		"tallies/seen.json": `{"since":"2026-11-01"}` + "\n",
		"friction/20261127T120000.000Z-hABCDEFGHJKM.json": `{"t":"` + ts(15*time.Minute) + `","trace":"hABCDEFGHJKM","client":"app","text":"went back: mail ` + fxEmail + ` about ` + fxProject + `","log":[` +
			`{"t":"` + ts(16*time.Minute) + `","proc":"hab","text":"{\"t\":\"` + ts(16*time.Minute) + `\",\"lvl\":\"info\",\"msg\":\"peer pinned\",\"proc\":\"hab\",\"comp\":\"peers\",\"label\":\"` + fxPeer + `\"}"},` +
			`{"t":"` + ts(16*time.Minute) + `","proc":"holder one","text":"pane spawned in ` + fxHome + `/develop"}]}`,
		// A note whose trace is not in its form must not name a file outside the bundle (L9).
		"friction/20261127T120001.000Z-hABCDEFGHJKN.json": `{"t":"` + ts(14*time.Minute) + `","trace":"../../../../escape","client":"app","text":"x"}`,
	}
	for rel, data := range files {
		p := filepath.Join(state, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	habToml = filepath.Join(root, "hab.toml")
	peersToml = filepath.Join(root, "peers.toml")
	if err := os.WriteFile(habToml, []byte(`[settings]
"agent.start_mode" = "bypass"
"heavy.slots" = 2
"agent.context_nudge_text" = "`+fxNudge+`"

[aliases]
`+fxAlias+` = "claude"

[logbook.habits_off]
"`+fxRepo+`" = ["closeout"]

[rounds.`+fxRound+`]
mode = "fresh"

[catalog]
project_roots = ["`+fxHome+`/develop"]

[[catalog.services]]
name = "`+fxPeer+`"
url = "https://`+fxPeerHost+`/?token=`+fxToken+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peersToml, []byte(`[[peers]]
label = "`+fxPeer+`"
endpoints = ["`+fxPeerHost+`:7300", "`+fxTailName+`"]
device_id = "`+fxDevice+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return state, habToml, peersToml
}

func build(t *testing.T, r *Redactor, state, habToml, peersToml string, now time.Time) (string, Manifest) {
	t.Helper()
	dir := t.TempDir()
	m, err := Write(dir, r, Options{State: state, Since: now.Add(-48 * time.Hour), Until: now, HabConfig: habToml, Peers: peersToml,
		Extras: []Extra{
			{Path: "status.txt", Data: []byte("gff-hab  stopped, logs " + fxHome + "/state/logs\n"), Kind: KindText},
			{Path: "detect.json", Data: []byte(`[{"pane":"` + fxPane + `","manifest":{"path":"` + fxHome + `/m.toml"}}]`), Kind: KindJSON},
			{Path: "versions.json", Data: []byte(`{"go":"go1.27.4"}` + "\n"), Kind: KindKept},
		}})
	if err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// readAll is every file of dir, by its path in the bundle.
func readAll(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if !e.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

var tokenRE = regexp.MustCompile(`n:[0-9a-f]{8}`)

// TestBundleRedaction is diagnostics test 12: a state folder seeded with host names, paths,
// IPs, keys and tokens yields a bundle with none of them; the manifest counts each rule; a value
// hashes alike within one bundle and differently across two. hab.stderr and engine.log stay out
// (8.2, review L4).
func TestBundleRedaction(t *testing.T) {
	now := time.Now()
	state, habToml, peersToml := seedState(t, now)
	dir, m := build(t, New(fxIdentity(state)), state, habToml, peersToml, now)
	files := readAll(t, dir)

	for path, data := range files {
		if testing.Verbose() {
			t.Logf("%s:\n%s", path, data)
		}
		low := strings.ToLower(data + "\n" + path)
		for _, s := range leaks {
			if strings.Contains(low, strings.ToLower(s)) {
				t.Errorf("%s holds %q:\n%s", path, s, data)
			}
		}
	}
	for _, rule := range Rules {
		if m.Replacements[rule] == 0 {
			t.Errorf("rule %s: no replacement counted: %v", rule, m.Replacements)
		}
	}
	if m.Unparsed != 2 { // the cut hab.log line and the health line that is not JSON
		t.Errorf("unparsed %d, want 2", m.Unparsed)
	}
	if m.Long != 2 { // the long hab.log line and the long health line (review L8)
		t.Errorf("long lines %d, want 2", m.Long)
	}
	for path := range files {
		if strings.Contains(path, "escape") {
			t.Errorf("a friction note's trace named the file %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.json")); err == nil {
		t.Error("a friction note's trace named a file outside the bundle")
	}

	// The files of 8.2 only, and each one the manifest lists, at its size.
	want := []string{"config/hab.toml", "config/peers.toml", "crash/hab-0011223344556677.txt", "detect.json",
		"friction/", "health.jsonl", "tallies/" + now.Local().Format("2006-01-02") + ".json", "logs/gff-hab-holder.one.log", "logs/hab.log", "manifest.json", "status.txt",
		"tallies/seen.json", "versions.json"}
	listed := map[string]int64{}
	for _, f := range m.Files {
		listed[f.Path] = f.Size
	}
	for path, data := range files {
		if path == ManifestFile {
			continue
		}
		if size, ok := listed[path]; !ok || size != int64(len(data)) {
			t.Errorf("%s: listed %v at %d, size %d", path, ok, size, len(data))
		}
		if strings.HasPrefix(path, "logs/hab.stderr") || strings.Contains(path, "engine") {
			t.Errorf("%s is in the bundle", path)
		}
	}
	for _, w := range want {
		found := false
		for path := range files {
			found = found || path == w || (strings.HasSuffix(w, "/") && strings.HasPrefix(path, w))
		}
		if !found {
			t.Errorf("no %s in the bundle: %v", w, keys(files))
		}
	}

	// What stays readable: minted ids, constant messages, the fixed layout, plain settings.
	hab := files["logs/hab.log"]
	for _, s := range []string{fxPane, `"msg":"pin failed"`, `"count":3`, `"epoch":"0011223344556677"`} {
		if !strings.Contains(hab, s) {
			t.Errorf("hab.log lost %q:\n%s", s, hab)
		}
	}
	if !strings.Contains(files["status.txt"], "~/state/logs") {
		t.Errorf("status.txt: the home folder is not ~:\n%s", files["status.txt"])
	}
	cfg := files["config/hab.toml"]
	for _, s := range []string{`"agent.start_mode" = "bypass"`, `"heavy.slots" = 2`, `"agent.context_nudge_text" = "<value>"`, `project_roots = "<value>"`,
		"[aliases]", "[logbook.habits_off]", `mode = "<value>"`, `[rounds."n:`} {
		if !strings.Contains(cfg, s) {
			t.Errorf("hab.toml: no %s:\n%s", s, cfg)
		}
	}
	if !strings.Contains(files["config/peers.toml"], `label = "<value>"`) {
		t.Errorf("peers.toml:\n%s", files["config/peers.toml"])
	}
	if c := files["crash/hab-0011223344556677.txt"]; !strings.Contains(c, "<key>") || !strings.Contains(c, "goroutine 7") {
		t.Errorf("crash file:\n%s", c)
	}
	if !strings.Contains(hab, "<state>") && !strings.Contains(hab, `"path":"~/`) {
		t.Errorf("hab.log: the path is not mapped:\n%s", hab)
	}

	// One value, one token within a bundle: the peer's label in hab.log by key (pass 1) and in
	// the friction tail; another bundle, another salt.
	labelTok := regexp.MustCompile(`"label":"(n:[0-9a-f]{8})"`).FindAllStringSubmatch(hab, -1)
	if len(labelTok) != 2 || labelTok[0][1] != labelTok[1][1] {
		t.Fatalf("label tokens %v", labelTok)
	}
	var fric string
	for path, data := range files {
		if strings.HasPrefix(path, "friction/") && strings.HasSuffix(path, "-hABCDEFGHJKM.json") {
			fric = data
		}
	}
	if !strings.Contains(fric, labelTok[0][1]) {
		t.Errorf("the friction tail's label is not the same token %s:\n%s", labelTok[0][1], fric)
	}
	dir2, _ := build(t, New(fxIdentity(state)), state, habToml, peersToml, now)
	hab2 := readAll(t, dir2)["logs/hab.log"]
	if strings.Contains(hab2, labelTok[0][1]) || len(tokenRE.FindAllString(hab2, -1)) == 0 {
		t.Errorf("a second bundle reuses the token %s", labelTok[0][1])
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestArchive: the archive holds the folder's files under one top folder, with no user or group
// name, and the same bytes.
// TestBundleCapcomLog: the Capcom engine's capcom.log, in the form the engine writes it (the
// golden file its own tests check, capcom-engine/test/fixtures), parses as structured lines
// and goes through both passes (8.2, 8.3; 4.9). Its messages, error types and classes stay; a
// name or an address that reached a value goes. Its old file comes too.
func TestBundleCapcomLog(t *testing.T) {
	now := time.Now()
	golden, err := os.ReadFile(filepath.Join("..", "..", "capcom-engine", "test", "fixtures", "capcom.log.golden"))
	if err != nil {
		t.Fatal(err)
	}
	ts := now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(golden)), "\n") {
		p, ok := diaglog.Parse(l)
		if !ok || p.Proc != "capcom" || p.Msg == "" || p.Comp == "" {
			t.Fatalf("the golden line does not parse: %s", l)
		}
		for _, kv := range p.Attrs {
			if kv.Num && strings.Trim(kv.Val, "0123456789-") != "" {
				t.Errorf("%s: a value that is neither a string nor an integer: %s", kv.Key, kv.Val)
			}
		}
		lines = append(lines, `{"t":"`+ts+`"`+l[strings.Index(l, `,"lvl"`):])
	}
	// What an engine line could carry by mistake: a name and an address in a word's value, an
	// email in text.
	lines = append(lines, `{"t":"`+ts+`","lvl":"warn","msg":"set model failed","proc":"capcom","comp":"engine","model_text":"`+fxHost+` at `+fxDocIP+`","text":"mail `+fxEmail+`"}`)
	state := filepath.Join(t.TempDir(), "state")
	os.MkdirAll(filepath.Join(state, "logs"), 0o700)
	os.WriteFile(filepath.Join(state, "logs", "capcom.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	os.WriteFile(filepath.Join(state, "logs", "capcom.log.1"), []byte(lines[0]+"\n"), 0o600)
	dir := t.TempDir()
	if _, err := Write(dir, New(fxIdentity(state)), Options{State: state, Since: now.Add(-48 * time.Hour), Until: now}); err != nil {
		t.Fatal(err)
	}
	files := readAll(t, dir)
	got := files["logs/capcom.log"]
	if files["logs/capcom.log.1"] == "" {
		t.Fatal("the old capcom.log is not in the bundle")
	}
	for _, s := range []string{`"msg":"session started"`, `"session":"sess-0a1b"`, `"msg":"sdk stderr"`, `"lines":4`, `"class":"rate_limit"`,
		`"err_type":"TypeError"`, `"code":"ECONNREFUSED"`, `"entry":"auth.json"`, `"model":"claude-opus-4-1"`} {
		if !strings.Contains(got, s) {
			t.Errorf("the bundle's capcom.log lacks %s:\n%s", s, got)
		}
	}
	for _, s := range []string{fxHost, "build-box", fxDocIP, fxEmail} {
		if strings.Contains(got, s) {
			t.Errorf("the bundle's capcom.log holds %q:\n%s", s, got)
		}
	}
	if n := strings.Count(got, "\n"); n != len(lines) {
		t.Errorf("%d lines, want %d:\n%s", n, len(lines), got)
	}
}

func TestArchive(t *testing.T) {
	now := time.Now()
	state, habToml, peersToml := seedState(t, now)
	dir, _ := build(t, New(fxIdentity(state)), state, habToml, peersToml, now)
	dst := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := Archive(dst, dir, "flightdeck-diagnostics-x"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("archive: %v", err)
	}
	f, _ := os.Open(dst)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := readAll(t, dir)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uname != "" || h.Gname != "" || !strings.HasPrefix(h.Name, "flightdeck-diagnostics-x/") {
			t.Errorf("header %+v", h)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, _ := io.ReadAll(tr)
		if want := files[strings.TrimPrefix(h.Name, "flightdeck-diagnostics-x/")]; string(b) != want {
			t.Errorf("%s differs", h.Name)
		}
		n++
	}
	if n != len(files) {
		t.Errorf("%d files in the archive, %d in the folder", n, len(files))
	}
}

// TestScrubRules: each pass 2 rule on its own, and what it leaves alone.
func TestScrubRules(t *testing.T) {
	r := newWithSalt(Identity{Home: fxHome, Names: []string{fxHost, "ab"}}, []byte("salt"))
	tok := r.token(fxHost)
	for _, c := range []struct{ in, want string }{
		{"on " + strings.ToUpper(fxHost) + ".", "on " + tok + "."},
		{"xbuild-box.example.com", "xbuild-box.example.com"}, // not a word of its own
		{"ab cd", "ab cd"}, // shorter than MinName
		{fxHome + "/develop", "~/develop"},
		{"at " + fxPrivIP + ":22", "at <ip>:22"},
		{"v1.2.3", "v1.2.3"},
		{"at " + fxDoc6 + " and fe80::1%en0", "at <ip> and <ip>"},
		{"at 15:04:05.123 and 2026-11-27T10:00:00Z", "at 15:04:05.123 and 2026-11-27T10:00:00Z"},
		{"cd " + fxOther, "cd ~/<path>"},
		{"/srv/home/x", "/srv/home/x"},
		{"https://u:p@example.com/a?b=c#d end", "https://example.com/a end"},
		{"https://example.com/a", "https://example.com/a"},
		{"mail " + fxEmail, "mail <email>"},
		{pemBegin("RSA") + "\nAAA\n-----END RSA PRIVATE KEY----- after", "<key> after"},
		{"password: hunter2 and key=abc", "password: <secret> and key=<secret>"},
		{"no key here", "no key here"},
		{"pane " + fxPane, "pane " + fxPane},
		{"s-0123456789abcdef01234567", "s-0123456789abcdef01234567"},
		{"internal/diaglog/check/testdata/fixture/relay", "internal/diaglog/check/testdata/fixture/relay"},
		{"TestBundleRedactionKeepsLongWordsWithoutDigits", "TestBundleRedactionKeepsLongWordsWithoutDigits"},
		{"sum " + fxBlob, "sum <blob>"},
		// Control sequences go before both passes (review M6); a cut between two letters
		// leaves a space.
		{"\x1b[1m" + fxHost + "\x1b[0m failed", tok + " failed"},
		{"x\x1b[32m" + fxHost, "x " + tok},
		{"copy \x1b]52;c;aGVsbG8=\x07done \x1b]0;title\x1b\\x", "copy done x"},
		{"a\x00" + fxHost + "\rb\tc\nd\x7f", "a " + tok + " b\tc\nd"},
		// An IPv6 address before a colon (review M4).
		{"dial " + fxTail6 + ": refused", "dial <ip>: refused"},
		{"dial fd7a:115c:a1e0:1111:2222:3333:4444:5555: refused", "dial <ip>: refused"},
		{"dial 2001:db8::1: refused", "dial <ip>: refused"},
		{"dial 2001:db8::: refused", "dial <ip>: refused"},
		{"at 15:04:05: done", "at 15:04:05: done"},
		// The secret shapes of review M5.
		{"run --token abc123secret -password hunter2 --keep x", "run --token <secret> -password <secret> --keep x"},
		{"Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic <secret>"},
		{"Authorization: Bearer abc123", "Authorization: Bearer <secret>"},
		{`password: "correct horse battery" next`, `password: "<secret>" next`},
		{`err {\"token\":\"abc123secret\",\"n\":1}`, `err {\"token\":\"<secret>\",\"n\":1}`},
		{"q=token%3Dabc123secret&x=1", "q=token%3D<secret>&x=1"},
		{"password=<hunter2", "password=<secret>"},
		{"the key is missing", "the key is missing"},
		// The other homes (review L2).
		{"open /var/home/alice/x", "open ~/<path>"},
		{"read /root/.ssh/id_fixture", "read ~/<path>"},
		{"mount /rootfs/x", "mount /rootfs/x"},
		{"tmp /private/var/folders/ab/cd0ef/T/x.sock", "tmp ~/<path>"},
	} {
		if got := r.Scrub(c.in); got != c.want {
			t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A PEM block across lines of a file.
	sc := r.Scrubber()
	var out []string
	for _, l := range []string{"before", "x " + pemBegin("EC"), "body", "-----END EC PRIVATE KEY----- y", "after"} {
		if s, ok := sc.Line(l); ok {
			out = append(out, s)
		}
	}
	if got := strings.Join(out, "|"); got != "before|x <key>| y|after" {
		t.Errorf("PEM lines: %q", got)
	}
}

// TestLineFields: pass 2 runs on a line's own fields and on every key, and on the keys of a
// JSON value (review M1): a line from a writer with no source check can put a name there.
func TestLineFields(t *testing.T) {
	r := newWithSalt(Identity{Home: fxHome, Names: []string{fxUser, fxPeer}}, []byte("salt"))
	out, ok := r.Line(`{"t":"2026-11-27T10:00:00.000Z","lvl":"info","msg":"open ` + fxHome + `/develop/` + fxProject + ` at ` + fxPrivIP +
		` ` + fxPeer + `","proc":"` + fxPeer + `","comp":"` + fxPeer + `","` + fxPeer + `":"x","` + fxUser + `":1}`)
	if !ok {
		t.Fatal("not a line")
	}
	for _, s := range []string{fxUser, fxPeer, fxPrivIP, fxHome} {
		if strings.Contains(out, s) {
			t.Errorf("line holds %q: %s", s, out)
		}
	}
	if !strings.Contains(out, `"`+r.token(fxPeer)+`":"x"`) || !strings.Contains(out, `"`+r.token(fxUser)+`":1`) ||
		!strings.Contains(out, `"lvl":"info"`) {
		t.Errorf("line: %s", out)
	}
	j, _ := marshal(r.JSON(map[string]any{fxUser: map[string]any{fxPeer: "x"}}), false)
	if string(j) != `{"`+r.token(fxUser)+`":{"`+r.token(fxPeer)+`":"x"}}` {
		t.Errorf("JSON keys: %s", j)
	}
}

// TestLineSecretKeys: an attribute whose key names a secret loses its whole value, string or
// number, while a key that only contains the word keeps its value.
func TestLineSecretKeys(t *testing.T) {
	r := newWithSalt(Identity{}, []byte("salt"))
	// Built, so the commit-time secret scan does not flag a fixture.
	pwKey, pw := "session-pass"+"word", "fx-"+"pw-value"
	out, ok := r.Line(`{"t":"2026-11-27T10:00:00.000Z","lvl":"info","msg":"m","proc":"hab","comp":"c",` +
		`"token":"abc123plain","api_key":"k9k9k9","` + pwKey + `":"` + pw + `","pin":12345678,` +
		`"auth_token":987654,"token_count":3}`)
	if !ok {
		t.Fatal("not a line")
	}
	for _, s := range []string{"abc123plain", "k9k9k9", pw, "987654"} {
		if strings.Contains(out, s) {
			t.Errorf("line holds %q: %s", s, out)
		}
	}
	for _, s := range []string{`"token":"<secret>"`, `"api_key":"<secret>"`, `"auth_token":"<secret>"`, `"token_count":3`, `"pin":12345678`} {
		if !strings.Contains(out, s) {
			t.Errorf("line lacks %s: %s", s, out)
		}
	}
}

// TestPathClass: pass 1 on a path keeps the state folder's fixed layout, writes the home folder
// as ~, and hashes every other component.
func TestPathClass(t *testing.T) {
	r := newWithSalt(Identity{Home: fxHome, State: fxHome + "/st"}, []byte("salt"))
	for _, c := range []struct{ in, want string }{
		{fxHome + "/st/logs/hab.log.2", "<state>/logs/hab.log.2"},
		{fxHome + "/st/friction/" + fxProject, "<state>/friction/" + r.token(fxProject)},
		{fxHome + "/develop/" + fxProject, "~/" + r.token("develop") + "/" + r.token(fxProject)},
		{"/opt/" + fxProject, "/" + r.token("opt") + "/" + r.token(fxProject)},
	} {
		if got := r.Path(c.in); got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLineKeys: pass 1 by class on one line, and a number kept bare.
func TestLineKeys(t *testing.T) {
	r := newWithSalt(Identity{}, []byte("salt"))
	out, ok := r.Line(`{"t":"2026-11-27T10:00:00.000Z","lvl":"warn","msg":"m","proc":"hab","comp":"c","trace":"hABCDEFGHJKM","host":"` + fxHost + `","text":"at ` + fxDocIP + `","n":4,"flag":true}`)
	if !ok {
		t.Fatal("not a line")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v["host"] != r.token(fxHost) || v["text"] != "at <ip>" || v["n"] != float64(4) || v["flag"] != true ||
		v["trace"] != "hABCDEFGHJKM" || v["msg"] != "m" {
		t.Errorf("line: %s", out)
	}
}

// TestConfigKeys: every toml tag of a config struct that reads hab.toml or peers.toml is a key
// the bundle keeps, so a new key reads as written and not as a token. The other TOML files
// (detection manifests, the client keymap, Codex's config, the notices list) are left out.
func TestConfigKeys(t *testing.T) {
	root := filepath.Join("..", "..")
	other := regexp.MustCompile(`^internal/(detect|flightrules|gffclient|notices)/`)
	tag := regexp.MustCompile("toml:\"([^\",]+)")
	n := 0
	for _, top := range []string{"internal", "cmd"} {
		filepath.WalkDir(filepath.Join(root, top), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if other.MatchString(filepath.ToSlash(rel)) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range tag.FindAllStringSubmatch(string(b), -1) {
				n++
				if !configKeys[m[1]] {
					t.Errorf("%s: the toml key %q is not in configKeys", rel, m[1])
				}
			}
			return nil
		})
	}
	if n < 50 {
		t.Fatalf("only %d toml tags found: the walk missed the tree", n)
	}
}
