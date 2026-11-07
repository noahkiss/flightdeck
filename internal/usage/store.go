package usage

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Bounds on what the store keeps: a harness that reports nonsense cannot grow it.
const (
	maxAccounts = 64
	maxWindows  = 16
)

// Refresh is how often an unchanged account is sent again, so a client sees its reading age
// move on. A change is sent at once.
const Refresh = 5 * time.Minute

// Store keeps the latest reading of each account. It is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	accounts map[string]*account
	publish  func(structure.Event)
	now      func() time.Time
}

type account struct {
	acct api.UsageAccount
	sent api.UsageAccount // as last published
	at   time.Time        // when last published
	// markAt is the report time of the reading that last set or cleared the degraded mark.
	markAt time.Time
}

// NewStore returns an empty store. publish receives each usage.updated event, under the store
// lock; it must not block. now may be nil.
func NewStore(publish func(structure.Event), now func() time.Time) *Store {
	if publish == nil {
		publish = func(structure.Event) {}
	}
	if now == nil {
		now = time.Now
	}
	return &Store{accounts: map[string]*account{}, publish: publish, now: now}
}

// Put merges a reading into its account. Windows merge by name: a window replaces the stored
// one of the same name unless it was read earlier, so a reading from an older file never undoes
// a newer one. A window the reading lacks stays, with its reset time: a client shows it as reset
// with no reading since (app.md 14.5). A reading with no windows and nothing in Degrade creates
// no account: a harness with no usage source reports none, never zero. A reading with Degrade
// marks its account degraded, and creates it if need be, so a client says "usage unavailable"
// rather than "no limits". A reading with windows and an empty Degrade clears the mark; one
// with neither (no token_count yet, no rate_limits) leaves it. Like a window, the mark follows
// report time: a reading older (by At) than the one that last set or cleared it leaves it; a
// reading with no At counts as new. It reports whether it published an event.
func (s *Store) Put(r Reading) bool {
	if r.Key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[r.Key]
	if a == nil {
		if (len(r.Windows) == 0 && len(r.Degrade) == 0) || len(s.accounts) >= maxAccounts {
			return false
		}
		a = &account{acct: api.UsageAccount{Key: r.Key, Harness: r.Harness, Windows: []api.UsageWindow{}}}
		s.accounts[r.Key] = a
	}
	newest := false
	for _, w := range r.Windows {
		i := slices.IndexFunc(a.acct.Windows, func(o api.UsageWindow) bool { return o.Name == w.Name })
		switch {
		case i < 0 && len(a.acct.Windows) < maxWindows:
			a.acct.Windows = append(a.acct.Windows, w)
			newest = true
		case i >= 0 && !before(w.ReadAt, a.acct.Windows[i].ReadAt):
			a.acct.Windows[i] = w
			newest = true
		}
	}
	if newest && r.Label != "" {
		a.acct.Label = r.Label
	}
	// An empty clean read says nothing either way; an older report never moves the mark.
	if (len(r.Degrade) > 0 || len(r.Windows) > 0) && (r.At.IsZero() || !r.At.Before(a.markAt)) {
		a.acct.Degraded = degraded(r.Degrade)
		if r.At.After(a.markAt) {
			a.markAt = r.At
		}
	}
	now := s.now()
	if same(a.acct, a.sent) && now.Sub(a.at) < Refresh {
		return false
	}
	a.sent = clone(a.acct)
	a.at = now
	s.publish(structure.Event{Type: api.EventUsageUpdated, ID: r.Key, Data: clone(a.acct)})
	return true
}

// List returns every account, by key.
func (s *Store) List() api.UsageReadings {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.UsageReadings{Accounts: make([]api.UsageAccount, 0, len(s.accounts))}
	for _, a := range s.accounts {
		out.Accounts = append(out.Accounts, clone(a.acct))
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Key < out.Accounts[j].Key })
	return out
}

// Get returns one account by key.
func (s *Store) Get(key string) (api.UsageAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[key]
	if a == nil {
		return api.UsageAccount{}, false
	}
	return clone(a.acct), true
}

// before reports whether RFC 3339 time a is earlier than b. A time that does not parse is
// never earlier.
func before(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339, a)
	tb, err2 := time.Parse(time.RFC3339, b)
	return err1 == nil && err2 == nil && ta.Before(tb)
}

// same reports whether two readings differ only in when windows were read.
func same(a, b api.UsageAccount) bool {
	if a.Key != b.Key || a.Label != b.Label || a.Harness != b.Harness || a.Degraded != b.Degraded ||
		len(a.Windows) != len(b.Windows) {
		return false
	}
	for i := range a.Windows {
		x, y := a.Windows[i], b.Windows[i]
		if x.Name != y.Name || x.UsedPct != y.UsedPct || x.ResetsAt != y.ResetsAt ||
			x.LengthS != y.LengthS || x.StartsAt != y.StartsAt {
			return false
		}
	}
	return true
}

// degraded is an account's degraded mark for a reading's drift: the first line, and how many
// more there are. Empty with no drift.
func degraded(drift []string) string {
	switch len(drift) {
	case 0:
		return ""
	case 1:
		return drift[0]
	}
	return fmt.Sprintf("%s (and %d more)", drift[0], len(drift)-1)
}

func clone(a api.UsageAccount) api.UsageAccount {
	a.Windows = slices.Clone(a.Windows)
	if a.Windows == nil {
		a.Windows = []api.UsageWindow{}
	}
	return a
}
