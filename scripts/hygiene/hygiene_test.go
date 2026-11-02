// Package hygiene tests scripts/hygiene-scan.sh in temporary git repositories.
//
// Every planted value is invented and built from parts at run time, so no address, path or
// pattern that the scan would flag sits in this file as one literal.
package hygiene

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var (
	privIP   = "192" + ".168.7.9"
	cgnatIP  = "100" + ".64.3.4"
	homeLeak = "/home" + "/zed/notes"
	fakeHost = "qzx" + "-fakehost-77"
	fakeRule = "host-name\t" + "qzx-fake" + "host-[0-9]+"
)

func script(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "hygiene-scan.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type repo struct {
	t   *testing.T
	dir string
	env []string
}

// newRepo makes a git repository whose git ignores the user's global and system config, so a
// global hooks path or identity never changes what the script sees.
func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	r := &repo{t: t, dir: t.TempDir()}
	r.env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GFF_HYGIENE_PATTERNS=", "GFF_HYGIENE_PATTERNS_FILE=",
		"GFF_HYGIENE_PR_TITLE=", "GFF_HYGIENE_PR_BODY=", "GFF_HYGIENE_REF=", "GFF_HYGIENE_AWK=",
	)
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(name, body string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// scan runs the script and returns its exit code and combined output. Where dash is installed
// it runs the script under dash too and requires the same result, which keeps it POSIX.
func (r *repo) scan(extraEnv []string, args ...string) (int, string) {
	r.t.Helper()
	code, out := r.run(nil, extraEnv, args...)
	if dash, err := exec.LookPath("dash"); err == nil {
		dcode, dout := r.run([]string{dash}, extraEnv, args...)
		if dcode != code || dout != out {
			r.t.Fatalf("dash differs: exit %d vs %d\n--- sh\n%s--- dash\n%s", code, dcode, out, dout)
		}
	}
	// CI runs the scan with gawk (hygiene.yml); where gawk is installed, it must agree.
	if gawk, err := exec.LookPath("gawk"); err == nil {
		gcode, gout := r.run(nil, append(append([]string{}, extraEnv...), "GFF_HYGIENE_AWK="+gawk), args...)
		if gcode != code || gout != out {
			r.t.Fatalf("gawk differs: exit %d vs %d\n--- awk\n%s--- gawk\n%s", code, gcode, out, gout)
		}
	}
	return code, out
}

func (r *repo) run(shell, extraEnv []string, args ...string) (int, string) {
	r.t.Helper()
	argv := append(append(shell, script(r.t)), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.dir
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return code, string(out)
}

// noLeak fails when the output holds any planted value or a part of the fake pattern.
func noLeak(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{privIP, cgnatIP, homeLeak, "/zed", fakeHost, "fakehost", "[0-9]+"} {
		if strings.Contains(out, s) {
			t.Fatalf("output holds planted text %q:\n%s", s, out)
		}
	}
}

func want(t *testing.T, code int, out string, wantCode int, lines ...string) {
	t.Helper()
	noLeak(t, out)
	if code != wantCode {
		t.Fatalf("exit %d, want %d:\n%s", code, wantCode, out)
	}
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Fatalf("output lacks %q:\n%s", l, out)
		}
	}
}

func TestStagedClean(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "nothing here\nloopback 127.0.0.1 and docs 192.0.2.1\n/home/user/x and /Users/u/y\n")
	r.git("add", "-A")
	code, out := r.scan(nil, "--staged")
	want(t, code, out, 0)
	if !strings.Contains(out, "built-in rules only") {
		t.Fatalf("no notice that private rules are missing:\n%s", out)
	}
}

func TestStagedGenericLeaks(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "line one\nhost at "+privIP+".\n"+cgnatIP+"\npath "+homeLeak+"\n")
	r.git("add", "-A")
	code, out := r.scan(nil, "--staged")
	want(t, code, out, 1, "a.txt:2: private-ipv4", "a.txt:3: cgnat-ipv4", "a.txt:4: home-path")
}

func TestCGNATEdges(t *testing.T) {
	r := newRepo(t)
	in := []string{"100" + ".63.1.1", "100" + ".64.0.1", "100" + ".127.9.9", "100" + ".128.0.1", "1100" + ".64.0.1"}
	r.write("a.txt", strings.Join(in, "\n")+"\n")
	r.git("add", "-A")
	code, out := r.scan(nil, "--staged")
	want(t, code, out, 1, "a.txt:2: cgnat-ipv4", "a.txt:3: cgnat-ipv4")
	if strings.Count(out, "cgnat-ipv4") != 2 {
		t.Fatalf("want two hits:\n%s", out)
	}
}

func TestPrivatePatternFromEnvAndFile(t *testing.T) {
	r := newRepo(t)
	r.write("b.md", "deploy to "+fakeHost+" tonight\n")
	r.git("add", "-A")

	code, out := r.scan([]string{"GFF_HYGIENE_PATTERNS=# comment\n" + fakeRule + "\n"}, "--staged")
	want(t, code, out, 1, "b.md:1: host-name")

	pf := filepath.Join(t.TempDir(), "patterns")
	if err := os.WriteFile(pf, []byte(fakeRule+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = r.scan([]string{"GFF_HYGIENE_PATTERNS_FILE=" + pf}, "--staged")
	want(t, code, out, 1, "b.md:1: host-name")

}

// A rule that is malformed, does not compile, or matches every line stops the scan. The error
// names the rule's number only: awk prints a bad regex in its own message, so none may reach the
// output.
func TestBadPrivateRulesStopTheScan(t *testing.T) {
	r := newRepo(t)
	r.write("b.md", "deploy to "+fakeHost+" tonight\n")
	r.git("add", "-A")
	for _, tc := range []struct{ name, rules, wantIndex string }{
		{"no tab", "# c\nqzx-fakehost-no-tab\n" + fakeRule, "rule 1"},
		{"bad regex", fakeRule + "\nleak\t" + "qzx-fake" + "host-(", "rule 2"},
		{"bad bracket", "leak\t" + "qzx-fake" + "host-[0-9", "rule 1"},
		{"matches empty", fakeRule + "\nall\t" + "(qzx-fakehost)*", "rule 2"},
		{"bad name", "host name\t" + "qzx-fakehost", "rule 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := r.scan([]string{"GFF_HYGIENE_PATTERNS=" + tc.rules}, "--staged")
			want(t, code, out, 2)
			if !strings.Contains(out, tc.wantIndex) {
				t.Fatalf("error does not name %s:\n%s", tc.wantIndex, out)
			}
			for _, s := range []string{"qzx", "fakehost", "(", "["} {
				if strings.Contains(out, s) {
					t.Fatalf("error holds %q:\n%s", s, out)
				}
			}
		})
	}
}

// A built-in rule for the tailnet: its IPv6 prefix and its DNS names.
func TestTailnetRules(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "ok\nreach "+"fd7a"+":115c:a1e0::5 now\nssh box."+"example-tail"+".ts"+".net\nFD7A"+":115C:A1E0:ab\n")
	r.git("add", "-A")
	code, out := r.scan(nil, "--staged")
	want(t, code, out, 1, "a.txt:2: tailnet-ipv6", "a.txt:3: tailnet-name", "a.txt:4: tailnet-ipv6")
	if strings.Contains(out, "example-tail") || strings.Contains(out, "a1e0") {
		t.Fatalf("output holds planted text:\n%s", out)
	}
}

// A path that hits is withheld everywhere it would be printed, and still fails the scan.
func TestPathHitIsWithheld(t *testing.T) {
	r := newRepo(t)
	r.write("notes/"+fakeHost+".md", "clean\nnext "+privIP+"\n")
	r.git("add", "-A")
	code, out := r.scan([]string{"GFF_HYGIENE_PATTERNS=" + fakeRule}, "--staged")
	want(t, code, out, 1, "<path withheld>: host-name (file path)", "<path withheld>:2: private-ipv4")
	base := r.commit("add notes")
	_ = base
	code, out = r.scan([]string{"GFF_HYGIENE_PATTERNS=" + fakeRule}, "--tree")
	want(t, code, out, 1, "<path withheld>: host-name (file path)")
}

func TestAllowFileCoversBuiltInOnly(t *testing.T) {
	r := newRepo(t)
	r.write(".github/hygiene-allow", "# meant\nfix.txt\tprivate-ipv4\n")
	r.write("fix.txt", "gateway "+privIP+" at "+fakeHost+"\n")
	r.git("add", "-A")
	code, out := r.scan([]string{"GFF_HYGIENE_PATTERNS=" + fakeRule}, "--staged")
	want(t, code, out, 1, "fix.txt:1: host-name")
	if strings.Contains(out, "private-ipv4") {
		t.Fatalf("an allowed built-in hit was reported:\n%s", out)
	}
}

func TestRangeDiffMessagesAndPullRequest(t *testing.T) {
	r := newRepo(t)
	r.write("keep.txt", "one\ntwo\n")
	base := r.commit("start")

	r.write("keep.txt", "one\ntwo\n++ "+privIP+"\n")
	r.write("bin.dat", "\x00\x01"+privIP+"\x00")
	head := r.commit("move the box to " + fakeHost + "\n\nbody on " + cgnatIP)
	short := r.git("rev-parse", "--short", head)

	code, out := r.scan([]string{
		"GFF_HYGIENE_PATTERNS=" + fakeRule,
		"GFF_HYGIENE_PR_TITLE=Ship it",
		"GFF_HYGIENE_PR_BODY=line one\nruns on " + fakeHost,
	}, "--range", base+".."+head)
	want(t, code, out, 1,
		"keep.txt (commit "+short+"):3: private-ipv4",
		"commit "+short+":1: host-name",
		"commit "+short+":3: cgnat-ipv4",
		"pull request body:2: host-name")
	if strings.Contains(out, "bin.dat") || strings.Contains(out, "pull request title") {
		t.Fatalf("binary file or clean title reported:\n%s", out)
	}

	// Removing the leak adds nothing to scan.
	r.write("keep.txt", "one\ntwo\n")
	r.git("rm", "-q", "bin.dat")
	fix := r.commit("tidy")
	code, out = r.scan(nil, "--range", head+".."+fix)
	want(t, code, out, 0)
}

// A leak one commit adds and a later commit removes was still pushed: the range fails on it.
func TestRangeScansEveryCommit(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "one\n")
	base := r.commit("start")
	r.write("a.txt", "one\n"+privIP+"\n")
	leak := r.commit("oops")
	r.write("a.txt", "one\n")
	head := r.commit("remove it")
	short := r.git("rev-parse", "--short", leak)
	code, out := r.scan(nil, "--range", base+".."+head)
	want(t, code, out, 1, "a.txt (commit "+short+"):2: private-ipv4")
}

// Author and committer identities, and the branch or tag name, are pushed text too.
func TestRangeScansIdentityAndRef(t *testing.T) {
	r := newRepo(t)
	base := r.commit("start")
	r.env = append(r.env, "GIT_AUTHOR_EMAIL=me@"+fakeHost+".local")
	head := r.commit("clean message")
	short := r.git("rev-parse", "--short", head)
	code, out := r.scan([]string{"GFF_HYGIENE_PATTERNS=" + fakeRule, "GFF_HYGIENE_REF=topic/" + fakeHost}, "--range", base+".."+head)
	want(t, code, out, 1, "commit "+short+" author:1: host-name", "ref name:1: host-name")
	if strings.Contains(out, "committer") {
		t.Fatalf("a clean committer was reported:\n%s", out)
	}
}

func TestRangeIgnoresWhatTheBaseGained(t *testing.T) {
	r := newRepo(t)
	start := r.commit("start")
	r.git("checkout", "-q", "-b", "topic")
	r.write("topic.txt", "clean\n")
	topic := r.commit("topic work")
	r.git("checkout", "-q", "main")
	r.write("main.txt", privIP+"\n")
	mainHead := r.commit("main gained this")
	_ = start
	code, out := r.scan(nil, "--range", mainHead+".."+topic)
	want(t, code, out, 0)
}

func TestTreeAndMessage(t *testing.T) {
	r := newRepo(t)
	r.write("ok.txt", "fine\n")
	r.write("sub/leak.go", "package x\n\n// see "+homeLeak+"\n")
	r.commit("start")
	code, out := r.scan(nil, "--tree")
	want(t, code, out, 1, "sub/leak.go:3: home-path")

	msg := filepath.Join(t.TempDir(), "MSG")
	if err := os.WriteFile(msg, []byte("subject\n# "+privIP+" is a git comment\nnext "+privIP+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = r.scan(nil, "--message", msg)
	want(t, code, out, 1, "commit message:3: private-ipv4")

	// `git commit -v` puts the diff below the scissors line; removing a leak must not be blocked.
	scissors := "remove the old address\n\n# ------------------------ >8 ------------------------\n# Do not modify or remove the line above.\ndiff --git a/a b/a\n-" + privIP + "\n"
	if err := os.WriteFile(msg, []byte(scissors), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = r.scan(nil, "--message", msg)
	want(t, code, out, 0)

	// core.commentChar picks the comment lines git drops.
	r.git("config", "core.commentChar", ";")
	if err := os.WriteFile(msg, []byte("subject\n; "+privIP+" is a comment here\n# "+privIP+" is not\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = r.scan(nil, "--message", msg)
	want(t, code, out, 1, "commit message:3: private-ipv4")
	if strings.Contains(out, "message:2:") {
		t.Fatalf("a comment line was scanned:\n%s", out)
	}
}

func TestInstallHooks(t *testing.T) {
	r := newRepo(t)
	code, out := r.scan(nil, "--install-hooks")
	want(t, code, out, 0)
	for _, h := range []string{"pre-commit", "commit-msg"} {
		b, err := os.ReadFile(filepath.Join(r.dir, ".git", "hooks", h))
		if err != nil || !bytes.Contains(b, []byte("hygiene-scan.sh")) {
			t.Fatalf("hook %s not installed: %v", h, err)
		}
	}
	// Installing twice is fine; a foreign hook is left alone.
	if code, out = r.scan(nil, "--install-hooks"); code != 0 {
		t.Fatalf("second install: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(r.dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out = r.scan(nil, "--install-hooks"); code != 2 {
		t.Fatalf("foreign hook overwritten: exit %d\n%s", code, out)
	}

	r.git("config", "core.hooksPath", filepath.Join(t.TempDir(), "hooks"))
	code, out = r.scan(nil, "--install-hooks")
	if code != 2 || !strings.Contains(out, "core.hooksPath") {
		t.Fatalf("hooks path not reported: exit %d\n%s", code, out)
	}
}

func TestUsage(t *testing.T) {
	r := newRepo(t)
	for _, args := range [][]string{{}, {"--range"}, {"--range", "nodots"}, {"--staged", "x"}, {"--bogus"}} {
		if code, out := r.scan(nil, args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
}
