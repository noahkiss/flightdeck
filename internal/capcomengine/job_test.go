package capcomengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ProcessJob: the process runs in its own folder with the given environment and log; a later
// gff-hab adopts it by its pid file and sees it end.
func TestProcessJob(t *testing.T) {
	dir := t.TempDir()
	j := ProcessJob{PidFile: filepath.Join(dir, PidFile)}
	log := filepath.Join(dir, LogFile)
	p, err := j.Start(context.Background(), JobSpec{Argv: []string{"/bin/sh", "-c", `echo "dir $(pwd) var $FD_TEST"; echo last line >&2; exec sleep 60`},
		Dir: dir, Env: []string{"FD_TEST=yes", "PATH=/usr/bin:/bin"}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(time.Second)
	waitFor(t, "the log", func() bool { return LastLine(log) == "last line" })
	b, _ := os.ReadFile(log)
	real, _ := filepath.EvalSymlinks(dir)
	if want := "dir " + real + " var yes\n"; string(b[:len(want)]) != want {
		t.Fatalf("log %q", b)
	}
	a, ok := j.Adopt(context.Background())
	if !ok || a.Pid() != p.Pid() {
		t.Fatalf("adopt: %v %v", ok, a)
	}
	if err := p.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the adopted process did not end")
	}
	if _, ok := j.Adopt(context.Background()); ok {
		t.Fatal("adopted a process that ended")
	}
}
