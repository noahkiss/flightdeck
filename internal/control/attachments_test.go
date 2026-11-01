package control

import (
	"context"
	"errors"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

type fakeAttachments struct{}

func (fakeAttachments) Put(_ context.Context, p api.AttachmentPutParams) (api.AttachmentStored, error) {
	if p.Session == "panic" {
		panic("a bug in the store")
	}
	if p.Session == "gone" {
		return api.AttachmentStored{}, Errorf(api.ErrorKindNotFound, "no session")
	}
	return api.AttachmentStored{ID: "a1", Path: "/p/.flightdeck/attachments/a1-x.png", Chunk: 4}, nil
}

func (fakeAttachments) Write(_ context.Context, p api.AttachmentWriteParams) (api.AttachmentWritten, error) {
	return api.AttachmentWritten{ID: p.ID, Received: p.Offset + int64(len(p.Data)), Done: true}, nil
}

func (fakeAttachments) Get(_ context.Context, p api.AttachmentGetParams) (api.AttachmentData, error) {
	return api.AttachmentData{ID: p.ID, Name: "x.png", Type: "image/png"}, nil // nil data: the wire still carries it
}

// TestAttachmentCalls: the attachment.* methods run through Attachments with the seq; missing
// required params are invalid; an *Error keeps its kind; with no Attachments each answers
// unavailable.
func TestAttachmentCalls(t *testing.T) {
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Attachments = fakeAttachments{} })
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}
	r, err := cl.AttachmentPut(ctx(t), api.AttachmentPutParams{Session: "s1", Name: "x.png", Size: 3, SHA256: "00"})
	if err != nil || r.ID != "a1" || r.Chunk != 4 {
		t.Fatalf("put %+v %v", r, err)
	}
	w, err := cl.AttachmentWrite(ctx(t), api.AttachmentWriteParams{ID: "a1", Data: []byte("abc")})
	if err != nil || w.Received != 3 || !w.Done {
		t.Fatalf("write %+v %v", w, err)
	}
	g, err := cl.AttachmentGet(ctx(t), api.AttachmentGetParams{ID: "a1"})
	if err != nil || g.Data == nil || g.Name != "x.png" {
		t.Fatalf("get %+v %v", g, err)
	}
	var ce *Error
	if _, err := cl.AttachmentPut(ctx(t), api.AttachmentPutParams{Session: "gone", Name: "x"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("not found: %v", err)
	}
	if _, err := cl.AttachmentPut(ctx(t), api.AttachmentPutParams{Session: "panic", Name: "x"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("a panic in the store: %v", err)
	}
	if _, err := cl.AttachmentGet(ctx(t), api.AttachmentGetParams{ID: "a1"}); err != nil {
		t.Errorf("the connection did not survive a panic: %v", err)
	}
	for name, call := range map[string]func() error{
		"put no session": func() error {
			_, err := cl.AttachmentPut(ctx(t), api.AttachmentPutParams{Name: "x"})
			return err
		},
		"put no name": func() error {
			_, err := cl.AttachmentPut(ctx(t), api.AttachmentPutParams{Session: "s1"})
			return err
		},
		"write no id": func() error { _, err := cl.AttachmentWrite(ctx(t), api.AttachmentWriteParams{}); return err },
		"get no id":   func() error { _, err := cl.AttachmentGet(ctx(t), api.AttachmentGetParams{}); return err },
	} {
		if err := call(); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	h2 := startHab(t, "", socketPath(t), nil)
	c2 := dial(t, h2.path)
	hello(t, c2, "")
	if _, err := (api.Client{Caller: c2}).AttachmentGet(ctx(t), api.AttachmentGetParams{ID: "a1"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Errorf("no Attachments: %v", err)
	}
}
