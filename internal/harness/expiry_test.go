package harness

import (
	"errors"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestRequestExpiry: with approval.max_age set, a request carries expires_at and takes an answer
// until then, and none after (app.md 7.4); a change applies to the next request; with no limit
// the request carries none and never expires.
func TestRequestExpiry(t *testing.T) {
	clock := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	maxAge := time.Minute
	s := NewStore(Config{Publish: func(structure.Event) {}, Now: func() time.Time { return clock }})
	s.SetMaxAge(func() time.Duration { return maxAge })

	req := claudeTurn(t, s)
	st, _ := s.Get("p")
	if st.Request == nil || st.Request.ExpiresAt != "2026-10-31T12:01:01Z" {
		t.Fatalf("request: %+v", st.Request)
	}
	maxAge = time.Hour // the open request keeps its limit
	clock = clock.Add(60 * time.Second)
	if _, err := s.BeginAnswer("p", req); err != nil {
		t.Fatalf("answer within the limit: %v", err)
	}
	s.EndAnswer("p", req, false)
	clock = clock.Add(time.Second)
	if _, err := s.BeginAnswer("p", req); !errors.Is(err, ErrExpired) {
		t.Fatalf("answer at expires_at: %v", err)
	}
	if _, err := s.CheckAnswer("p", req); err != nil {
		t.Fatalf("the expired request is still open: %v", err)
	}

	// The next request takes the new limit; with none it carries no expires_at.
	s.Hook("p", claudeHook("PostToolUse", 2000, `{"session_id":"a","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"x"}`))
	_, req2 := s.Hook("p", claudeHook("PermissionRequest", 2100, `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`))
	if st, _ := s.Get("p"); st.Request == nil || st.Request.ExpiresAt != "2026-10-31T13:01:02Z" {
		t.Fatalf("second request: %+v", st.Request)
	}
	maxAge = 0
	s.Hook("p", claudeHook("PostToolUse", 3000, `{"session_id":"a","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"y"}`))
	_, req3 := s.Hook("p", claudeHook("PermissionRequest", 3100, `{"session_id":"a","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"pwd"}}`))
	if st, _ := s.Get("p"); st.State != api.AgentStatusBlocked || st.Request == nil || st.Request.ExpiresAt != "" {
		t.Fatalf("request with no limit: %+v", st)
	}
	clock = clock.Add(48 * time.Hour)
	if _, err := s.BeginAnswer("p", req3); err != nil {
		t.Fatalf("answer with no limit: %v", err)
	}
	if req2 == req3 {
		t.Fatal("one id for two requests")
	}
}
