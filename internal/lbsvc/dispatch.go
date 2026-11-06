package lbsvc

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// Methods are the logbook.* methods the service serves (10.3). logbook.brief comes with the
// habits (stage 6), logbook.pull and logbook.push with sync (stage 3).
var Methods = []string{"logbook.find", "logbook.get", "logbook.list", "logbook.ready", "logbook.history",
	"logbook.write", "logbook.resolve", "logbook.decide", "logbook.projects", "logbook.status"}

// IsMethod reports whether m is one of Methods.
func IsMethod(m string) bool { return slices.Contains(Methods, m) }

// Dispatch answers one call by name, for the doors that carry raw params: the pane route and the
// CLI when it opens the file itself. With pane set, writes count against the pane's limit and
// record the pane as the actor; else a is the actor.
func (s *Service) Dispatch(ctx context.Context, a api.LogbookActor, pane *Pane, method string, params json.RawMessage) (any, error) {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	decode := func(v any) error {
		if err := json.Unmarshal(params, v); err != nil {
			return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err)
		}
		return nil
	}
	switch method {
	case "logbook.find":
		var p api.LogbookFindParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.Find(ctx, p)
	case "logbook.get":
		var p api.LogbookGetParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.Get(ctx, p)
	case "logbook.list":
		var p api.LogbookListParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.List(ctx, p)
	case "logbook.ready":
		var p api.LogbookReadyParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.Ready(ctx, p)
	case "logbook.history":
		var p api.LogbookHistoryParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.History(ctx, p)
	case "logbook.write":
		var p api.LogbookWriteParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		if pane != nil {
			return s.WritePane(ctx, *pane, p)
		}
		return s.Write(ctx, a, p)
	case "logbook.resolve":
		var p api.LogbookResolveParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		if pane != nil {
			if pane.Propose {
				return nil, control.Errorf(api.ErrorKindInvalid, "this session's writes wait for review: write the value with logbook.write, which files a proposal")
			}
			if err := s.paneSet(ctx, p.ID, p.Field, false, false); err != nil {
				return nil, control.Errorf(api.ErrorKindInvalid, "%v", err)
			}
			if err := s.allow(pane.ID, 1); err != nil {
				return nil, err
			}
			a = PaneActor(*pane)
		}
		return s.Resolve(ctx, a, p)
	case "logbook.decide":
		var p api.LogbookDecideParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		if pane != nil {
			return nil, control.Errorf(api.ErrorKindInvalid, "only a person decides a proposal (logbook.md 9.2): the user approves or rejects it")
		}
		return s.Decide(ctx, a, p)
	case "logbook.projects":
		return s.Projects(ctx)
	case "logbook.status":
		return s.Status(ctx)
	}
	return nil, control.Errorf(api.ErrorKindInvalid, "%q is not a Logbook method this hab serves; it serves %v", method, Methods)
}
