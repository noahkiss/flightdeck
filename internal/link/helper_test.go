package link

// The link process for the end-to-end tests. TestMain runs it in place of the tests when
// GFF_LINK_HELPER is set, so each gff-hab restart is a real process that can be stopped by plan
// or killed. It takes one JSON command per line on stdin and writes one JSON event per line on
// stdout.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

type helperCmd struct {
	Cmd  string   `json:"cmd"`
	Pane hp.UUID  `json:"pane"`
	Argv []string `json:"argv,omitempty"`
	Env  []string `json:"env,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	From uint64   `json:"from,omitempty"`
	Sub  int      `json:"sub,omitempty"` // names a subscription in its chunk events
}

type helperEvent struct {
	Ev       string                 `json:"ev"`
	Sub      int                    `json:"sub,omitempty"`
	Err      string                 `json:"err,omitempty"`
	Previous map[string]hp.Previous `json:"previous,omitempty"`
	Pane     *PaneStatus            `json:"pane,omitempty"`
	Panes    []PaneStatus           `json:"panes,omitempty"`
	Screen   *Screen                `json:"screen,omitempty"`
	Chunk    *Chunk                 `json:"chunk,omitempty"`
}

// testScrollback is the scrollback cap of every parser in the tests. libghostty's default is
// about 10 kB, and near the cap a restored terminal drops pages at other points than one that
// parsed the whole stream, so the restart loop's screens would differ for that reason alone.
const testScrollback = 16 << 20

func envDuration(name string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return 0
	}
	return d
}

func helperMain() int {
	logger := log.New(os.Stderr, "link-helper: ", log.Lmicroseconds)
	var outMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(ev helperEvent) {
		outMu.Lock()
		defer outMu.Unlock()
		if err := enc.Encode(ev); err != nil {
			logger.Fatal(err)
		}
	}
	l, err := Open(Config{
		HolderDir:       os.Getenv("GFF_LINK_HOLDERS"),
		StateDir:        os.Getenv("GFF_LINK_STATE"),
		Build:           "test",
		Parser:          testParser(),
		IdleAfter:       envDuration("GFF_LINK_IDLE"),
		CheckpointEvery: envDuration("GFF_LINK_EVERY"),
		Rescan:          100 * time.Millisecond,
		KeepLost:        envDuration("GFF_LINK_KEEPLOST"),
		ScrollbackBytes: testScrollback,
		Logf:            logger.Printf,
	})
	if err != nil {
		logger.Print(err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = l.Ready(ctx)
	cancel()
	if err != nil {
		logger.Print(err)
		return 1
	}
	emit(helperEvent{Ev: "ready", Previous: l.Previous(), Panes: l.Panes()})

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var c helperCmd
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			logger.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		reply := helperEvent{Ev: c.Cmd}
		switch c.Cmd {
		case "spawn":
			st, err := l.Spawn(ctx, hp.Spawn{UUID: c.Pane, Cols: c.Cols, Rows: c.Rows, Cwd: "/", Argv: c.Argv, Env: c.Env})
			reply.Pane = &st
			if err != nil {
				reply.Err = err.Error()
			}
		case "panes":
			reply.Panes = l.Panes()
		case "screen":
			s, err := l.Screen(c.Pane)
			reply.Screen = &s
			if err != nil {
				reply.Err = err.Error()
			}
		case "checkpoint":
			if err := l.Checkpoint(); err != nil {
				reply.Err = err.Error()
			}
		case "sub":
			s, err := l.Subscribe(ctx, c.Pane, c.From)
			if err != nil {
				reply.Err = err.Error()
				break
			}
			go func(id int) {
				for ch := range s.C {
					emit(helperEvent{Ev: "chunk", Sub: id, Chunk: &ch})
				}
				emit(helperEvent{Ev: "subend", Sub: id, Err: fmt.Sprint(s.Err())})
			}(c.Sub)
		case "stop":
			if err := l.Close(); err != nil {
				reply.Err = err.Error()
			}
			emit(reply)
			cancel()
			return 0
		default:
			reply.Err = "unknown command"
		}
		cancel()
		emit(reply)
	}
	return 0
}
