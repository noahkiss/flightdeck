package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// stuckDetect is a Detect whose Explain ignores its context and returns only when released.
type stuckDetect struct {
	release chan struct{}
	called  chan struct{}
}

func (d *stuckDetect) Explain(ctx context.Context, p api.DetectExplainParams) (api.DetectExplain, error) {
	close(d.called)
	<-d.release
	return api.DetectExplain{Pane: p.Pane}, nil
}

// TestExplainOverrun: an Explain that ignores its context still gets the caller an answer, an
// unavailable error that names the limit, within maxExplainTime and the grace. The read loop
// serves other calls meanwhile.
func TestExplainOverrun(t *testing.T) {
	oldMax, oldGrace := maxExplainTime, explainGrace
	maxExplainTime, explainGrace = 150*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { maxExplainTime, explainGrace = oldMax, oldGrace })

	d := &stuckDetect{release: make(chan struct{}), called: make(chan struct{})}
	t.Cleanup(func() { close(d.release) })
	cfg := testConfig(&fakeSource{})
	cfg.Detect = d
	srv := New(cfg)
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	t.Cleanup(func() { c.Close() })
	hello(t, c, "")

	cx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		errc <- c.Call(cx, "detect.explain", api.DetectExplainParams{Pane: "00000000-0000-0000-0000-000000000001"}, nil)
	}()
	<-d.called
	if err := c.Call(cx, "hab.ping", nil, nil); err != nil {
		t.Fatalf("a ping while explain is stuck: %v", err)
	}
	err := <-errc
	elapsed := time.Since(start)
	var ce *Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable || !strings.Contains(ce.Message, "did not finish within") {
		t.Fatalf("got %v; want an unavailable error that names the limit", err)
	}
	if limit := maxExplainTime + explainGrace + 2*time.Second; elapsed > limit {
		t.Errorf("answered after %v; want within %v", elapsed, limit)
	}
}
