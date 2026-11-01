package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
	"github.com/noahkiss/flightdeck/internal/link"
)

// linkSource is the holder link as the data streams see it.
type linkSource struct{ lk *link.Link }

type linkSub struct{ s *link.Subscription }

func (s linkSub) Chunks() <-chan link.Chunk { return s.s.C }
func (s linkSub) Err() error                { return s.s.Err() }
func (s linkSub) Close()                    { s.s.Close() }

func (l linkSource) Subscribe(ctx context.Context, pane string, from uint64) (datastream.Sub, error) {
	u, ok := parseUUID(pane)
	if !ok {
		return nil, link.ErrNoPane
	}
	s, err := l.lk.Subscribe(ctx, u, from)
	if err != nil {
		return nil, err
	}
	return linkSub{s}, nil
}

func (l linkSource) Size(pane string) (int, int, bool) {
	u, ok := parseUUID(pane)
	if !ok {
		return 0, 0, false
	}
	st, ok := l.lk.Pane(u)
	if !ok || st.Gone {
		return 0, 0, false
	}
	return st.Cols, st.Rows, true
}

// dataService serves data connections for the control socket (spec 5).
type dataService struct{ ds *datastream.Server }

func (d dataService) Ticket(client, pane string, observe bool) (string, time.Duration, error) {
	tok, ttl, err := d.ds.Ticket(client, pane, observe)
	var re *datastream.RefusedError
	if errors.As(err, &re) {
		return "", 0, control.Errorf(re.Kind, "%s", re.Hint)
	}
	return tok, ttl, err
}

func (d dataService) ServeConn(nc net.Conn, br *bufio.Reader) { d.ds.ServeConn(nc, br) }

// inputService writes pane.input to a pane through the link (spec 3.8).
type inputService struct{ lk *link.Link }

func (i inputService) QueueInput(ctx context.Context, _ string, pane string, data []byte) func() error {
	u, ok := parseUUID(pane)
	if !ok {
		return func() error { return control.Errorf(api.ErrorKindInvalid, "pane %q is not a pane id", pane) }
	}
	wait := i.lk.QueueInput(ctx, u, data)
	return func() error { return inputErr(pane, wait()) }
}

// inputErr gives a link input error its kind.
func inputErr(pane string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, link.ErrNoPane), errors.Is(err, link.ErrGone):
		return control.Errorf(api.ErrorKindNotFound, "pane %s: %v", pane, err)
	}
	return control.Errorf(api.ErrorKindUnavailable, "pane %s: %v", pane, err)
}
