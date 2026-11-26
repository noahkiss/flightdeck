package main

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/devicestore"
	"github.com/noahkiss/flightdeck/internal/pairing"
)

// The devices.* methods (transport.md 5, 6.1.4, 6.1.7): the hab's device list, revoke, and a
// pairing window opened by a client the owner already uses. The window is the one `gff-hab pair`
// opens, with the same addresses, limits and key line; only the confirmation differs: the client
// that opened it takes the last characters of the device's key fingerprint, not a terminal.

// deviceConfig sets up the device service.
type deviceConfig struct {
	state    string // the state folder, which holds devices.json
	hostKey  ed25519.PrivateKey
	label    string // the hab's label in the code; "": the host name up to the first dot
	authKeys string // the system sshd's authorized_keys; "": ~/.ssh/authorized_keys
	command  string // the forced command of a key line
	user     string // the login the device uses over the system sshd
	sshPort  int
	sshdKeys string // folder with the system sshd's host keys
	// listen picks the window's addresses; nil: every LAN and tailnet address (6.1.3).
	listen   func() ([]netip.Addr, error)
	lifetime time.Duration // 0: pairing.MaxLifetime
	logf     func(string, ...any)
}

type deviceService struct {
	cfg   deviceConfig
	store *devicestore.Store
	ctx   context.Context // ends at Close: an open window closes
	stop  context.CancelFunc
	wg    sync.WaitGroup

	mu  sync.Mutex
	win *pairWindow // the window devices.pair opened last; nil before the first
}

// pairWindow is one window devices.pair opened, as devices.confirm sees it.
type pairWindow struct {
	owner    string // the client id that opened it: the only one that answers its device
	cancel   context.CancelFunc
	changed  chan struct{} // closed and made again at every change
	waiting  bool          // a device waits for the owner's answer
	answered bool
	answer   chan bool // the owner's answer to the waiting device
	code     string    // the last codeLen characters of the waiting device's fingerprint
	tries    int
	device   *api.PairingDevice
	declined bool // the owner declined, or closed the window with nothing waiting
	final    *api.DevicePairing
}

func newDeviceService(cfg deviceConfig) *deviceService {
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	ctx, stop := context.WithCancel(context.Background())
	return &deviceService{cfg: cfg, store: devicestore.Open(cfg.state), ctx: ctx, stop: stop}
}

// Close closes an open window and waits for it.
func (s *deviceService) Close() {
	s.stop()
	s.wg.Wait()
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
	s.cfg.logf("devices: revoked %s (%s); removed %d line(s) from %s", pairing.Printable(r.Label), r.ID, n, r.AuthorizedKeys)
	return nil
}

// Pair opens a window, unless one is open.
func (s *deviceService) Pair(client string, p api.DevicesPairParams) (api.PairingOffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return api.PairingOffer{}, control.Errorf(api.ErrorKindUnavailable, "the hab is stopping")
	}
	if s.win != nil && s.win.final == nil {
		return api.PairingOffer{}, control.Errorf(api.ErrorKindConflict, "a pairing window is open already; it closes after one device, a decline, or five minutes")
	}
	listen := s.cfg.listen
	if listen == nil {
		listen = func() ([]netip.Addr, error) { return pairAddrs(nil) }
	}
	addrs, err := listen()
	if err != nil {
		return api.PairingOffer{}, control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	label := p.Label
	if label == "" {
		label = s.cfg.label
	}
	if label == "" {
		h, _ := os.Hostname()
		label, _, _ = strings.Cut(h, ".")
	}
	port := s.cfg.sshPort
	if port == 0 {
		port = 22
	}
	var eps []string
	for _, a := range addrs {
		eps = append(eps, net.JoinHostPort(a.String(), strconv.Itoa(port)))
	}
	sshdKeys := s.cfg.sshdKeys
	if sshdKeys == "" {
		sshdKeys = "/etc/ssh"
	}
	authKeys := s.cfg.authKeys
	if authKeys == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return api.PairingOffer{}, err
		}
		authKeys = home + "/.ssh/authorized_keys"
	}
	pw := &pairWindow{owner: client, changed: make(chan struct{})}
	w, err := pairing.Open(pairing.Config{
		HostKey:  s.cfg.hostKey,
		Label:    label,
		Listen:   addrs,
		Lifetime: s.cfg.lifetime,
		Logf:     func(f string, a ...any) { s.cfg.logf("devices: pairing window: "+f, a...) },
		Confirm:  func(ctx context.Context, d pairing.Device) (bool, error) { return s.ask(pw, ctx, d) },
		Admit: admitter{store: s.store, authKeys: authKeys, command: s.cfg.command, user: s.cfg.user,
			endpoints: eps, hostKeys: sshdHostKeys(sshdKeys)}.admit,
	})
	if err != nil {
		return api.PairingOffer{}, control.Errorf(api.ErrorKindUnavailable, "the pairing window did not open: %v", err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	pw.cancel = cancel
	s.win = pw
	var wins []string
	for _, a := range w.Addrs() {
		wins = append(wins, a.String())
	}
	s.cfg.logf("devices: pairing window open for %s on %s, for client %s", w.Lifetime(), strings.Join(wins, ", "), client)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		dev, err := w.Wait(ctx)
		s.finish(pw, dev, err)
	}()
	o := w.Offer()
	return api.PairingOffer{DeviceID: o.DeviceID, Endpoints: o.Endpoints, Token: pairing.EncodeToken(o.Token), ExpiresInMS: int(w.Lifetime() / time.Millisecond)}, nil
}

// changedLocked wakes the devices.confirm calls that wait. The caller holds s.mu.
func (pw *pairWindow) changedLocked() {
	close(pw.changed)
	pw.changed = make(chan struct{})
}

// ask is the window's Confirm: it shows the device to devices.confirm and waits for the
// owner's answer, or the end of the window.
func (s *deviceService) ask(pw *pairWindow, ctx context.Context, d pairing.Device) (bool, error) {
	fp := d.Fingerprint
	s.mu.Lock()
	pw.waiting, pw.answered, pw.tries = true, false, 0
	pw.answer = make(chan bool, 1)
	pw.code = fp[len(fp)-codeLen:]
	pw.device = &api.PairingDevice{Name: d.Name, Kind: api.DeviceKind(d.Kind), KeyType: d.Key.Type(),
		Fingerprint: fp[:len(fp)-codeLen] + strings.Repeat("?", codeLen), From: hostOf(d.Remote)}
	answer := pw.answer
	pw.changedLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		pw.waiting = false
		pw.changedLocked()
		s.mu.Unlock()
	}()
	select {
	case yes := <-answer:
		return yes, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// finish records how the window ended.
func (s *deviceService) finish(pw *pairWindow, dev *pairing.Device, err error) {
	f := &api.DevicePairing{Device: pw.device}
	switch {
	case err == nil:
		f.State = api.PairingState("paired")
		if r, ferr := s.store.Find(dev.Fingerprint); ferr == nil {
			f.Paired = &api.Device{ID: r.ID, Label: r.Label, Kind: api.DeviceKind(r.Kind), State: api.DeviceState(r.State), PairedAt: r.PairedAt}
		}
		s.cfg.logf("devices: paired %s (%s)", pairing.Printable(dev.Name), dev.Fingerprint)
	case errors.Is(err, pairing.ErrDeclined):
		f.State, f.Reason = "declined", "declined; the window is closed and its token is spent"
	case errors.Is(err, pairing.ErrExpired):
		f.State, f.Reason = "expired", "the pairing window expired; no device paired"
	case errors.Is(err, context.Canceled):
		s.mu.Lock()
		declined := pw.declined
		s.mu.Unlock()
		if declined {
			f.State, f.Reason = "declined", "the window was closed before a device paired"
		} else {
			f.State, f.Reason = "closed", "the hab stopped; no device paired"
		}
	default:
		f.State, f.Reason = "failed", err.Error()
	}
	if f.State != "paired" {
		s.cfg.logf("devices: pairing window closed: %s", f.Reason)
	}
	s.mu.Lock()
	pw.final = f
	pw.changedLocked()
	s.mu.Unlock()
}

// Confirm waits for the device, or answers it (transport.md 6.1.7).
func (s *deviceService) Confirm(ctx context.Context, client string, p api.DevicesConfirmParams) (api.DevicePairing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pw := s.win
	if pw == nil {
		return api.DevicePairing{}, control.Errorf(api.ErrorKindNotFound, "no pairing window: devices.pair opens one")
	}
	if pw.owner != client {
		return api.DevicePairing{}, control.Errorf(api.ErrorKindDenied, "only the client that opened the pairing window answers its device")
	}
	for {
		switch {
		case pw.final != nil:
			return *pw.final, nil
		case p.Decline && pw.waiting && !pw.answered:
			pw.answered, pw.declined = true, true
			pw.answer <- false
		case p.Decline && !pw.waiting && !pw.declined:
			pw.declined = true
			pw.cancel()
		case pw.waiting && !pw.answered && p.Code == "" && !p.Decline:
			return api.DevicePairing{State: "waiting", Device: pw.device}, nil
		case pw.waiting && !pw.answered && p.Code != "":
			if subtle.ConstantTimeCompare([]byte(p.Code), []byte(pw.code)) == 1 {
				pw.answered = true
				pw.answer <- true
				break
			}
			pw.tries++
			if pw.tries < confirmTries {
				return api.DevicePairing{State: "waiting", Device: pw.device, TriesLeft: confirmTries - pw.tries,
					Reason: "that does not match the device's key; type the last 4 characters the device shows"}, nil
			}
			pw.answered, pw.declined = true, true
			pw.answer <- false
		}
		ch := pw.changed
		s.mu.Unlock()
		select {
		case <-ch:
			s.mu.Lock()
		case <-ctx.Done():
			s.mu.Lock()
			if pw.final != nil {
				return *pw.final, nil
			}
			return api.DevicePairing{}, control.Errorf(api.ErrorKindUnavailable, "no device presented the token yet; call devices.confirm again")
		}
	}
}
