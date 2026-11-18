package flightrules

import (
	"context"
	"sync"

	"github.com/noahkiss/flightdeck/api"
)

// Session is one session's ledger and the source that feeds it. It is safe for concurrent use.
type Session struct {
	mu  sync.Mutex
	src Source
	l   *Ledger
}

// NewSession returns a session ledger fed by src. configured may be nil.
func NewSession(src Source, configured func(path, text string) []string) *Session {
	l := NewLedger()
	l.Configured = configured
	return &Session{src: src, l: l}
}

// Harness names the source's harness.
func (s *Session) Harness() string { return s.src.Harness() }

// List reads what the harness recorded since the last call, then returns the ledger. A read
// that fails part way keeps what it read; the error says why the rest is missing.
func (s *Session) List(ctx context.Context) (entries []api.FlightRule, warnings []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.src.Update(ctx, s.l)
	return s.l.Entries(), s.l.Warnings(), err
}

// Text returns the text of one entry, cut at MaxText, and its whole size. An id the ledger does
// not hold, or a placeholder, is ErrNoEntry. The source reads without the ledger's lock, so a
// List goes on meanwhile.
func (s *Session) Text(ctx context.Context, id string) (text string, size int64, truncated bool, err error) {
	s.mu.Lock()
	known := s.l.HasText(id)
	s.mu.Unlock()
	if !known {
		return "", 0, false, ErrNoEntry
	}
	full, err := s.src.Text(ctx, id)
	if err != nil {
		return "", 0, false, err
	}
	text, truncated = Truncate(full, MaxText)
	return text, int64(len(full)), truncated, nil
}
