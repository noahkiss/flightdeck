package main

import (
	"context"
	"encoding/json"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habits"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/settings"
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

var habitSettings = map[string]string{
	habits.Brief:    settings.HabitBrief,
	habits.Reminder: settings.HabitReminder,
	habits.Closeout: settings.HabitCloseout,
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
	if h.value != nil {
		var v bool
		if json.Unmarshal(h.value(habitSettings[habit]), &v) == nil && !v {
			return false
		}
	}
	return !h.off.Get().Has(habit, project, identity)
}

// Context is the hook server's ContextHandler.
func (h *habitService) Context(ctx context.Context, pane string, env harness.Envelope) string {
	habit, ok := hook.ContextEvent(env.Harness, env.Event)
	if !ok {
		return ""
	}
	k, s, inSession := h.key(pane)
	project, identity := "", ""
	if inSession {
		if pr, ok := h.store.Project(s.Project); ok {
			identity = lbsvc.Identity(pr.Remote, pr.RootCommit)
		}
		project = logbookProject(ctx, h.svc, h.store, h.host, s.Project)
	}
	switch habit {
	case habits.Brief:
		if !h.on(habit, project, identity) {
			return ""
		}
		res, err := h.svc.Dispatch(ctx, api.LogbookActor{}, &lbsvc.Pane{ID: pane, Session: s.ID, Host: h.host, Project: project}, "logbook.brief", nil)
		if err != nil {
			h.logf("logbook: the brief for pane %s: %v", pane, err)
			return ""
		}
		return res.(api.LogbookBrief).Text
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
