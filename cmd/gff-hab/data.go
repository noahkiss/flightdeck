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
	"github.com/noahkiss/flightdeck/internal/panesize"
	"github.com/noahkiss/flightdeck/internal/structure"
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

func (l linkSource) Owner(pane string) string {
	u, ok := parseUUID(pane)
	if !ok {
		return ""
	}
	st, _ := l.lk.Pane(u)
	return st.Owner
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

// sizeService serves pane size ownership and redraw (spec 6) with internal/panesize.
type sizeService struct{ m *panesize.Manager }

func (s sizeService) Connected(client string)     { s.m.Connected(client) }
func (s sizeService) Disconnected(client string)  { s.m.Disconnected(client) }
func (s sizeService) Typed(client, pane string)   { s.m.Typed(client, pane) }
func (s sizeService) Created(client, pane string) { s.m.Created(client, pane) }

func (s sizeService) Take(client, pane string) (string, error) {
	o, err := s.m.Take(client, pane)
	return o, sizeErr(err)
}

func (s sizeService) Request(client, pane string) (string, error) {
	o, err := s.m.Request(client, pane)
	return o, sizeErr(err)
}

func (s sizeService) Disown(client, pane string) (string, error) {
	o, err := s.m.Disown(client, pane)
	return o, sizeErr(err)
}

func (s sizeService) SetDesired(client, pane string, cols, rows int) (bool, error) {
	ok, err := s.m.SetDesired(client, pane, cols, rows)
	return ok, sizeErr(err)
}

func (s sizeService) Redraw(client, pane string, mode api.RedrawMode) error {
	return sizeErr(s.m.Redraw(client, pane, mode))
}

func sizeErr(err error) error {
	var pe *panesize.Error
	if errors.As(err, &pe) {
		return control.Errorf(pe.Kind, "%s", pe.Hint)
	}
	return err
}

// newSizes wires the size owner manager to the link, the structure store and the data streams:
// the owner's size goes to the holder, each owner change tags the pane's resize frames and
// emits one pane.owner event.
func newSizes(lk *link.Link, store *structure.Store, ds *datastream.Server, logf func(string, ...any)) *panesize.Manager {
	src := linkSource{lk}
	return panesize.New(panesize.Config{
		Size: src.Size,
		Resize: func(ctx context.Context, pane string, cols, rows int) error {
			u, ok := parseUUID(pane)
			if !ok {
				return link.ErrNoPane
			}
			return lk.Resize(ctx, u, cols, rows)
		},
		Owner: func(pane string, o api.PaneOwner) {
			if u, ok := parseUUID(pane); ok {
				if err := lk.SetOwner(u, o.Owner, o.Reason); err != nil {
					logf("pane %s: owner: %v", pane, err)
				}
			}
			store.SetOwner(pane, o)
		},
		Observer: ds.Observer,
		Snapshot: func(client, pane string) { ds.Redraw(client, pane) },
		// A client's sizes and its claim to restore last as long as its reconnect token.
		Grace: control.DefaultReconnectGrace,
		Logf:  logf,
	})
}
