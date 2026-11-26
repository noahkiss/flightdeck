package main

import (
	"strings"
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
	r, n, err := s.store.Revoke(query, time.Now())
	if err != nil {
		if strings.HasPrefix(err.Error(), "no paired device") {
			return control.Errorf(api.ErrorKindNotFound, "%v", err)
		}
		if strings.Contains(err.Error(), " matches ") {
			return control.Errorf(api.ErrorKindConflict, "%v", err)
		}
		return err
	}
	s.logf("devices: revoked %s (%s); removed %d line(s) from %s", pairing.Printable(r.Label), r.ID, n, r.AuthorizedKeys)
	return nil
}
