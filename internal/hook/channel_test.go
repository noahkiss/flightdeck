package hook

import (
	"context"
	"encoding/json"
	"errors"
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
}
