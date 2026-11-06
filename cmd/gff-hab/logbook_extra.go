package main

import (
	"context"
	"math"
	"os"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/lbextra"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/peers"
)

// vectorsCapability is the welcome word of a hab with a vector index (logbook.md 13).
const vectorsCapability = "logbook-vectors"

// vectorSyncEvery is how often the vector index embeds what changed. $GFF_TEST_VECTOR_SYNC
// shortens it for tests.
var vectorSyncEvery = 30 * time.Second

func init() {
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_VECTOR_SYNC")); err == nil && d > 0 {
		vectorSyncEvery = d
	}
}

// logbookSemantic is how this hab answers logbook.find with semantic (13): its own index when
// [logbook.vectors] names an embeddings server, else a forward to an online peer whose welcome
// lists logbook-vectors. The config is read once, at start.
type logbookSemantic struct {
	index *lbextra.Vectors
	links func() []peers.Link
}

func newLogbookSemantic(cpath, state string, pm *peers.Manager, logf func(string, ...any)) (*logbookSemantic, error) {
	cfg, err := lbextra.LoadConfig(cpath, state)
	if err != nil {
		return nil, err
	}
	ls := &logbookSemantic{}
	if pm != nil {
		ls.links = pm.Links
	}
	if cfg.VectorsOn() {
		ls.index = &lbextra.Vectors{Engine: lbextra.HTTPEngine{URL: cfg.Vectors.URL, Model: cfg.Vectors.Model}, Path: cfg.Vectors.Index}
		logf("logbook: vector search on, index %s", cfg.Vectors.Index)
	}
	return ls, nil
}

// options fills the service's semantic hooks.
func (ls *logbookSemantic) options(c *lbsvc.Config) {
	if ls.index != nil {
		c.Vectors = ls.index.Search
	}
	c.Forward = ls.forward
}

// forward asks the first online peer with vector search, in peer file order.
func (ls *logbookSemantic) forward(ctx context.Context, p api.LogbookFindParams) (api.LogbookRecords, bool, error) {
	if ls.links == nil {
		return api.LogbookRecords{}, false, nil
	}
	for _, l := range ls.links() {
		if !l.Online || l.Conn == nil || !slices.Contains(l.Caps, vectorsCapability) {
			continue
		}
		var res api.LogbookRecords
		if err := l.Conn.Call(ctx, "logbook.find", p, &res); err != nil {
			return api.LogbookRecords{}, false, err
		}
		return res, true, nil
	}
	return api.LogbookRecords{}, false, nil
}

// run keeps the index up to date until ctx ends.
func (ls *logbookSemantic) run(ctx context.Context, st *logbook.Store, logf func(string, ...any)) {
	if ls.index == nil {
		return
	}
	t := time.NewTicker(vectorSyncEvery)
	defer t.Stop()
	last := ""
	for {
		if _, err := ls.index.Sync(ctx, st); err != nil && ctx.Err() == nil {
			if err.Error() != last {
				logf("logbook vectors: %v", err)
				last = err.Error()
			}
		} else {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// catalogAlwaysOn reads this hab's always-on mark from its own catalog (mesh.md 8).
func catalogAlwaysOn(cs *catalog.Service) func() bool {
	return func() bool {
		c := cs.Get(math.MaxUint64).Catalog
		return c != nil && c.AlwaysOn
	}
}
