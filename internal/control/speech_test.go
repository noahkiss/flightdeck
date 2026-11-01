package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// slowSpeech blocks each transcribe until its context ends or release is closed.
type slowSpeech struct {
	started chan struct{}
	release chan struct{}
	ended   chan error
}

func (s *slowSpeech) Prepare(context.Context) (api.SpeechStatus, error) {
	return api.SpeechStatus{State: api.SpeechStateReady, Model: "m"}, nil // nil formats: NonNil makes []
}

func (s *slowSpeech) Transcribe(ctx context.Context, p api.SpeechTranscribeParams) (api.SpeechTranscript, error) {
	if string(p.Audio) == "bad" {
		return api.SpeechTranscript{}, Errorf(api.ErrorKindInvalid, "the clip does not decode")
	}
	s.started <- struct{}{}
	select {
	case <-ctx.Done():
		s.ended <- ctx.Err()
		return api.SpeechTranscript{}, ctx.Err()
	case <-s.release:
		return api.SpeechTranscript{Text: "hello", AudioMS: 300, DecodeMS: 20}, nil
	}
}

// TestSpeech: the speech methods run through Speech, carry the seq and keep an *Error's kind;
// formats goes out as []; with no Speech they answer unavailable with the hint.
func TestSpeech(t *testing.T) {
	sp := &slowSpeech{started: make(chan struct{}, 4), release: make(chan struct{}), ended: make(chan error, 4)}
	close(sp.release)
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Speech = sp })
	c := dial(t, h.path)
	hello(t, c, "")
	var raw map[string]any
	if err := c.Call(ctx(t), "speech.prepare", api.SpeechPrepareParams{}, &raw); err != nil {
		t.Fatal(err)
	}
	if f, ok := raw["formats"].([]any); !ok || len(f) != 0 || raw["state"] != "ready" {
		t.Fatalf("prepare: %v", raw)
	}
	cl := api.Client{Caller: c}
	got, err := cl.SpeechTranscribe(ctx(t), api.SpeechTranscribeParams{Audio: []byte{1, 2}, Format: api.SpeechFormatPcm16})
	if err != nil || got.Text != "hello" || got.AudioMS != 300 {
		t.Fatalf("transcribe: %+v %v", got, err)
	}
	var ce *Error
	if _, err := cl.SpeechTranscribe(ctx(t), api.SpeechTranscribeParams{Audio: []byte("bad"), Format: "pcm16"}); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindInvalid {
		t.Fatalf("invalid from Speech: %v", err)
	}

	h2 := startHab(t, "", socketPath(t), nil)
	c2 := dial(t, h2.path)
	hello(t, c2, "")
	for _, m := range []string{"speech.prepare", "speech.transcribe"} {
		err := c2.Call(ctx(t), m, map[string]any{}, nil)
		if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable || ce.Data.Hint != m+" needs the speech engine, which this hab is running without; the speech capability is absent" {
			t.Errorf("%s with no Speech: %v", m, err)
		}
	}
}

// TestSpeechOffReadLoop: a decode in progress does not hold up other calls on the connection,
// and it stops when the connection ends.
func TestSpeechOffReadLoop(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	sp := &slowSpeech{started: make(chan struct{}, 1), release: make(chan struct{}), ended: make(chan error, 1)}
	cfg.Speech = sp
	srv := New(cfg)
	t.Cleanup(srv.Close)
	nc := pipeServe(t, srv)
	c := NewClient(nc)
	hello(t, c, "")
	go c.Call(context.Background(), "speech.transcribe", api.SpeechTranscribeParams{Audio: []byte{0, 0}, Format: "pcm16"}, nil)
	<-sp.started
	pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Call(pctx, "hab.ping", nil, nil); err != nil {
		t.Fatalf("hab.ping behind a running speech.transcribe: %v", err)
	}
	nc.Close()
	select {
	case err := <-sp.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("transcribe ended with %v, want canceled by the connection", err)
		}
	case <-ctx(t).Done():
		t.Fatal("transcribe kept running after its connection ended")
	}
}
