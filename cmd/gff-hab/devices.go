package main

import (
	"errors"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/pairing"
)

// The devices.* methods (transport.md 5, 6.1.4, 6.6): the hab's device list and revoke. Pairing
// over the API is not built: `gff-hab pair` pairs a device on the host.

// deviceService answers devices.list and devices.revoke from the device list in the state folder.
type deviceService struct {
	mu    sync.Mutex // one change at a time in this hab; the store's compare-and-swap covers the CLI
	store *devicestore.Store
	logf  func(string, ...any)
}

func newDeviceService(state string, logf func(string, ...any)) *deviceService {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &deviceService{store: devicestore.Open(state), logf: logf}
}

func (s *deviceService) List() ([]api.Device, error) {
	recs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	out := []api.Device{}
	for _, r := range recs {
		out = append(out, api.Device{ID: r.ID, Label: r.Label, Kind: api.DeviceKind(r.Kind), State: api.DeviceState(r.State), PairedAt: r.PairedAt})
	}
	return out, nil
}

func (s *deviceService) Revoke(query string) error {
	s.mu.Lock()
	r, n, err := s.store.Revoke(query, time.Now())
	s.mu.Unlock()
	switch {
	case errors.Is(err, devicestore.ErrNotFound):
		return control.Errorf(api.ErrorKindNotFound, "%v", err)
	case errors.Is(err, devicestore.ErrAmbiguous):
		return control.Errorf(api.ErrorKindConflict, "%v", err)
	case err != nil:
		return err
	}
	s.logf("devices: revoked %s (%s); removed %d line(s) from %s; its push registration is kept", pairing.Printable(r.Label), r.ID, n, r.AuthorizedKeys)
	return nil
}
