package control

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// fakeActions records the calls it gets and answers with err when set.
type fakeActions struct {
	mu      sync.Mutex
	created []api.SessionCreateParams
	closed  []string
	err     error
}

func (f *fakeActions) CreateSession(_ context.Context, p api.SessionCreateParams) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, p)
	return "s-1", "p-1", f.err
}

func (f *fakeActions) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeActions) calls() ([]api.SessionCreateParams, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.SessionCreateParams(nil), f.created...), append([]string(nil), f.closed...)
}

func (f *fakeActions) ClosePane(_ context.Context, pane string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, pane)
	return f.err
}

// TestClientCallsAnswered: every method in the schema is answered, never with method not found.
// session.create and pane.close run through Actions and carry the seq; the calls later stages
// build answer unavailable with a hint that names the stage.
func TestClientCallsAnswered(t *testing.T) {
	fa := &fakeActions{}
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Actions = fa })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}

	got, err := cl.SessionCreate(ctx(t), api.SessionCreateParams{Name: "n", Argv: []string{"/bin/true"}})
	if err != nil || got.Session != "s-1" || got.Pane != "p-1" {
		t.Fatalf("session.create: %+v, %v", got, err)
	}
	if created, _ := fa.calls(); len(created) != 1 || created[0].Name != "n" || created[0].Argv[0] != "/bin/true" {
		t.Errorf("Actions got %+v", created)
	}
	_, err = cl.PaneClose(ctx(t), api.PaneRef{Pane: "p-1"})
	if _, closed := fa.calls(); err != nil || len(closed) != 1 || closed[0] != "p-1" {
		t.Fatalf("pane.close: %v, Actions got %v", err, closed)
	}
	var ce *Error
	if _, err := cl.PaneClose(ctx(t), api.PaneRef{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Errorf("pane.close with no pane: %v", err)
	}

	// An *Error keeps its kind; any other error is unavailable.
	fa.setErr(Errorf(api.ErrorKindConflict, "taken"))
	if _, err := cl.SessionCreate(ctx(t), api.SessionCreateParams{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
		t.Errorf("conflict from Actions: %v", err)
	}
	fa.setErr(errors.New("boom"))
	if _, err := cl.PaneClose(ctx(t), api.PaneRef{Pane: "p"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("plain error from Actions: %v", err)
	}

	for _, m := range api.Methods {
		if m.Name == "hab.hello" {
			continue
		}
		var raw any
		err := c.Call(ctx(t), m.Name, struct{}{}, &raw)
		if errors.As(err, &ce) && ce.Code == codeMethodNotFound {
			t.Errorf("%s: method not found", m.Name)
		}
		if stage, ok := later[m.Name]; ok {
			if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable || !strings.Contains(ce.Data.Hint, stage) {
				t.Errorf("%s: %v, want unavailable naming %q", m.Name, err, stage)
			}
		}
	}
	for name := range later {
		if !hasMethod(name) {
			t.Errorf("later names %s, which the schema lacks", name)
		}
	}
}

// TestClientCallsWithoutActions: with no Actions (no holder link), session.create and pane.close
// answer unavailable.
func TestClientCallsWithoutActions(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	var ce *Error
	if _, err := cl.SessionCreate(ctx(t), api.SessionCreateParams{}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("session.create: %v", err)
	}
	if _, err := cl.PaneClose(ctx(t), api.PaneRef{Pane: "p"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("pane.close: %v", err)
	}
}

func hasMethod(name string) bool {
	for _, m := range api.Methods {
		if m.Name == name {
			return true
		}
	}
	return false
}
