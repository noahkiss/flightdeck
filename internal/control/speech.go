package control

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Speech is the hab speech engine (voice.md). gff-hab implements it with internal/speech. An
// error that is an *Error keeps its kind.
type Speech interface {
	// Prepare starts the model download or the load if needed and returns the state at once.
	Prepare(ctx context.Context) (api.SpeechStatus, error)
	// Transcribe turns one clip into text. It waits its turn for the decoder.
	Transcribe(ctx context.Context, p api.SpeechTranscribeParams) (api.SpeechTranscript, error)
}

const (
	hintNoSpeech = "%s needs the speech engine, which this hab is running without; the speech capability is absent"
	// maxRunningSpeech bounds the speech calls one connection may have running or waiting.
	maxRunningSpeech = 8
	// speechTimeout bounds one speech call: a wait for the decoder, a model load and the decode
	// of a clip of the longest length.
	speechTimeout = 5 * time.Minute
)

// handleSpeech runs speech.prepare and speech.transcribe on their own goroutine: a decode takes
// seconds and the read loop goes on meanwhile. The engine runs one decode at a time across all
// connections.
func (c *conn) handleSpeech(m message) {
	seq := c.currentSeq()
	var (
		prep  api.SpeechPrepareParams
		trans api.SpeechTranscribeParams
		e     *Error
	)
	if m.Method == "speech.prepare" {
		e = c.decodeUp(m, &prep)
	} else {
		e = c.decodeUp(m, &trans)
	}
	if e != nil {
		c.reply(m, seq, nil, e)
		return
	}
	sp := c.s.cfg.Speech
	if sp == nil {
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, hintNoSpeech, m.Method))
		return
	}
	c.mu.Lock()
	if c.speeches >= maxRunningSpeech {
		c.mu.Unlock()
		c.reply(m, seq, nil, appErr(api.ErrorKindUnavailable, "%d speech calls already run on this connection", maxRunningSpeech))
		return
	}
	c.speeches++
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, speechTimeout)
		defer cancel()
		var (
			res any
			err error
		)
		if m.Method == "speech.prepare" {
			var st api.SpeechStatus
			st, err = sp.Prepare(ctx)
			st.Seq = api.Seq{Seq: seq}
			res = api.NonNil(st)
		} else {
			var t api.SpeechTranscript
			t, err = sp.Transcribe(ctx, trans)
			t.Seq = api.Seq{Seq: seq}
			res = t
		}
		c.mu.Lock()
		c.speeches--
		c.mu.Unlock()
		if err != nil {
			c.reply(m, seq, nil, actionErr(m.Method, err))
			return
		}
		c.reply(m, seq, res, nil)
	}()
}
