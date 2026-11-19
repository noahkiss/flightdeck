package gffclient

import (
	"context"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/friction"
)

// frictionChoices are the kinds of a friction note, in the order Tab cycles them.
var frictionChoices = []string{"preference", "blocker"}

// noteFriction asks for one line and sends it as a friction note on the focused view's session
// to that session's hab (diagnostics.md 9.1). Tab marks the note a blocker: the work could not
// be done in Flightdeck. The bottom bar shows the note's trace.
func (u *workspaceUI) noteFriction() {
	v := u.c.focused()
	if v == nil || v.h == nil {
		u.msg = "a friction note needs a pane in focus"
		return
	}
	host, name := v.h.label, v.spec.Label
	p := api.FrictionParams{Pane: v.spec.ID, Session: v.spec.Session}
	if r, ok := rowOf(u.rows, v); ok {
		if r.Offline() {
			u.msg = r.Host + " is offline"
			return
		}
		p.Session, p.Pane, name = r.Session, r.Pane, r.Name
	}
	pr := &prompt{label: "friction note on " + name + " · Tab: blocker or preference", choices: frictionChoices, host: host}
	pr.done = func(text, host string) {
		t, err := friction.CheckText(text)
		if err != nil {
			u.msg = "friction note not sent: " + err.Error()
			return
		}
		p.Text, p.Blocker = t, frictionChoices[pr.choice] == "blocker"
		u.call(host, "friction note", func(ctx context.Context, a api.Client) error {
			res, err := a.DiagFriction(ctx, p)
			if err == nil {
				u.c.run(func() { u.msg = "friction note " + res.Trace + " stored on " + host })
			}
			return err
		})
	}
	u.ask(pr)
}
