package version

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRequested(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"version"}, true},
		{[]string{"-version"}, true},
		{[]string{"--version"}, true},
		{[]string{"serve", "version"}, false},
		{[]string{"-v"}, false},
		{[]string{"versions"}, false},
	} {
		if got := Requested(c.args); got != c.want {
			t.Errorf("Requested(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestPrint(t *testing.T) {
	var b bytes.Buffer
	Print(&b, "gff", "1.2.3-rc.1")
	if b.String() != "gff 1.2.3-rc.1\n" {
		t.Fatalf("Print wrote %q", b.String())
	}
}

// TestLinkedVersion builds each command the way a release does, with the version set at link
// time, and checks that every form of the request prints it. A wrong -X target links silently
// and leaves "dev", so only a real build proves the variable is there.
func TestLinkedVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three commands")
	}
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	dir := t.TempDir()
	const want = "1.2.3-rc.4"
	cmds := []string{"gff", "gff-hab", "gff-hab-holder"}
	args := []string{"build", "-trimpath", "-ldflags", "-s -w -X main.version=" + want + " -X main.build=test", "-o", dir + string(filepath.Separator)}
	for _, c := range cmds {
		args = append(args, "./cmd/"+c)
	}
	build := exec.Command("go", args...)
	build.Dir = repo
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, c := range cmds {
		for _, form := range []string{"version", "-version", "--version"} {
			out, err := exec.Command(filepath.Join(dir, c), form).CombinedOutput()
			if err != nil {
				t.Fatalf("%s %s: %v\n%s", c, form, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != c+" "+want {
				t.Errorf("%s %s printed %q, want %q", c, form, got, c+" "+want)
			}
		}
	}
}
