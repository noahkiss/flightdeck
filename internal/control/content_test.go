package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type fakeContent struct{}

func (fakeContent) Show(_ context.Context, p api.ContentShowParams) (api.ContentShown, error) {
	return api.ContentShown{ID: "c1", Session: "s1"}, nil
}

func (fakeContent) List(_ context.Context, p api.ContentListParams) (api.ContentList, error) {
	if p.Session == "gone" {
		return api.ContentList{}, Errorf(api.ErrorKindNotFound, "no session")
	}
	return api.ContentList{}, nil // nil items: the wire still carries a list
}

func (fakeContent) Read(_ context.Context, p api.ContentReadParams) (api.ContentData, error) {
	return api.ContentData{ID: p.ID, Data: []byte("abc"), Size: 3, EOF: true}, nil
}

func (fakeContent) Diff(_ context.Context, p api.ContentDiffParams) (api.ContentDiff, error) {
	return api.ContentDiff{ID: p.ID, Source: api.ContentDiffSourceNone}, nil
}

// TestContentCalls: the content.* methods run through Content with the seq; missing required
// params are invalid; an *Error keeps its kind; with no Content each answers unavailable.
func TestContentCalls(t *testing.T) {
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Content = fakeContent{} })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	if r, err := cl.ContentShow(ctx(t), api.ContentShowParams{Session: "s1", Path: "/a.png"}); err != nil || r.ID != "c1" {
		t.Fatalf("show %+v %v", r, err)
	}
	if r, err := cl.ContentList(ctx(t), api.ContentListParams{Session: "s1"}); err != nil || r.Items == nil {
		t.Fatalf("list %+v %v", r, err)
	}
	if r, err := cl.ContentRead(ctx(t), api.ContentReadParams{Session: "s1", ID: "c1"}); err != nil || string(r.Data) != "abc" || !r.EOF {
		t.Fatalf("read %+v %v", r, err)
	}
	if r, err := cl.ContentDiff(ctx(t), api.ContentDiffParams{Session: "s1", ID: "c1"}); err != nil || r.Source != "none" {
		t.Fatalf("diff %+v %v", r, err)
	}
	var ce *Error
	if _, err := cl.ContentList(ctx(t), api.ContentListParams{Session: "gone"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("not found: %v", err)
	}
	for name, call := range map[string]func() error{
		"show no path":    func() error { _, err := cl.ContentShow(ctx(t), api.ContentShowParams{Session: "s1"}); return err },
		"list no session": func() error { _, err := cl.ContentList(ctx(t), api.ContentListParams{}); return err },
		"read no id":      func() error { _, err := cl.ContentRead(ctx(t), api.ContentReadParams{Session: "s1"}); return err },
		"diff no session": func() error { _, err := cl.ContentDiff(ctx(t), api.ContentDiffParams{ID: "c1"}); return err },
	} {
		if err := call(); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	h2 := startHab(t, "", socketPath(t), nil)
	c2 := dial(t, h2.path)
	hello(t, c2, "")
	if _, err := (api.Client{Caller: c2}).ContentList(ctx(t), api.ContentListParams{Session: "s1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no Content: %v", err)
	}
	// The app tries a content call again only on the busy hint's phrase (app.md 4.7).
	if strings.Contains(ce.Data.Hint, busyPhrase) {
		t.Errorf("the no-Content hint reads as busy: %q", ce.Data.Hint)
	}
	if !strings.Contains(hintContentBusy, busyPhrase) {
		t.Errorf("the busy hint %q lost the phrase the app matches", hintContentBusy)
	}
}

// busyPhrase is what the app looks for to tell a busy answer from another unavailable one.
const busyPhrase = "already run on this connection"

// TestContentScope: content.updated reaches a subscription with its session in scope and stays
// out of other scopes.
func TestContentScope(t *testing.T) {
	e := structure.Event{Type: EventContentUpdated, ID: "c1", Data: api.ContentItem{ID: "c1", Session: "s1"}}
	if !newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s1"}}).pass(e) {
		t.Error("scope sessions s1 dropped its content")
	}
	if newFilter(api.Scope{Kind: api.ScopeKindSessions, IDs: []string{"s2"}}).pass(e) {
		t.Error("scope sessions s2 got another session's content")
	}
	if !newFilter(api.Scope{Kind: api.ScopeKindHost}).pass(e) {
		t.Error("scope host dropped content")
	}
}
