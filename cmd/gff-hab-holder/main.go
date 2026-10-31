// Command gff-hab-holder owns the PTYs on one host. It spawns programs, keeps a raw byte ring
// per pane, and serves the holder protocol to gff-hab over a local Unix socket. See
// docs/specs/hab-holder.md.
//
// The socket path comes from -socket, or from -dir plus the instance id. -dir defaults to
// $GFF_HOLDER_DIR, then $XDG_RUNTIME_DIR/gff/holders, then a per-user directory under the
// system temporary directory.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/holder"
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
	logger := log.New(os.Stderr, "gff-hab-holder: ", log.LstdFlags)

	id := *instance
	if id == "" {
		id = holder.NewInstanceID()
	}
	path := *socket
	if path == "" {
		d := *dir
		if d == "" {
			d = defaultDir()
		}
		path = filepath.Join(d, id+".sock")
	}

	h := holder.New(holder.Config{
		Build:       build,
		Instance:    id,
		RingSize:    *ring,
		HangTimeout: *hang,
		Logf:        logger.Printf,
	})
	l, err := holder.Listen(path)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Printf("instance %s build %s listening on %s", id, build, path)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	// A holder must not die of a hang-up: that would end every program. Catch SIGHUP rather
	// than ignore it, because children inherit an ignored signal but not a caught one.
	hups := make(chan os.Signal, 1)
	signal.Notify(hups, syscall.SIGHUP)
	go func() {
		for range hups {
			logger.Print("SIGHUP ignored")
		}
	}()
	go func() {
		s := <-sigs
		logger.Printf("%v: shutting down", s)
		h.Shutdown()
	}()

	if err := h.Serve(l); err != nil {
		logger.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(30 * time.Second):
		logger.Print("exit did not finish in time")
		os.Exit(1)
	}
}

// defaultDir picks the per-user runtime directory for holder sockets.
func defaultDir() string {
	if d := os.Getenv("GFF_HOLDER_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "gff", "holders")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("gff-%d", os.Getuid()), "holders")
}
