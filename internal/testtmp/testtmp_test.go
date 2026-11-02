package testtmp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMainDir: a dead owner's directory goes, a live owner's and a foreign one stay, the new
// directory names this process, and Remove leaves nothing.
func TestMainDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	mk := func(name, pid string) string {
		d := filepath.Join(tmp, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if pid != "" {
			os.WriteFile(filepath.Join(d, pidFile), []byte(pid), 0o600)
		}
		return d
	}
	dead := mk("tt1", strconv.Itoa(cmd.Process.Pid))
	live := mk("tt2", strconv.Itoa(os.Getppid()))
	foreign := mk("tt3", "")
	other := mk("zz1", strconv.Itoa(cmd.Process.Pid))

	dir, err := MainDir("tt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(tmp, "tt")) {
		t.Fatalf("dir %s not under %s", dir, tmp)
	}
	for d, want := range map[string]bool{dead: false, live: true, foreign: true, other: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s: present %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, pidFile))
	if string(b) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file %q", b)
	}
	Sweep("tt") // this process's own directory stays
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "bin"), []byte("x"), 0o700)
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}
