// Package diagbundle builds the diagnostics bundle of docs/specs/diagnostics.md 8: the files of a
// hab's state folder that help a maintainer, redacted in two passes. Pass 1 works by key on
// structured log lines (diaglog's classes); pass 2 scrubs every string that is left. The bundle
// is a folder or a .tar.gz the user reviews and attaches by hand. Nothing here opens a network
// connection.
package diagbundle

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/internal/diaglog"
)

// The rules a bundle counts in its manifest (8.3). Pass 1 counts name and path values; pass 2
// counts each rule in the order it runs.
const (
	RuleName        = "name"
	RulePath        = "path"
	RuleKnownName   = "known-name"
	RulePrivateIPv4 = "private-ipv4"
	RuleCGNATIPv4   = "cgnat-ipv4"
	RuleTailnetIPv6 = "tailnet-ipv6"
	RuleTailnetName = "tailnet-name"
	RuleHomePath    = "home-path"
	RuleIP          = "ip"
	RuleEmail       = "email"
	RuleURL         = "url"
	RulePEM         = "pem"
	RuleSSHKey      = "ssh-key"
	RuleFingerprint = "fingerprint"
	RuleOpRef       = "op-ref"
	RuleSecret      = "secret"
	RuleBlob        = "blob"
)

// Rules lists every rule, in the order the manifest prints them.
var Rules = []string{RuleName, RulePath, RuleKnownName, RulePrivateIPv4, RuleCGNATIPv4, RuleTailnetIPv6,
	RuleTailnetName, RuleHomePath, RuleIP, RuleEmail, RuleURL, RulePEM, RuleSSHKey, RuleFingerprint, RuleOpRef,
	RuleSecret, RuleBlob}

// Identity is what pass 2's first rule knows of this machine (8.3).
type Identity struct {
	// Home is this user's home folder: pass 1 and pass 2 write it as ~.
	Home string
	// State is gff-hab's state folder: pass 1 writes it as <state> and keeps its fixed layout.
	State string
	// Names are replaced by their token wherever they stand, in any case: this machine's host
	// name and user name, each host, label and device id of the config files, the projects
	// gff-hab knows. A name shorter than MinName is left out, and so is a word the bundle's own
	// fields use (reservedName): a session named claude must not turn every harness id into a
	// token.
	Names []string
}

// MinName is the shortest name pass 2 replaces: a shorter one would hit common words.
const MinName = 3

// Redactor runs the two passes with one salt, so a value gives one token within a bundle and
// another in the next (8.3). It is safe for use by several goroutines.
type Redactor struct {
	salt  []byte
	home  string
	state string
	// names are the lower-case names, longest first, by their first byte.
	names map[byte][]string

	mu     sync.Mutex
	counts map[string]int
}

// New returns a Redactor with a new random salt.
func New(id Identity) *Redactor {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		panic("diagbundle: crypto/rand failed")
	}
	return newWithSalt(id, salt)
}

func newWithSalt(id Identity, salt []byte) *Redactor {
	r := &Redactor{salt: salt, home: filepath.Clean(id.Home), state: filepath.Clean(id.State),
		names: map[byte][]string{}, counts: map[string]int{}}
	if id.Home == "" {
		r.home = ""
	}
	if id.State == "" {
		r.state = ""
	}
	seen := map[string]bool{}
	for _, n := range id.Names {
		n = asciiLower(strings.TrimSpace(n))
		if len(n) < MinName || seen[n] || !strings.ContainsFunc(n, isLetter) || reservedName(n) {
			continue
		}
		seen[n] = true
		r.names[n[0]] = append(r.names[n[0]], n)
	}
	for k := range r.names {
		slices.SortFunc(r.names[k], func(a, b string) int { return len(b) - len(a) })
	}
	return r
}

// reservedWords are words of a bundle's own fields that no name replaces: the harness ids, the
// process kinds, the levels, the components and keys of a structured line.
var reservedWords = setOf("claude", "codex", "opencode", "hab", "holder", "gff", "gff-hab", "gff-hab-holder",
	"hook", "capcom", "engine", "app", "cli", "script", "debug", "info", "warn", "error", "legacy", "modhost",
	"trust", "speech", "peers", "rounds", "groups", "fixture", "session", "pane", "method", "kind", "trace",
	"harness", "event", "client", "client_id", "host", "label", "device", "was", "project", "branch", "title",
	"group", "path", "err", "text", "count", "dur", "epoch", "comp", "proc", "msg", "suppressed")

// reservedName reports whether a lower-case name is a word of the bundle's own: a reserved word,
// a config key the schema owns, or a name of the state folder's fixed layout.
func reservedName(n string) bool {
	return reservedWords[n] || configKeys[n] || stateFixed[n]
}

// Counts returns the replacements per rule so far, every rule included.
func (r *Redactor) Counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(Rules))
	for _, k := range Rules {
		out[k] = r.counts[k]
	}
	return out
}

func (r *Redactor) count(rule string, n int) {
	if n == 0 {
		return
	}
	r.mu.Lock()
	r.counts[rule] += n
	r.mu.Unlock()
}

// token is a name's stand-in: n: and 8 hex digits of HMAC-SHA256 keyed with the salt, over the
// lower-case value, so pass 1 and pass 2 agree.
func (r *Redactor) token(v string) string {
	m := hmac.New(sha256.New, r.salt)
	m.Write([]byte(asciiLower(v)))
	return "n:" + hex.EncodeToString(m.Sum(nil))[:8]
}

// Name is pass 1 on a value of class name: its token. An empty value stays empty.
func (r *Redactor) Name(v string) string {
	if v = clean(v); v == "" {
		return v
	}
	r.count(RuleName, 1)
	return r.token(v)
}

// stateFixed are the names of gff-hab's own layout under <state>, which a path keeps (8.3).
var stateFixed = map[string]bool{
	"logs": true, "run": true, "crash": true, "capcom": true, "engine": true, "friction": true, "tallies": true,
	"bin": true, "builds": true, "current": true, "modules": true, "logbook": true, "rounds": true, "groups": true,
	"manifests": true, "attachments": true, "hab.log": true, "hab.stderr": true, "hook.log": true,
	"capcom.log": true, "engine.log": true, "health.jsonl": true, "structure.json": true, "settings.json": true,
	"seen.json": true, "hab.json": true, "debug.json": true, "install.json": true, "governor.json": true,
}

// stateFixedRE matches the other fixed names under <state>: an old log file, a holder's log, a
// crash file or a stack dump, a tally day, a friction note, a holder's run marker.
var stateFixedRE = regexp.MustCompile(`^(?:[a-z]+\.log\.[0-9]+|gff-hab-holder\.[A-Za-z0-9_-]+\.log(?:\.[0-9]+)?|(?:hab|holder)-[0-9a-f]+(?:-stacks-[0-9TZ._-]+)?\.txt|[0-9]{4}-[0-9]{2}-[0-9]{2}\.json|[0-9TZ._-]+-[A-Za-z0-9]{12}\.json|holder\.[A-Za-z0-9_-]+\.json)$`)

// Path is pass 1 on a value of class path. The state folder becomes <state> and keeps its fixed
// layout; the home folder becomes ~; every other component becomes its token.
func (r *Redactor) Path(p string) string {
	if p = clean(p); p == "" {
		return p
	}
	var prefix, rest string
	fixed := false
	switch {
	case r.state != "" && (p == r.state || strings.HasPrefix(p, r.state+"/")):
		prefix, rest, fixed = "<state>", strings.TrimPrefix(p, r.state), true
	case r.home != "" && (p == r.home || strings.HasPrefix(p, r.home+"/")):
		prefix, rest = "~", strings.TrimPrefix(p, r.home)
	default:
		rest = p
	}
	parts := strings.Split(rest, "/")
	for i, c := range parts {
		switch {
		case c == "" || c == "." || c == ".." || c == "~":
		case fixed && (stateFixed[c] || stateFixedRE.MatchString(c)):
		default:
			parts[i] = r.token(c)
		}
	}
	r.count(RulePath, 1)
	return prefix + strings.Join(parts, "/")
}

// Value is pass 1 on one value of a structured line, by its key's class (8.3), then pass 2 on
// what remains. A safe value is scrubbed too: a minted id matches no rule, so it reads the same,
// and a value that only looks safe cannot carry a name out (diagnostics review L3).
func (r *Redactor) Value(key, v string) string {
	switch diaglog.KeyClass(key) {
	case diaglog.ClassName:
		return r.Name(v)
	case diaglog.ClassPath:
		return r.Path(v)
	}
	return r.Scrub(v)
}

// Line is both passes on one structured line (diagnostics.md 4.2), written back as JSON in the
// order the sink writes: t, lvl, msg, proc, epoch, comp, trace, then the values. Every field
// and every key gets pass 2 too: a Go writer's constants read the same after it, and a line from
// another writer (capcom.log, an older holder) has no source check (diagnostics review M1). ok is
// false for a line that is not a structured line.
func (r *Redactor) Line(s string) (string, bool) {
	l, ok := diaglog.Parse(s)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteByte('{')
	field := func(k, v string) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		writeJSONString(&b, k)
		b.WriteByte(':')
		writeJSONString(&b, v)
	}
	field(diaglog.KeyT, l.T.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	field(diaglog.KeyLvl, r.Scrub(l.Lvl))
	field(diaglog.KeyMsg, r.Scrub(l.Msg))
	field(diaglog.KeyProc, r.Scrub(l.Proc))
	if l.Epoch != "" {
		field(diaglog.KeyEpoch, r.Scrub(l.Epoch))
	}
	field(diaglog.KeyComp, r.Scrub(l.Comp))
	if l.Trace != "" {
		field(diaglog.KeyTrace, r.Scrub(l.Trace))
	}
	for _, kv := range l.Attrs {
		key := r.Scrub(kv.Key)
		if secretAttrRE.MatchString(kv.Key) {
			// The key names a secret, so the value goes whole: pass 2 alone sees no key word.
			field(key, "<secret>")
			r.count(RuleSecret, 1)
			continue
		}
		if kv.Num && plainJSON(kv.Val) {
			b.WriteByte(',')
			writeJSONString(&b, key)
			b.WriteByte(':')
			b.WriteString(kv.Val)
			continue
		}
		field(key, r.Value(kv.Key, kv.Val))
	}
	b.WriteByte('}')
	return b.String(), true
}

// secretAttrRE matches a line attribute key that names a secret: the word alone or as the
// last part of the key, such as token, api_key or session-password.
var secretAttrRE = regexp.MustCompile(`(?i)^(?:.*[_.-])?(?:token|secret|password|passwd|authorization|api_?key|private_?key)$`)

// plainJSON reports whether a value written bare is a JSON number or Boolean.
func plainJSON(s string) bool {
	if s == "true" || s == "false" || s == "null" {
		return true
	}
	var n json.Number
	return json.Unmarshal([]byte(s), &n) == nil
}

func writeJSONString(b *strings.Builder, s string) {
	j, _ := marshal(s, false)
	b.Write(j)
}

// marshal is JSON with no HTML escaping, so <ip> and <secret> read as written. indent: two
// spaces per level. The result has no final newline.
func marshal(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// JSON is pass 2 on every string in a decoded JSON value (keys too), in place where it can, and
// returns the result. Pass 1 has no keys outside structured lines, so a caller that knows a
// field's class applies it first.
func (r *Redactor) JSON(v any) any {
	switch x := v.(type) {
	case string:
		return r.Scrub(x)
	case []any:
		for i := range x {
			x[i] = r.JSON(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[r.Scrub(k)] = r.JSON(e)
		}
		return out
	}
	return v
}

// Scrub is pass 2 on one string (8.3): control sequences out, then each rule in order. A PEM block within the string is
// replaced whole; a log file's block that spans lines goes through a Scrubber.
func (r *Redactor) Scrub(s string) string {
	if s = clean(s); s == "" {
		return s
	}
	s = r.knownNames(s)
	s = r.ipv4(s)
	s = r.ipv6(s)
	s = r.replace(s, RuleTailnetName, tailnetNameRE, func(string) string { return "<host>" })
	s = r.homePaths(s)
	// The URL rule runs before the email rule: user info such as user:pw@host would otherwise
	// read as an address, and leave the user name.
	s = r.urls(s)
	s = r.replace(s, RuleEmail, emailRE, func(string) string { return "<email>" })
	s = r.replace(s, RulePEM, pemRE, func(string) string { return "<key>" })
	s = r.replace(s, RuleSSHKey, sshKeyRE, func(string) string { return "<key>" })
	s = r.replace(s, RuleFingerprint, fingerprintRE, func(string) string { return "<key>" })
	s = r.replace(s, RuleOpRef, opRefRE, func(string) string { return "<secret>" })
	s = r.secrets(s)
	return r.blobs(s)
}

var (
	tailnetNameRE = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+ts\.net\b`)
	emailRE       = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	pemRE         = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|$)`)
	sshKeyRE      = regexp.MustCompile(`\b(?:ssh|ecdsa|sk-ssh|sk-ecdsa)-[A-Za-z0-9@.-]+\s+AAAA[A-Za-z0-9+/]+={0,3}`)
	fingerprintRE = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]{20,}={0,2}`)
	// An op:// reference's vault and item names may hold spaces; its field ends at one.
	opRefRE  = regexp.MustCompile(`op://[^/\n"'<>]+/[^/\n"'<>]+/[^\s"'<>]+|op://[^\s"'<>]+`)
	urlRE    = regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s/?#@"'<>]*@)?([^\s?#"'<>]*)(\?[^\s#"'<>]*)?(#[^\s"'<>]*)?`)
	bearerRE = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]+)`)
	// secretKeyRE ends at the separator after a secret's key: an optional quote, \ escaped or
	// not, then :, = or their URL encoding.
	secretKeyRE = regexp.MustCompile(`(?i)(token|key|secret|password|passwd|authorization)[A-Za-z0-9_-]*(?:\\?["'])?\s*(?:[:=]|%3[ad])\s*`)
	// secretFlagRE ends at the space after a command line flag that names a secret.
	secretFlagRE = regexp.MustCompile(`(?i)(?:^|[\s"'=])--?[a-z0-9_-]*(?:token|secret|password|passwd|key)[a-z0-9_-]*[ \t]+`)
	schemeRE     = regexp.MustCompile(`(?i)^(?:basic|bearer|token|digest)\s+`)
	homeRE       = regexp.MustCompile(`/(?:Users|home|var/home)/[A-Za-z0-9._-]+(?:/[^\s"'<>,;]*)?|/root(?:/[^\s"'<>,;]*)?|(?:/private)?/var/folders/[^/\s"'<>,;]+/[^/\s"'<>,;]+(?:/[^\s"'<>,;]*)?`)
	ipv4RE       = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`)
	ipv6RE       = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:%[0-9A-Za-z]+)?`)
	blobRE       = regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}|[A-Za-z0-9+/]{40,}={0,2}`)
	uuidRE       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// replace runs one rule: each match of re becomes f(match), counted under rule.
func (r *Redactor) replace(s, rule string, re *regexp.Regexp, f func(string) string) string {
	n := 0
	out := re.ReplaceAllStringFunc(s, func(m string) string {
		n++
		return f(m)
	})
	r.count(rule, n)
	return out
}

// knownNames replaces each name of the Identity where it stands as a word: no letter or digit
// right before or after it. The home folder becomes ~.
func (r *Redactor) knownNames(s string) string {
	low := asciiLower(s)
	home := asciiLower(r.home)
	var b strings.Builder
	last, n := 0, 0
	for i := 0; i < len(s); i++ {
		if i > 0 && isAlnum(s[i-1]) {
			continue
		}
		if home != "" && strings.HasPrefix(low[i:], home) && (i+len(r.home) == len(s) || !isAlnum(s[i+len(r.home)])) {
			b.WriteString(s[last:i])
			b.WriteString("~")
			i += len(r.home) - 1
			last, n = i+1, n+1
			continue
		}
		for _, name := range r.names[low[i]] {
			if strings.HasPrefix(low[i:], name) && (i+len(name) == len(s) || !isAlnum(s[i+len(name)])) {
				b.WriteString(s[last:i])
				b.WriteString(r.token(name))
				i += len(name) - 1
				last, n = i+1, n+1
				break
			}
		}
	}
	if n == 0 {
		return s
	}
	b.WriteString(s[last:])
	r.count(RuleKnownName, n)
	return b.String()
}

// ipv4 replaces dotted quads: the hygiene scan's private and CGNAT ranges under their rules,
// every other address under ip.
func (r *Redactor) ipv4(s string) string {
	return r.each(s, ipv4RE, func(s string, i, j int) (string, string) {
		if i > 0 && (isDigit(s[i-1]) || s[i-1] == '.') {
			return "", ""
		}
		if j < len(s) && (isDigit(s[j]) || s[j] == '.' && j+1 < len(s) && isDigit(s[j+1])) {
			return "", ""
		}
		a, err := netip.ParseAddr(s[i:j])
		if err != nil {
			return "", ""
		}
		switch {
		case a.IsPrivate():
			return RulePrivateIPv4, "<ip>"
		case cgnat.Contains(a):
			return RuleCGNATIPv4, "<ip>"
		}
		return RuleIP, "<ip>"
	})
}

// The hygiene scan's CGNAT range, 100.64/10, and its tailnet IPv6 range, the /48 that starts
// fd7a, 115c, a1e0 (ci-release.md 9.4). They are built from bytes so this file holds no address
// the scan would flag.
var (
	cgnat    = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10)
	tailnet6 = netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0}), 48)
)

// ipv6 replaces IPv6 addresses: the tailnet range under its rule, every other under ip.
func (r *Redactor) ipv6(s string) string {
	return r.each(s, ipv6RE, func(s string, i, j int) (string, string) {
		if i > 0 && (isHex(s[i-1]) || s[i-1] == ':' || s[i-1] == '.') {
			return "", ""
		}
		m, tail := s[i:j], ""
		a, err := netip.ParseAddr(m)
		if err != nil && strings.HasSuffix(m, ":") {
			// "%s: %w" after an address: the match took the colon that follows it.
			m, tail, j = m[:len(m)-1], ":", j-1
			a, err = netip.ParseAddr(m)
		}
		// A colon after the address ends it unless a hex digit follows: then the run is longer
		// than an address, such as a time or a MAC address.
		if j < len(s) && (isAlnum(s[j]) || s[j] == ':' && j+1 < len(s) && isHex(s[j+1])) {
			return "", ""
		}
		if err != nil || !a.Is6() || !strings.ContainsFunc(m, func(c rune) bool { return c != ':' }) {
			return "", ""
		}
		if tailnet6.Contains(a.WithZone("")) {
			return RuleTailnetIPv6, "<ip>" + tail
		}
		return RuleIP, "<ip>" + tail
	})
}

// each replaces each match of re that f takes: f returns the rule and the replacement, or an
// empty rule to keep the match.
func (r *Redactor) each(s string, re *regexp.Regexp, f func(s string, i, j int) (rule, with string)) string {
	locs := re.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		rule, with := f(s, l[0], l[1])
		if rule == "" {
			continue
		}
		b.WriteString(s[last:l[0]])
		b.WriteString(with)
		last = l[1]
		r.count(rule, 1)
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// homePaths replaces another user's home folder, and the path under it, with ~/<path>, where it
// starts a path (the hygiene scan's home-path rule), and the other homes: /var/home, /root, and
// a macOS user's temporary folder under /private/var/folders.
func (r *Redactor) homePaths(s string) string {
	return r.each(s, homeRE, func(s string, i, j int) (string, string) {
		if i > 0 && (isAlnum(s[i-1]) || strings.IndexByte("._-/", s[i-1]) >= 0) {
			return "", ""
		}
		if k := i + len("/root"); strings.HasPrefix(s[i:], "/root") && k < len(s) && s[k] != '/' &&
			(isAlnum(s[k]) || strings.IndexByte("._-", s[k]) >= 0) {
			return "", "" // /rootfs, not /root
		}
		return RuleHomePath, "~/<path>"
	})
}

// urls removes a URL's user info, query and fragment.
func (r *Redactor) urls(s string) string {
	n := 0
	out := urlRE.ReplaceAllStringFunc(s, func(m string) string {
		p := urlRE.FindStringSubmatch(m)
		if p[2] == "" && p[4] == "" && p[5] == "" {
			return m
		}
		n++
		return p[1] + p[3]
	})
	r.count(RuleURL, n)
	return out
}

// secrets replaces the value after a bearer; after token, key, secret, password or
// authorization followed by : or = (or %3A, %3D), with an optional quote, \ escaped or not, on
// either side of it; and after a command line flag that names one, such as --token x. A quoted
// value is taken to its closing quote. An authorization scheme (Basic, Bearer, Token, Digest)
// stays and the word after it goes.
func (r *Redactor) secrets(s string) string {
	n := 0
	s = bearerRE.ReplaceAllStringFunc(s, func(m string) string {
		p := bearerRE.FindStringSubmatch(m)
		if placeholderRE.MatchString(p[2]) {
			return m
		}
		n++
		return p[1] + "<secret>"
	})
	for _, re := range []*regexp.Regexp{secretKeyRE, secretFlagRE} {
		var b strings.Builder
		last := 0
		for pos := 0; pos < len(s); {
			loc := re.FindStringIndex(s[pos:])
			if loc == nil {
				break
			}
			k := pos + loc[1]
			v0, v1, ok := secretValue(s, k, re == secretFlagRE)
			if !ok {
				pos = max(k, pos+1)
				continue
			}
			b.WriteString(s[last:v0])
			b.WriteString("<secret>")
			last, pos = v1, v1
			n++
		}
		if last > 0 {
			b.WriteString(s[last:])
			s = b.String()
		}
	}
	r.count(RuleSecret, n)
	return s
}

// secretQuotes are the quotes a secret's value may stand in, longest first.
var secretQuotes = []string{`\"`, `\'`, "%22", `"`, `'`}

// placeholderRE matches what an earlier rule wrote, which the secret rule keeps.
var placeholderRE = regexp.MustCompile(`^<(?:secret|ip|host|email|key|blob|path|value)>`)

// secretValue finds the value that starts at k: within its quotes, else after an authorization
// scheme, up to white space, a quote, a backslash or , ; &. flag: the value of a command line
// flag, which another flag does not start. ok is false for no value, or one a rule already wrote.
func secretValue(s string, k int, flag bool) (v0, v1 int, ok bool) {
	if !flag {
		for _, q := range secretQuotes {
			if !strings.HasPrefix(s[k:], q) {
				continue
			}
			v0 = k + len(q)
			v1 = len(s)
			for i := v0; i < len(s); i++ {
				if strings.HasPrefix(s[i:], q) && (len(q) > 1 || s[i-1] != '\\') {
					v1 = i
					break
				}
			}
			return v0, v1, v1 > v0 && !placeholderRE.MatchString(s[v0:v1])
		}
		k += len(schemeRE.FindString(s[k:]))
	}
	v1 = k
	for v1 < len(s) && strings.IndexByte(" \t\r\n\"',;&\\", s[v1]) < 0 {
		v1++
	}
	if v1 == k || flag && s[k] == '-' || placeholderRE.MatchString(s[k:v1]) {
		return 0, 0, false
	}
	return k, v1, true
}

// blobs replaces runs of 32 or more base64, base64url or hex characters that hold a letter and a
// digit. A UUID, the form of a pane id, is kept: it is an id Flightdeck minted.
func (r *Redactor) blobs(s string) string {
	return r.each(s, blobRE, func(s string, i, j int) (string, string) {
		m := s[i:j]
		if uuidRE.MatchString(m) || !strings.ContainsFunc(m, isLetter) || !strings.ContainsFunc(m, func(c rune) bool { return c >= '0' && c <= '9' }) {
			return "", ""
		}
		if strings.Contains(m, "/") && !(strings.ContainsFunc(m, func(c rune) bool { return c >= 'A' && c <= 'Z' }) &&
			strings.ContainsFunc(m, func(c rune) bool { return c >= 'a' && c <= 'z' })) {
			return "", "" // a path, not base64
		}
		return RuleBlob, "<blob>"
	})
}

// Scrubber is pass 2 over the lines of one text file: it carries a PEM block from its BEGIN line
// to its END line, so no line of the block is written.
type Scrubber struct {
	r     *Redactor
	inPEM bool
}

// Scrubber returns a Scrubber for one file.
func (r *Redactor) Scrubber() *Scrubber { return &Scrubber{r: r} }

var pemBeginRE = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`)
var pemEndRE = regexp.MustCompile(`-----END [A-Z0-9 ]+-----`)

// Line scrubs one line. It returns "" and false for a line inside a PEM block.
func (sc *Scrubber) Line(s string) (string, bool) {
	if sc.inPEM {
		loc := pemEndRE.FindStringIndex(s)
		if loc == nil {
			return "", false
		}
		sc.inPEM = false
		s = s[loc[1]:]
	} else if loc := pemBeginRE.FindStringIndex(s); loc != nil && !pemEndRE.MatchString(s[loc[1]:]) {
		sc.inPEM = true
		sc.r.count(RulePEM, 1)
		return sc.r.Scrub(s[:loc[0]]) + "<key>", true
	}
	return sc.r.Scrub(s), true
}

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool   { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
func isAlnum(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isLetter(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// clean removes terminal control sequences (CSI, OSC and the other ESC forms) and the C0
// controls and DEL, keeping tab and newline, before both passes (diagnostics review M6): an SGR
// sequence's final letter would otherwise touch a name, and raw escapes would reach the terminal
// of whoever reads the bundle. Where a removed run stood between two letters or digits it leaves
// a space, so the words on each side still stand as words.
func clean(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	cut := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i, cut = skipEscape(s, i), true
			continue
		case isControl(rune(c)):
			i, cut = i+1, true
			continue
		}
		if cut && b.Len() > 0 && isAlnum(b.String()[b.Len()-1]) && isAlnum(c) {
			b.WriteByte(' ')
		}
		cut = false
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isControl(c rune) bool { return c < 0x20 && c != '\t' && c != '\n' || c == 0x7f }

// skipEscape returns the index after the escape sequence that starts at s[i], an ESC: a CSI to
// its final byte, an OSC, DCS, SOS, PM or APC string to BEL or ST, any other to its final byte.
// A sequence the string cuts short runs to its end.
func skipEscape(s string, i int) int {
	i++
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		for i++; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
			if s[i] < 0x20 || s[i] > 0x7e {
				return i
			}
		}
		return i
	case ']', 'P', 'X', '^', '_':
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return i
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) {
		i++
	}
	return i
}
