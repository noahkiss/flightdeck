// Command gff-hab-holder owns the PTYs on one host. It spawns programs, keeps a raw byte ring
// per pane, and serves the holder protocol to gff-hab over a local Unix socket. See
// docs/specs/hab-holder.md.
//
// The socket path comes from -socket, or from -dir plus the instance id. -dir defaults to
// $GFF_HOLDER_DIR. Without it, macOS uses gff/holders under the user's cache directory, and
// other systems use $XDG_RUNTIME_DIR/gff/holders; with no XDG_RUNTIME_DIR the holder refuses to
// start rather than fall back to a shared directory.
//
// Without -socket, the holder also writes its registry file, <instance>.json, in that directory
// (spec 4.2), so gff-hab can find it. With -socket it writes none.
//
// Exit status 0 means a clean exit: a shutdown, the end of a drain, or a holder that was
// restarted after it crashed while draining (spec 6.7). The init system restarts only a
// failed holder.
package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/holder"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// build is set at link time: -ldflags "-X main.build=<id>".
var build = "dev"

func main() {
	var (
		dir      = flag.String("dir", "", "directory for the holder socket (default: see package doc)")
		socket   = flag.String("socket", "", "socket path; overrides -dir")
		instance = flag.String("instance", "", "instance id (default: random)")
		ring     = flag.Int("ring", holder.DefaultRingSize, "byte ring size per pane")
		hang     = flag.Duration("hang-timeout", holder.DefaultHangTimeout, "drop a connection whose writer makes no progress for this long")
	)
	flag.Parse()

	// A holder must not die of a hang-up, or of a write to a closed stderr: either would end
	// every program. Catch SIGHUP and SIGPIPE rather than ignore them, because children
	// inherit an ignored signal but not a caught one. This comes before the first log write.
	quiet := make(chan os.Signal, 1)
	signal.Notify(quiet, syscall.SIGHUP, syscall.SIGPIPE)
	logger := log.New(os.Stderr, "gff-hab-holder: ", log.LstdFlags)
	go func() {
		for s := range quiet {
			if s == syscall.SIGHUP {
				logger.Print("SIGHUP ignored")
			}
		}
	}()

	id := *instance
	if id == "" {
		id = holder.NewInstanceID()
	}
	if !holderreg.ValidInstance(id) {
		logger.Fatalf("bad instance id %q", id)
	}
	path := *socket
	var regDir string
	if path == "" {
		d := *dir
		if d == "" {
			var err error
			if d, err = defaultDir(runtime.GOOS, os.Getenv, os.UserConfigDir); err != nil {
				logger.Fatal(err)
			}
		}
		path = holderreg.SocketPath(d, id)
		regDir = d
	}

	h := holder.New(holder.Config{
		Build:       build,
		Instance:    id,
		RingSize:    *ring,
		HangTimeout: *hang,
		RegistryDir: regDir,
		Logf:        logger.Printf,
	})
	l, err := holder.Listen(path)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Printf("instance %s build %s listening on %s", id, build, path)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		logger.Printf("%v: shutting down", s)
		h.Shutdown()
	}()

	if err := h.Serve(l); err != nil {
		// Serve gives up only when no pane is left, so this ends no program.
		logger.Printf("accept: %v", err)
		h.Shutdown()
		os.Exit(1)
	}
	select {
	case <-h.Done():
	case <-time.After(30 * time.Second):
		logger.Print("exit did not finish in time")
		os.Exit(1)
	}
}

var errNoRuntimeDir = errors.New("no socket directory: set XDG_RUNTIME_DIR or GFF_HOLDER_DIR, or pass -dir or -socket")

// defaultDir picks the per-user directory for holder sockets (spec 4.2). On macOS it is under
// ~/Library/Application Support (os.UserConfigDir), not Caches, which storage cleanup may empty.
func defaultDir(goos string, getenv func(string) string, stateDir func() (string, error)) (string, error) {
	if d := getenv("GFF_HOLDER_DIR"); d != "" {
		return d, nil
	}
	if goos == "darwin" {
		d, err := stateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "gff", "holders"), nil
	}
	if d := getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "gff", "holders"), nil
	}
	return "", errNoRuntimeDir
}
