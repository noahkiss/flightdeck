package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// TestChannelRoute: a channel call names its pane by the token; the answer carries the result
// or the error kind; written hears the answer was written, after the client has it.
func TestChannelRoute(t *testing.T) {
	reg := newTestPanes()
	sock := filepath.Join(shortDir(t), "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, func(string, harness.Envelope) string { return "" }, t.Logf)
	wrote := make(chan error, 1)
	s.SetChannels(func(_ context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		if method == "channel.status" {
			return nil, nil, control.Errorf(api.ErrorKindNotFound, "no message")
		}
		var p api.ChannelWaitParams
		json.Unmarshal(params, &p)
		return api.ChannelWaitResult{Message: &api.ChannelInbound{ID: pane, Text: p.Session}}, func(err error) { wrote <- err }, nil
	})
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	addr := Addr(sock, reg.mint("pane-3"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var r api.ChannelWaitResult
	if err := Call(ctx, addr, "channel.wait", api.ChannelWaitParams{Session: "x"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Message == nil || r.Message.ID != "pane-3" || r.Message.Text != "x" {
		t.Fatalf("result %+v", r)
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("written(%v)", err)
		}
	case <-ctx.Done():
		t.Fatal("written was not called")
	}
	err = Call(ctx, addr, "channel.status", api.ChannelStatusParams{ID: "x"}, nil)
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Fatalf("status: %v", err)
	}
	if err := Call(ctx, addr, "session.send", struct{}{}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("a method the route does not take: %v", err)
	}
	if err := Call(ctx, Addr(sock, "forged"), "channel.wait", api.ChannelWaitParams{}, nil); err == nil {
		t.Fatal("an unknown token was served")
	}
	// Confirmations: a request reaches the confirm handler with its pane; with none set it is
	// unavailable; confirm.answer is never on the route.
	if err := Call(ctx, addr, "confirm.request", api.ConfirmRequestParams{Kind: "start"}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Fatalf("confirm with no handler: %v", err)
	}
	s.SetConfirm(func(_ context.Context, pane, method string, _ json.RawMessage) (any, func(error), error) {
		return api.ConfirmRequested{Confirmation: api.Confirmation{ID: pane, From: method}}, nil, nil
	})
	var cr api.ConfirmRequested
	if err := Call(ctx, addr, "confirm.request", api.ConfirmRequestParams{Kind: "start"}, &cr); err != nil || cr.Confirmation.ID != "pane-3" || cr.Confirmation.From != "confirm.request" {
		t.Fatalf("confirm.request: %+v %v", cr, err)
	}
	if err := Call(ctx, addr, "confirm.answer", api.ConfirmAnswerParams{ID: "x", Choice: "confirm"}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("confirm.answer on the route: %v", err)
	}
}

// TestChannelRouteWriteFails: when the caller is gone before the answer, written hears the
// failed write, so a message the wait took goes back rather than counting as delivered.
func TestChannelRouteWriteFails(t *testing.T) {
	reg := newTestPanes()
	sock := filepath.Join(shortDir(t), "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, func(string, harness.Envelope) string { return "" }, t.Logf)
	wrote := make(chan error, 1)
	s.SetChannels(func(ctx context.Context, _, _ string, _ json.RawMessage) (any, func(error), error) {
		<-ctx.Done() // the caller hung up
		return api.ChannelWaitResult{Message: &api.ChannelInbound{ID: "m", Text: "lost"}}, func(err error) { wrote <- err }, nil
	})
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"method":"channel.wait","params":{}}`
	fmt.Fprintf(c, "POST %s%s HTTP/1.1\r\nHost: hab\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", ChannelPath, reg.mint("pane-1"), len(body), body)
	time.Sleep(50 * time.Millisecond) // the request is read and the handler waits
	c.Close()
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("written heard success, though the caller was gone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("written was not called")
	}
}

// TestReadRoute: a pane reaches the read tools through its GFF_HAB address; with no handler a
// read answers unavailable, and a write method stays refused.
func TestReadRoute(t *testing.T) {
	reg := newTestPanes()
	sock := filepath.Join(shortDir(t), "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(reg, func(string, harness.Envelope) string { return "" }, t.Logf)
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	addr := Addr(sock, reg.mint("pane-4"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ce *control.Error
	if err := Call(ctx, addr, "file.read", api.FileReadParams{Path: "/x"}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
		t.Fatalf("no handler: %v", err)
	}
	s.SetReads(func(_ context.Context, method string, params json.RawMessage) (any, error) {
		var p api.FileReadParams
		json.Unmarshal(params, &p)
		if p.Path == "/denied" {
			return nil, control.Errorf(api.ErrorKindDenied, "host-x: the read scope refused this read")
		}
		return api.FileData{Host: "host-x", Path: p.Path, Text: method}, nil
	})
	var d api.FileData
	if err := Call(ctx, addr, "file.read", api.FileReadParams{Path: "/ok"}, &d); err != nil || d.Text != "file.read" || d.Path != "/ok" {
		t.Fatalf("read: %v %+v", err, d)
	}
	if err := Call(ctx, addr, "file.read", api.FileReadParams{Path: "/denied"}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindDenied {
		t.Fatalf("denied: %v", err)
	}
	if err := Call(ctx, addr, "session.create", struct{}{}, nil); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("a write method on the route: %v", err)
	}
}
