package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// sleepGapMin is the least gap between two rounds that counts as a sleep of this host: after
// one, the always-on clock starts again (mesh.md 8).
const sleepGapMin = 30 * time.Second

// maxBackoff is the longest a link whose catalog calls fail waits before it tries again.
const maxBackoff = 5 * time.Minute

// failure is a link whose last exchange failed: it is skipped until next, and each failure in
// a row doubles the wait, from one poll period up to maxBackoff.
type failure struct {
	n    int
	wait time.Duration
	next time.Time
}

// sync runs a pull and put round every Pull, and at once on Kick.
func (s *Service) sync() {
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	t := time.NewTicker(s.cfg.Pull)
	defer t.Stop()
	for {
		s.round(ctx)
		select {
		case <-s.done:
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// round puts and pulls over every online link, drops the copies of habs that left the peer
// list, and checks the always-on hosts.
func (s *Service) round(ctx context.Context) {
	if s.cfg.Peers == nil {
		return
	}
	s.mu.Lock()
	stores := s.stores
	s.mu.Unlock()
	links := s.cfg.Peers.Links()
	now := s.now()
	var wg sync.WaitGroup
	for _, l := range links {
		if !l.Online || l.Caller == nil || !slices.Contains(l.Caps, Capability) {
			s.mu.Lock()
			delete(s.put, l.Label) // a link that comes back gets the catalog again
			delete(s.fails, l.Label)
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		f := s.fails[l.Label]
		wait := f != nil && now.Before(f.next)
		s.mu.Unlock()
		if wait {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.exchange(ctx, l)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	pl := s.cfg.Peers.List()
	s.prune(pl, links, stores)
	s.checkAlwaysOn(pl)
}

// exchange hands this hab's catalog to one peer when the peer lacks this generation, then
// pulls the peer's catalog since the generation held.
func (s *Service) exchange(ctx context.Context, l Link) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cl := api.Client{Caller: l.Caller}
	s.mu.Lock()
	gen, own := s.gen, s.own
	sent := s.put[l.Label]
	// since is the generation this hab last pulled itself, never one a catalog.put handed over:
	// a put can be a claim, and a pull over this hab's own link always replaces it.
	since := s.pulled[l.DeviceID]
	s.mu.Unlock()
	if sent != gen {
		if _, err := cl.CatalogPut(ctx, api.CatalogPutParams{Catalog: own}); err != nil {
			s.failed(l, "put to", err)
			return
		}
		s.mu.Lock()
		s.put[l.Label] = gen
		s.mu.Unlock()
	}
	res, err := cl.CatalogGet(ctx, api.CatalogGetParams{Since: since})
	if err == nil && res.Catalog != nil {
		err = s.checkPulled(l, *res.Catalog)
	}
	if err != nil {
		s.failed(l, "pull from", err)
		return
	}
	s.mu.Lock()
	if f := s.fails[l.Label]; f != nil {
		s.cfg.Logf("catalog: %s answers again", l.Label)
		delete(s.fails, l.Label)
	}
	s.mu.Unlock()
	if res.Catalog == nil {
		s.seen(l.DeviceID)
		return
	}
	s.store(*res.Catalog)
	s.mu.Lock()
	s.pulled[l.DeviceID] = res.Catalog.Generation
	// The peer changed, perhaps by a restart that lost its copy of this hab's catalog: put again
	// next round.
	delete(s.put, l.Label)
	s.mu.Unlock()
}

// failed records a failed exchange: the link waits one poll period, doubled for each failure
// in a row up to maxBackoff, and only the first failure in a row and each longer wait are logged.
func (s *Service) failed(l Link, what string, err error) {
	s.mu.Lock()
	f := s.fails[l.Label]
	if f == nil {
		f = &failure{}
		s.fails[l.Label] = f
	}
	f.n++
	wait := min(s.cfg.Pull<<min(f.n-1, 16), maxBackoff)
	logIt := wait != f.wait
	f.wait, f.next = wait, s.now().Add(wait)
	s.mu.Unlock()
	if logIt {
		s.cfg.Logf("catalog: %s %s: %v; next try in %v", what, l.Label, err, wait)
	}
}

// checkPulled refuses a catalog that is not the linked hab's own.
func (s *Service) checkPulled(l Link, c api.Catalog) error {
	switch {
	case l.DeviceID == "":
		return errors.New("the link has no device id")
	case c.DeviceID == "":
		return errors.New("a catalog with no device id")
	case c.DeviceID == s.cfg.Self:
		return errors.New("the peer answered with this hab's own catalog")
	case c.DeviceID != l.DeviceID:
		return fmt.Errorf("the catalog is device %s, the link is %s", c.DeviceID, l.DeviceID)
	}
	return nil
}

// prune drops the copies of habs that are neither in the peer list nor linked to this hab
// (mesh.md 5.2: dropped only when the user removes the peer). pl and links are read during the
// round, so a copy stored after stores (a link that came up meanwhile handed it over) stays
// until a round that saw that link.
func (s *Service) prune(pl api.PeerList, links []Link, stores uint64) {
	keep := map[string]bool{}
	for _, p := range pl.Peers {
		keep[p.DeviceID] = true
	}
	for _, u := range pl.Unlisted {
		keep[u.DeviceID] = true
	}
	for _, l := range links {
		keep[l.DeviceID] = true
	}
	var gone []string
	s.mu.Lock()
	for id := range s.copies {
		if !keep[id] && s.copies[id].stored <= stores {
			delete(s.copies, id)
			delete(s.alerted, id)
			delete(s.pulled, id)
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		s.saveLocked()
	}
	s.mu.Unlock()
	for _, id := range gone {
		s.cfg.Logf("catalog: dropped the copy of %s: not in the peer list", id)
		if s.cfg.Publish != nil {
			s.cfg.Publish(api.CatalogUpdated{DeviceID: id})
		}
	}
	if len(gone) > 0 {
		s.Poke() // the hosts this hab watches may have changed
	}
}

// peerRow is a peer's state for the always-on check.
type peerRow struct {
	label    string
	online   bool
	lastSeen time.Time
}

// checkAlwaysOn raises one notification per outage of a peer marked always on (mesh.md 8): once
// it has been offline for AlertAfter while this hab was up the whole time, and only when this
// hab has the lowest device id among the online habs that watch the marked one. Every time here
// is wall time: a sleep stops the monotonic clock, and a gap measured with it would hide the
// sleep.
func (s *Service) checkAlwaysOn(pl api.PeerList) {
	now := s.now()
	rows := map[string]peerRow{}
	for _, p := range pl.Peers {
		if p.DeviceID == "" {
			continue
		}
		r := peerRow{label: p.Label, online: p.State == api.PeerStateOnline}
		if t, err := time.Parse(time.RFC3339, p.LastSeen); err == nil {
			r.lastSeen = t
		}
		rows[p.DeviceID] = r
	}
	for _, u := range pl.Unlisted {
		rows[u.DeviceID] = peerRow{online: true}
	}
	var raise []api.NoticeRaiseParams
	s.mu.Lock()
	gap := max(3*s.cfg.Pull, sleepGapMin)
	if !s.lastTick.IsZero() && now.Sub(s.lastTick) > gap {
		s.watchFrom = now // this host slept: it saw nothing meanwhile
	}
	s.lastTick = now
	for id, c := range s.copies {
		if !c.Catalog.AlwaysOn {
			continue
		}
		r, listed := rows[id]
		if r.online {
			delete(s.alerted, id) // back online: the next outage alerts again
			continue
		}
		if !listed || s.alerted[id] || !slices.Contains(s.own.Watches, id) {
			continue
		}
		from := s.watchFrom
		if r.lastSeen.After(from) {
			from = r.lastSeen
		}
		if now.Sub(from) < s.declared.AlertAfter || !s.lowestLocked(id, rows) {
			continue
		}
		s.alerted[id] = true
		name := r.label
		if name == "" {
			name = c.Catalog.Host
		}
		raise = append(raise, api.NoticeRaiseParams{
			Title: name + " is offline",
			Body:  fmt.Sprintf("Marked always on; offline for %s", roundDuration(now.Sub(from))),
			Level: api.NoticeLevelActive,
		})
	}
	s.mu.Unlock()
	for _, n := range raise {
		s.cfg.Logf("catalog: %s", n.Title)
		if s.cfg.Notify != nil {
			s.cfg.Notify(n)
		}
	}
}

// lowestLocked reports whether this hab has the lowest device id among the online habs that
// watch marked, as their copies say. A hab that lists marked but holds no copy with the mark
// cannot alert, so it does not count. The caller holds s.mu.
func (s *Service) lowestLocked(marked string, rows map[string]peerRow) bool {
	for id, c := range s.copies {
		if id == marked || id >= s.cfg.Self || !rows[id].online {
			continue
		}
		if slices.Contains(c.Catalog.Watches, marked) {
			return false
		}
	}
	return true
}

// roundDuration is a duration in whole minutes, or seconds under one minute.
func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}
