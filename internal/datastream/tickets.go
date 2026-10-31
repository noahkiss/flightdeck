package datastream

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// TicketTTL is how long a data ticket stays valid (spec 5.2).
const TicketTTL = 30 * time.Second

// Ticket is what a data ticket grants: one data connection to one pane, for one client.
type Ticket struct {
	Pane    string
	Client  string // the control connection's client id
	Observe bool   // an observer stream (spec 6.4)
	expires time.Time
}

// Tickets issues and redeems data tickets. A ticket is single use and expires after TicketTTL.
type Tickets struct {
	now func() time.Time

	mu sync.Mutex
	m  map[string]Ticket
}

// NewTickets returns an empty ticket store. now may be nil.
func NewTickets(now func() time.Time) *Tickets {
	if now == nil {
		now = time.Now
	}
	return &Tickets{now: now, m: map[string]Ticket{}}
}

// Issue makes a ticket for client to open one data connection to pane.
func (t *Tickets) Issue(client, pane string, observe bool) (string, time.Duration) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	tok := hex.EncodeToString(b)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.m {
		if !now.Before(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[tok] = Ticket{Pane: pane, Client: client, Observe: observe, expires: now.Add(TicketTTL)}
	return tok, TicketTTL
}

// Redeem takes a ticket. It works once, and only before the ticket expires.
func (t *Tickets) Redeem(tok string) (Ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[tok]
	if !ok {
		return Ticket{}, false
	}
	delete(t.m, tok)
	if !t.now().Before(v.expires) {
		return Ticket{}, false
	}
	return v, true
}
