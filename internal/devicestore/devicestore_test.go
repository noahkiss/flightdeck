package devicestore

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var day = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func p256(t *testing.T) ssh.PublicKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func ed(t *testing.T) ssh.PublicKey {
	t.Helper()
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func authLine(pub ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment
}

func TestKeyLine(t *testing.T) {
	pub := p256(t)
	l, err := KeyLine(pub, "/usr/local/bin/gff-hab connect", "Owner's\tiPhone\x1b[31m", day)
	if err != nil {
		t.Fatal(err)
	}
	want := `restrict,command="/usr/local/bin/gff-hab connect" ` + authLine(pub, "flightdeck: Owner's iPhone[31m (paired 2026-11-02)")
	if l != want {
		t.Fatalf("line\n%s\nwant\n%s", l, want)
	}
	k, c, opts, _, err := ssh.ParseAuthorizedKey([]byte(l))
	if err != nil || !sameKey(k, pub) || !strings.HasPrefix(c, CommentPrefix) || len(opts) != 2 {
		t.Fatalf("sshd would read %v %q %v %v", k, c, opts, err)
	}
	if _, err := KeyLine(pub, `/bin/x"; evil`, "n", day); err == nil {
		t.Fatal("a quote in the command was accepted")
	}
	l, _ = KeyLine(pub, "", "", day)
	if !strings.HasPrefix(l, "restrict ecdsa-sha2-nistp256 ") || !strings.Contains(l, "flightdeck: device ") {
		t.Fatalf("no command, no name: %s", l)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  Owner's   iPhone ":    "Owner's iPhone",
		"a‮b​c":                  "abc",
		"x\x00y\x7fz\n":          "xyz",
		"\xff\xfeok":             "ok",
		strings.Repeat("é", 100): strings.Repeat("é", 64),
		"\t\r\n":                 "",
		"tab\there":              "tab here",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAppendKeepsOtherLines: every other byte stays, in order: comments, options, CRLF, blank
// lines, a last line with no newline. The file ends at 0600.
func TestAppendKeepsOtherLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")
	other := ed(t)
	before := "# keys for user\r\n" +
		`from="192.0.2.0/24",no-pty ` + authLine(other, "laptop") + "\r\n" +
		"\n" +
		"not a key line at all\n" +
		authLine(ed(t), "no final newline")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	pub := p256(t)
	line, _ := KeyLine(pub, "/opt/gff/bin/gff-hab connect", "phone", day)
	added, err := AppendAuthorizedKey(path, pub, line)
	if err != nil || !added {
		t.Fatalf("added %v, err %v", added, err)
	}
	got, _ := os.ReadFile(path)
	if want := before + "\n" + line + "\n"; string(got) != want {
		t.Fatalf("file\n%q\nwant\n%q", got, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// Again: the key is there, nothing changes.
	added, err = AppendAuthorizedKey(path, pub, line)
	if err != nil || added {
		t.Fatalf("second add: added %v, err %v", added, err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(got) {
		t.Fatal("a second add changed the file")
	}
	// A key the user already trusts by hand is not added twice.
	if added, _ := AppendAuthorizedKey(path, other, "restrict "+authLine(other, "flightdeck: x")); added {
		t.Fatal("a key already in the file was added")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files", len(entries))
	}

	// Remove takes out only our line.
	n, err := RemoveAuthorizedKey(path, pub)
	if err != nil || n != 1 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != before+"\n" {
		t.Fatalf("after remove\n%q\nwant\n%q", after, before+"\n")
	}
	// A line for the same key without our comment stays.
	if n, _ := RemoveAuthorizedKey(path, other); n != 0 {
		t.Fatal("removed a line pairing did not write")
	}
}

func TestAppendMakesFolderAndFile(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "home", "user", ".ssh")
	path := filepath.Join(ssh, "authorized_keys")
	pub := p256(t)
	line, _ := KeyLine(pub, "", "phone", day)
	if _, err := AppendAuthorizedKey(path, pub, line); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(ssh); fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", fi.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != line+"\n" {
		t.Fatalf("file %q", got)
	}
}

func TestAppendFollowsALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "authorized_keys")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("# mine\n"), 0o600)
	link := filepath.Join(dir, "authorized_keys")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pub := p256(t)
	line, _ := KeyLine(pub, "", "phone", day)
	if _, err := AppendAuthorizedKey(link, pub, line); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	got, _ := os.ReadFile(real)
	if string(got) != "# mine\n"+line+"\n" {
		t.Fatalf("target %q", got)
	}
	// A dangling link is refused.
	os.Remove(real)
	if _, err := AppendAuthorizedKey(link, pub, line); err == nil {
		t.Fatal("a dangling link was written through")
	}
}

func TestAppendRefusesLineBreak(t *testing.T) {
	pub := p256(t)
	if _, err := AppendAuthorizedKey(filepath.Join(t.TempDir(), "k"), pub, "a\nb"); err == nil {
		t.Fatal("a line break was accepted")
	}
}

func TestStoreAddFindRevoke(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "authorized_keys")
	os.WriteFile(keys, []byte("# keep\n"), 0o600)
	s := Open(dir)
	pub := p256(t)
	line, _ := KeyLine(pub, "", "Owner's iPhone", day)
	AppendAuthorizedKey(keys, pub, line)
	r := Record{ID: ssh.FingerprintSHA256(pub), Label: "Owner's iPhone", Kind: "phone", PairedAt: day.Format(time.RFC3339),
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), AuthorizedKeys: keys, State: StatePaired}
	if err := s.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(r); err != nil {
		t.Fatal(err)
	}
	devs, _ := s.List()
	if len(devs) != 1 {
		t.Fatalf("%d records after adding one device twice", len(devs))
	}
	if fi, _ := os.Stat(s.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode %v", fi.Mode().Perm())
	}
	if _, err := s.Find("SHA2"); err == nil {
		t.Fatal("a short prefix matched")
	}
	got, err := s.Find(strings.TrimPrefix(r.ID, "SHA256:")[:8])
	if err != nil || got.ID != r.ID {
		t.Fatalf("find by prefix: %v %v", got, err)
	}
	rv, n, err := s.Revoke("Owner's iPhone", day)
	if err != nil || n != 1 || rv.State != StateRevoked {
		t.Fatalf("revoke: %v %d %v", rv, n, err)
	}
	b, _ := os.ReadFile(keys)
	if string(b) != "# keep\n" {
		t.Fatalf("authorized_keys after revoke %q", b)
	}
	devs, _ = s.List()
	if len(devs) != 1 || devs[0].State != StateRevoked || devs[0].RevokedAt == "" {
		t.Fatalf("tombstone %+v", devs)
	}
	if _, _, err := s.Revoke(r.ID, day); err == nil {
		t.Fatal("a revoked device was revoked again")
	}
}
