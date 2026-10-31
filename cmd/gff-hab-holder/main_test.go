package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// TestMain runs the real main when the test binary is started as the holder.
func TestMain(m *testing.M) {
	if os.Getenv("GFF_HOLDER_TEST_MAIN") == "1" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...) // drop the -test.run guard
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestBinary runs the holder as its own process: connect, spawn, read output and the exit, then
// SIGTERM ends it cleanly and removes its socket.
func TestBinary(t *testing.T) {
	var stderr bytes.Buffer
	runBinary(t, &stderr)
}

// TestClosedStderr runs the holder with stderr a pipe whose reader is gone. Every log write
// then fails with EPIPE; the holder must live on and serve.
func TestClosedStderr(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	runBinary(t, w)
}

func runBinary(t *testing.T, stderr io.Writer) {
	dir, err := os.MkdirTemp("", "fdb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "h.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^$", "-socket", sock, "-ring", "65536")
	cmd.Env = append(os.Environ(), "GFF_HOLDER_TEST_MAIN=1")
	cmd.Stderr = stderr
	logs := func() string {
		if b, ok := stderr.(*bytes.Buffer); ok {
			return b.String()
		}
		return ""
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var nc net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		if nc, err = net.Dial("unix", sock); err == nil {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("holder exited before it listened: %v\n%s", werr, logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not listen: %v\n%s", err, logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	c := hp.NewConn(nc, hp.V1())
	w, err := c.Hello("test")
	if err != nil {
		t.Fatal(err)
	}
	if w.RingSize != 65536 || w.Build != "dev" || w.Pid != uint32(cmd.Process.Pid) {
		t.Fatalf("welcome %+v", w)
	}
	if err := c.WriteFrame(&hp.Spawn{Token: 1, Cols: 80, Rows: 24, Argv: []string{"/bin/sh", "-c", "echo hi; exit 4"},
		Env: []string{"PATH=/usr/bin:/bin"}}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("%v; output %q", err, out)
		}
		if o, ok := f.(*hp.Output); ok {
			out = append(out, o.Data...)
		}
		if e, ok := f.(*hp.Exited); ok {
			if e.Status.Code != 4 || string(out) != "hi\r\n" {
				t.Fatalf("exit %+v, output %q", e.Status, out)
			}
			break
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("no bye: %v", err)
		}
		if _, ok := f.(*hp.Bye); ok {
			break
		}
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("holder exit: %v\n%s", err, logs())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not exit")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestDefaultDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cache := func() (string, error) { return "/cache", nil }
	for _, tc := range []struct {
		goos string
		env  map[string]string
		want string
		err  error
	}{
		{"linux", map[string]string{"GFF_HOLDER_DIR": "/x", "XDG_RUNTIME_DIR": "/run/u"}, "/x", nil},
		{"linux", map[string]string{"XDG_RUNTIME_DIR": "/run/u"}, "/run/u/gff/holders", nil},
		{"linux", map[string]string{}, "", errNoRuntimeDir},
		{"darwin", map[string]string{"XDG_RUNTIME_DIR": "/run/u"}, "/cache/gff/holders", nil},
		{"darwin", map[string]string{"GFF_HOLDER_DIR": "/x"}, "/x", nil},
	} {
		got, err := defaultDir(tc.goos, env(tc.env), cache)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s %v: got %q, %v; want %q, %v", tc.goos, tc.env, got, err, tc.want, tc.err)
		}
	}
}
