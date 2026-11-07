package confirm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// fakeRounds makes rounds: host "self" is this hab and is refused, as gff-hab's runner refuses
// it; a round on "lost" loses its answer; "refuses" fails.
type fakeRounds struct {
	mu   sync.Mutex
	made []api.ConfirmRound
}

func (f *fakeRounds) CheckRound(r api.ConfirmRound) (api.ConfirmRound, error) {
	if r.Host == "self" || r.Host == "" {
		return r, control.Errorf(api.ErrorKindInvalid, "a round on this hab needs no confirmation")
	}
	r.Trigger = "every 1h"
	return r, nil
}

func (f *fakeRounds) RunRound(_ context.Context, id, from string, r api.ConfirmRound) (api.ConfirmResult, error) {
	f.mu.Lock()
	f.made = append(f.made, r)
	f.mu.Unlock()
	switch r.Host {
	case "lost":
		return api.ConfirmResult{}, fmt.Errorf("%w: the link to lost dropped", ErrUnknown)
	case "refuses":
		return api.ConfirmResult{}, control.Errorf(api.ErrorKindConflict, "a round named %s exists", r.Round.Name)
	}
	return api.ConfirmResult{Host: r.Host, Name: r.Round.Name, Round: "round-" + id[:4]}, nil
}

func (f *fakeRounds) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.made)
}

func newRoundRig(t *testing.T, on bool) (*rig, *fakeRounds) {
	t.Helper()
	r := &rig{runner: &fakeRunner{}, on: on}
	fr := &fakeRounds{}
	svc, err := New(Config{
		Runner:  r.runner,
		Rounds:  fr,
		Enabled: func() bool { return r.on },
		Publish: func(e structure.Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() },
		Notify: func(session, text string) error {
			r.mu.Lock()
			r.notes = append(r.notes, session+": "+text)
			r.mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	r.svc = svc
	return r, fr
}

func roundReq(host, name string, wait int) api.ConfirmRequestParams {
	return api.ConfirmRequestParams{Kind: "round", From: "agent", WaitMS: wait, Round: &api.ConfirmRound{Host: host,
		Round: api.RoundSpec{Name: name, Mode: api.RoundModeFresh, Prompt: "check", Trigger: api.RoundTrigger{Kind: "cron", Every: "1h"}}}}
}

// TestRoundOff: with the setting off, a round on another host is made at once and its card is a
// record; this hab is refused; a lost answer is unknown and a refusal failed.
func TestRoundOff(t *testing.T) {
	r, fr := newRoundRig(t, false)
	ctx := context.Background()
	c, err := r.svc.Request(ctx, roundReq("bee", "nightly", 0))
	if err != nil || c.State != "done" || !c.Record || c.Kind != "round" || c.Result == nil || c.Result.Round == "" || c.Round.Trigger != "every 1h" {
		t.Fatalf("record: %+v %v", c, err)
	}
	if fr.count() != 1 {
		t.Fatalf("made %d rounds", fr.count())
	}
	if _, err := r.svc.Request(ctx, roundReq("self", "here", 0)); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("this hab: %v", err)
	}
	if c, _ := r.svc.Request(ctx, roundReq("lost", "x", 0)); c.State != "unknown" || !strings.Contains(c.Reason, "rounds") {
		t.Fatalf("lost: %+v", c)
	}
	if c, _ := r.svc.Request(ctx, roundReq("refuses", "x", 0)); c.State != "failed" || !strings.Contains(c.Reason, "exists") {
		t.Fatalf("refused: %+v", c)
	}
	if got := strings.Join(r.types(), ","); !strings.HasPrefix(got, "confirm.requested,confirm.closed") {
		t.Errorf("events: %s", got)
	}
}

// TestRoundOn: with the setting on, a round on another host waits for the answer; nothing is
// made before it; a cancel makes nothing; a confirm makes it once; edits are refused; the
// requester whose wait ended hears the outcome in a round's words.
func TestRoundOn(t *testing.T) {
	r, fr := newRoundRig(t, true)
	ctx := context.Background()
	c, err := r.svc.Request(ctx, roundReq("bee", "nightly", 0))
	if err != nil || c.State != "open" || c.Record || c.ExpiresAt == "" {
		t.Fatalf("open: %+v %v", c, err)
	}
	if fr.count() != 0 {
		t.Fatal("a round was made before the answer")
	}
	if _, err := r.svc.Answer(ctx, "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm", Edits: &api.ConfirmEdits{Model: "opus"}}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("edits: %v", err)
	}
	ans, err := r.svc.Answer(ctx, "phone", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm", Edits: &api.ConfirmEdits{}})
	if err != nil || ans.State != "done" || ans.AnsweredBy != "phone" || fr.count() != 1 {
		t.Fatalf("confirm: %+v %v", ans, err)
	}
	if _, err := r.svc.Answer(ctx, "mac", api.ConfirmAnswerParams{ID: c.ID, Choice: "confirm"}); kind(err) != api.ErrorKindConflict {
		t.Fatalf("second answer: %v", err)
	}
	waitFor(t, func() bool { return len(r.notesNow()) == 1 })
	if n := r.notesNow()[0]; !strings.Contains(n, "round nightly on bee") || !strings.Contains(n, "confirmed") {
		t.Errorf("note: %s", n)
	}

	// A wait that gets the cancel; nothing is made.
	done := make(chan api.Confirmation, 1)
	go func() {
		c, _ := r.svc.Request(ctx, roundReq("bee", "weekly", 10000))
		done <- c
	}()
	var open api.Confirmation
	waitFor(t, func() bool {
		l := r.svc.Open()
		if len(l) == 1 {
			open = l[0]
		}
		return len(l) == 1
	})
	if _, err := r.svc.Answer(ctx, "phone", api.ConfirmAnswerParams{ID: open.ID, Choice: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.State != "cancelled" || fr.count() != 1 {
		t.Fatalf("cancelled: %+v, made %d", got, fr.count())
	}
}

// TestRoundUnavailable: a hab with no round runner refuses kind round.
func TestRoundUnavailable(t *testing.T) {
	r := newRig(t, true, 0)
	if _, err := r.svc.Request(context.Background(), roundReq("bee", "x", 0)); kind(err) != api.ErrorKindUnavailable {
		t.Fatalf("no runner: %v", err)
	}
	if _, err := r.svc.Request(context.Background(), api.ConfirmRequestParams{Kind: "round"}); kind(err) != api.ErrorKindInvalid {
		t.Fatalf("no round: %v", err)
	}
}
