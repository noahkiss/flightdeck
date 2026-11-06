package main

import (
	"context"
	"encoding/json"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habits"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// habitService answers the recording habits through the hook endpoint (logbook.md 11, 11.4): the
// brief at a session's start, the reminder at a prompt, the closeout at a stop. Each habit has a
// hab-side setting, and [logbook.habits_off] in the hab's config file turns one off per project.
type habitService struct {
	svc     *lbsvc.Service
	store   *structure.Store
	host    string
	tracker *habits.Tracker
	value   func(id string) json.RawMessage // the hab-side settings; nil: every habit on
	off     *habits.OffFile
	logf    func(string, ...any)
}

// key is the tracker's key for a pane: its session, else the pane.
func (h *habitService) key(pane string) (string, api.Session, bool) {
	if s, ok := h.store.SessionByPane(pane); ok {
		return s.ID, s, true
	}
	return pane, api.Session{}, false
}

// Hook takes every envelope the sink takes: a file change or a commit marks the session.
func (h *habitService) Hook(pane string, env harness.Envelope) {
	if env.Event == "PostToolUse" && habits.Changes(env.Payload) {
		k, _, _ := h.key(pane)
		h.tracker.Changed(k)
	}
}

// Wrote records a Logbook write from a pane's session.
func (h *habitService) Wrote(pane string) {
	k, _, _ := h.key(pane)
	h.tracker.Wrote(k)
}

// on reports whether habit is on: its setting, then the project's switch by the Logbook project
// id or the session's identity.
func (h *habitService) on(habit, project, identity string) bool {
	return habits.On(habit, h.value, h.off.Get(), project, identity)
}

// Context is the hook server's ContextHandler.
func (h *habitService) Context(ctx context.Context, pane string, env harness.Envelope) string {
	habit, ok := hook.ContextEvent(env.Harness, env.Event)
	if !ok {
		return ""
	}
	k, s, inSession := h.key(pane)
	project, identity := h.project(ctx, s, inSession)
	switch habit {
	case habits.Brief:
		return h.brief(ctx, pane, s, project, identity)
	case habits.Reminder:
		// The prompt counts whether or not the reminder is on.
		if h.tracker.Prompt(k) && h.on(habit, project, identity) {
			return habits.ReminderText
		}
	case habits.Closeout:
		// A round's run ends at its turn's end: a closeout would add a turn nobody reads, and
		// the run would close its pane while the agent records (rounds.md 7).
		if s.Round != nil || !h.on(habit, project, identity) {
			return ""
		}
		if h.tracker.Stop(k) {
			return habits.CloseoutText
		}
	}
	return ""
}

// project is a session's Logbook project id and its identity; none outside a session.
func (h *habitService) project(ctx context.Context, s api.Session, inSession bool) (project, identity string) {
	if !inSession {
		return "", ""
	}
	if pr, ok := h.store.Project(s.Project); ok {
		identity = lbsvc.Identity(pr.Remote, pr.RootCommit)
	}
	return logbookProject(ctx, h.svc, h.store, h.host, s.Project), identity
}

// brief is the brief for a pane's session when the habit is on for its project, else "".
func (h *habitService) brief(ctx context.Context, pane string, s api.Session, project, identity string) string {
	if !h.on(habits.Brief, project, identity) {
		return ""
	}
	res, err := h.svc.Dispatch(ctx, api.LogbookActor{}, &lbsvc.Pane{ID: pane, Session: s.ID, Host: h.host, Project: project}, "logbook.brief", nil)
	if err != nil {
		h.logf("logbook: the brief for pane %s: %v", pane, err)
		return ""
	}
	return res.(api.LogbookBrief).Text
}

// OpencodeBrief is the brief gff-hab adds to a new opencode session through the pane's server
// (11.4): as at a hook's session start, but none for a round's run session, whose run owns its
// turns (rounds.md 7).
func (h *habitService) OpencodeBrief(ctx context.Context, pane string) string {
	_, s, inSession := h.key(pane)
	if inSession && s.Round != nil {
		return ""
	}
	project, identity := h.project(ctx, s, inSession)
	return h.brief(ctx, pane, s, project, identity)
}
