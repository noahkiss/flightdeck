// Package speech is gff-hab's side of the hab speech engine (voice.md, Hab speech engine): the
// model download, the speech helper process, one decode at a time in arrival order, the idle
// exit and the session vocabulary fix-up. The helper itself (cmd/gff-hab-speech) needs cgo;
// this package is pure Go and talks to it over stdio (internal/speechproto).
//
// Nothing here stores or logs a clip or its text.
package speech

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/oggopus"
	"github.com/noahkiss/flightdeck/internal/speechproto"
)

// Limits of one clip (voice.md, API addition).
const (
	MaxAudioBytes = 1 << 20
	MaxAudioMS    = 120_000
	// DefaultIdleExit is how long the helper stays loaded with no request: the idle limit of
	// voice mode (app.md 13.1).
	DefaultIdleExit = 10 * time.Minute
	// maxWaiting bounds the clips that wait for the decoder; one more is refused as busy.
	maxWaiting = 16
	// minRate and maxRate bound a pcm16 clip's sample rate.
	minRate = 8000
	maxRate = 48000
)

// Config sets up an Engine.
type Config struct {
	Helper   string        // the helper program
	State    string        // the hab's state folder; the model goes in <State>/speech/<name>
	Model    Model         // the model; Parakeet when zero
	Threads  int           // decode threads; 0: Settings.ThreadCount
	IdleExit time.Duration // DefaultIdleExit when zero
	// Vocabulary returns a session's terms (Terms). nil, or ok false, applies no fix-up.
	Vocabulary func(ctx context.Context, session string) (terms []string, ok bool)
	HTTP       *http.Client // the model download; http.DefaultClient when nil
	Logf       func(format string, args ...any)
}

// Engine is the hab speech engine. Its methods are safe for concurrent use.
type Engine struct {
	cfg    Config
	dir    string // the model folder
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	info     *speechproto.Hello // from helper -info, once it ran well
	present  bool               // the model folder is complete
	fetching bool
	read     int64 // archive bytes read so far
	lastErr  string
	proc     *helper
	busy     bool            // a clip holds the decoder
	waiters  []chan struct{} // clips waiting for the decoder, oldest first
	idle     *time.Timer
	nextID   uint64
	closed   bool
}

// New returns an engine. It starts nothing: the helper starts on the first request.
func New(cfg Config) *Engine {
	if cfg.Model.Name == "" {
		cfg.Model = Parakeet
	}
	if cfg.Threads <= 0 {
		cfg.Threads = Settings{}.ThreadCount()
	}
	if cfg.IdleExit <= 0 {
		cfg.IdleExit = DefaultIdleExit
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	e := &Engine{cfg: cfg, dir: filepath.Join(cfg.State, "speech", cfg.Model.Name)}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.present = present(e.dir, cfg.Model)
	return e
}

// Close stops a download and the helper.
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	p := e.proc
	e.proc = nil
	if e.idle != nil {
		e.idle.Stop()
	}
	e.mu.Unlock()
	e.cancel()
	if p != nil {
		p.stop()
		<-p.done
	}
}

// Prepare answers speech.prepare: it starts the download or the load if needed and returns the
// state at once.
func (e *Engine) Prepare(ctx context.Context) (api.SpeechStatus, error) {
	formats, err := e.formats(ctx)
	if err != nil {
		return api.SpeechStatus{}, control.Errorf(api.ErrorKindUnavailable, "the speech helper does not run: %v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.present {
		if _, err := e.startLocked(); err != nil {
			e.lastErr = err.Error()
		}
		e.touchLocked()
	} else {
		e.fetchLocked()
	}
	st := e.statusLocked()
	st.Formats = formats
	return st, nil
}

// statusLocked is the engine's state without the formats.
func (e *Engine) statusLocked() api.SpeechStatus {
	st := api.SpeechStatus{Model: e.cfg.Model.Name, Error: e.lastErr}
	switch {
	case e.fetching:
		st.State = api.SpeechStateDownloading
		pct := e.pctLocked()
		st.Progress = &pct
	case !e.present:
		st.State = api.SpeechStateAbsent
	case e.proc == nil:
		st.State = api.SpeechStateLoading // the load starts with the next request
	default:
		select {
		case <-e.proc.ready:
			st.State = api.SpeechStateReady
		default:
			st.State = api.SpeechStateLoading
		}
	}
	return st
}

func (e *Engine) pctLocked() int {
	if e.cfg.Model.Size <= 0 {
		return 0
	}
	return int(min(e.read*100/e.cfg.Model.Size, 100))
}

// formats returns the audio formats the helper decodes, from `gff-hab-speech -info`, run once.
func (e *Engine) formats(ctx context.Context) ([]api.SpeechFormat, error) {
	e.mu.Lock()
	info := e.info
	e.mu.Unlock()
	if info == nil {
		h, err := helperInfo(ctx, e.cfg.Helper)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		e.info = &h
		info = &h
		e.mu.Unlock()
	}
	out := []api.SpeechFormat{}
	for _, f := range []api.SpeechFormat{api.SpeechFormatOpus, api.SpeechFormatPcm16} {
		if slices.Contains(info.Formats, string(f)) {
			out = append(out, f)
		}
	}
	return out, nil
}

// fetchLocked starts the model download unless one runs.
func (e *Engine) fetchLocked() {
	if e.fetching || e.present || e.closed {
		return
	}
	e.fetching, e.read = true, 0 // lastErr stays until a download or load succeeds
	m := e.cfg.Model
	e.cfg.Logf("speech: downloading the model %s from %s into %s. %s", m.Name, m.URL, e.dir, m.Attribution)
	go func() {
		err := fetch(e.ctx, e.cfg.HTTP, m, e.dir, func(n int64) {
			e.mu.Lock()
			e.read = n
			e.mu.Unlock()
		})
		e.mu.Lock()
		defer e.mu.Unlock()
		e.fetching = false
		if err != nil {
			e.lastErr = "model download: " + err.Error()
			e.cfg.Logf("speech: %s", e.lastErr)
			return
		}
		e.present, e.lastErr = true, ""
		e.cfg.Logf("speech: model %s downloaded and checked", m.Name)
		if _, err := e.startLocked(); err != nil {
			e.lastErr = err.Error()
		}
		e.touchLocked()
	}()
}

// startLocked returns the running helper, starting one if none runs.
func (e *Engine) startLocked() (*helper, error) {
	if e.closed {
		return nil, errors.New("the speech engine is closed")
	}
	if e.proc != nil {
		return e.proc, nil
	}
	p, err := startHelper(e.cfg.Helper, e.dir, e.cfg.Threads, e.cfg.Logf)
	if err != nil {
		return nil, err
	}
	e.proc = p
	go func() {
		select {
		case <-p.ready:
			e.mu.Lock()
			e.lastErr = ""
			e.mu.Unlock()
			<-p.done
		case <-p.done:
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.proc == p {
			e.proc = nil
		}
		if err := p.failure(); err != nil && !e.closed && !p.stoppedOnPurpose() {
			e.lastErr = err.Error()
			e.cfg.Logf("speech: %v", err)
		} else {
			e.cfg.Logf("speech: helper stopped")
		}
	}()
	return p, nil
}

// touchLocked restarts the idle timer.
func (e *Engine) touchLocked() {
	if e.closed {
		return
	}
	if e.idle != nil {
		e.idle.Stop()
	}
	e.idle = time.AfterFunc(e.cfg.IdleExit, e.idleExit)
}

// idleExit stops the helper when no clip holds or waits for the decoder.
func (e *Engine) idleExit() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy || len(e.waiters) > 0 || e.proc == nil {
		return // the clip that ends restarts the timer
	}
	e.cfg.Logf("speech: no request for %v: stopping the helper", e.cfg.IdleExit)
	e.proc.stop()
	e.proc = nil
}

// Transcribe answers speech.transcribe. Errors are *control.Error.
func (e *Engine) Transcribe(ctx context.Context, p api.SpeechTranscribeParams) (api.SpeechTranscript, error) {
	var res api.SpeechTranscript
	formats, err := e.formats(ctx)
	if err != nil {
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech helper does not run: %v", err)
	}
	audioMS, err := check(p, formats)
	if err != nil {
		return res, err
	}
	e.mu.Lock()
	if !e.present {
		e.fetchLocked()
		st := e.statusLocked()
		e.mu.Unlock()
		if st.State == api.SpeechStateDownloading {
			return res, control.Errorf(api.ErrorKindUnavailable, "downloading %d%%", *st.Progress)
		}
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech model is not downloaded: %s", st.Error)
	}
	e.mu.Unlock()

	if err := e.acquire(ctx); err != nil {
		return res, err
	}
	held := true
	defer func() {
		if held {
			e.release()
		}
	}()
	e.mu.Lock()
	h, err := e.startLocked()
	e.nextID++
	id := e.nextID
	e.mu.Unlock()
	if err != nil {
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech helper did not start: %v", err)
	}
	select {
	case <-h.ready:
	case <-h.done:
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech engine did not load: %v", h.failure())
	case <-ctx.Done():
		return res, control.Errorf(api.ErrorKindUnavailable, "speech.transcribe: %v while the model loads", ctx.Err())
	}
	format := p.Format
	req := speechproto.Request{ID: id, Format: string(format)}
	if format == api.SpeechFormatPcm16 {
		req.SampleRate = rate(p)
	}
	if err := h.send(req, p.Audio); err != nil {
		h.fail(err)
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech helper stopped: %v", err)
	}
	var f speechproto.Frame
	select {
	case f = <-h.replies:
	case <-h.done:
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech helper stopped: %v", h.failure())
	case <-ctx.Done():
		// The decoder is still busy with this clip: keep the slot until the helper answers, so
		// the next clip never runs beside it.
		held = false
		go func() {
			select {
			case <-h.replies:
			case <-h.done:
			}
			e.release()
		}()
		return res, control.Errorf(api.ErrorKindUnavailable, "speech.transcribe: %v", ctx.Err())
	}
	if f.Kind == speechproto.KindError {
		var pe speechproto.Error
		json.Unmarshal(f.Payload, &pe)
		if pe.Kind == speechproto.ErrInvalid {
			return res, control.Errorf(api.ErrorKindInvalid, "the clip does not decode: %s", pe.Message)
		}
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech engine failed: %s", pe.Message)
	}
	var r speechproto.Result
	if err := json.Unmarshal(f.Payload, &r); err != nil || r.ID != id {
		h.fail(errors.New("the helper answered out of turn"))
		return res, control.Errorf(api.ErrorKindUnavailable, "the speech helper answered out of turn")
	}
	res = api.SpeechTranscript{Text: r.Text, AudioMS: audioMS, DecodeMS: r.DecodeMS}
	if r.AudioMS > 0 {
		res.AudioMS = r.AudioMS
	}
	if p.Session != "" && e.cfg.Vocabulary != nil {
		if terms, ok := e.cfg.Vocabulary(ctx, p.Session); ok {
			if fixed, changed := Fixup(r.Text, terms); changed {
				res.Text, res.Raw = fixed, r.Text
			}
		}
	}
	return res, nil
}

// rate is a pcm16 clip's sample rate.
func rate(p api.SpeechTranscribeParams) int {
	if p.SampleRate == 0 {
		return 16000
	}
	return p.SampleRate
}

// check applies the limits and returns the clip's length in milliseconds.
func check(p api.SpeechTranscribeParams, formats []api.SpeechFormat) (int, error) {
	switch {
	case p.Language != "" && p.Language != "en":
		return 0, control.Errorf(api.ErrorKindInvalid, "language %q: this hab transcribes en only", p.Language)
	case len(p.Audio) == 0:
		return 0, control.Errorf(api.ErrorKindInvalid, "speech.transcribe needs audio")
	case len(p.Audio) > MaxAudioBytes:
		return 0, control.Errorf(api.ErrorKindInvalid, "the clip is %d bytes; at most %d", len(p.Audio), MaxAudioBytes)
	case p.Format != api.SpeechFormatOpus && p.Format != api.SpeechFormatPcm16:
		return 0, control.Errorf(api.ErrorKindInvalid, "format %q: use opus or pcm16", p.Format)
	case !slices.Contains(formats, p.Format):
		return 0, control.Errorf(api.ErrorKindInvalid, "format %s: this hab accepts %v", p.Format, formats)
	}
	var ms int
	if p.Format == api.SpeechFormatPcm16 {
		r := rate(p)
		if r < minRate || r > maxRate {
			return 0, control.Errorf(api.ErrorKindInvalid, "sample_rate %d: use %d to %d", r, minRate, maxRate)
		}
		if len(p.Audio)%2 != 0 {
			return 0, control.Errorf(api.ErrorKindInvalid, "a pcm16 clip has an even number of bytes")
		}
		ms = int(int64(len(p.Audio)/2) * 1000 / int64(r))
	} else {
		s, err := oggopus.Parse(p.Audio)
		if err != nil {
			return 0, control.Errorf(api.ErrorKindInvalid, "the clip is not Ogg Opus: %v", err)
		}
		ms = s.DurationMS()
	}
	if ms > MaxAudioMS {
		return 0, control.Errorf(api.ErrorKindInvalid, "the clip is %d ms; at most %d", ms, MaxAudioMS)
	}
	return ms, nil
}

// acquire waits for the decoder in arrival order.
func (e *Engine) acquire(ctx context.Context) error {
	e.mu.Lock()
	if !e.busy && len(e.waiters) == 0 {
		e.busy = true
		e.mu.Unlock()
		return nil
	}
	if len(e.waiters) >= maxWaiting {
		e.mu.Unlock()
		return control.Errorf(api.ErrorKindUnavailable, "%d clips already wait for the speech engine", maxWaiting)
	}
	ch := make(chan struct{})
	e.waiters = append(e.waiters, ch)
	e.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		e.mu.Lock()
		if i := slices.Index(e.waiters, ch); i >= 0 {
			e.waiters = slices.Delete(e.waiters, i, i+1)
			e.mu.Unlock()
		} else {
			e.mu.Unlock()
			e.release() // handed over just now: pass it on
		}
		return control.Errorf(api.ErrorKindUnavailable, "speech.transcribe: %v while waiting for the engine", ctx.Err())
	}
}

// release hands the decoder to the oldest waiting clip, or frees it and restarts the idle timer.
func (e *Engine) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.waiters) > 0 {
		ch := e.waiters[0]
		e.waiters = e.waiters[1:]
		close(ch)
		return
	}
	e.busy = false
	e.touchLocked()
}

// PCM16 encodes samples as little-endian 16-bit bytes, the pcm16 format.
func PCM16(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}
