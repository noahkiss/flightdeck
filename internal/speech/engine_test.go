package speech

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/oggopus"
)

// clip is the hand-made fixture: ms milliseconds of a 440 Hz tone at 16 kHz, pcm16, with the
// first sample set to tag so the fake helper can tell clips apart.
func clip(tag int16, ms int) []byte {
	n := 16 * ms
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	s[0] = tag
	return PCM16(s)
}

func pcm(tag int16) api.SpeechTranscribeParams {
	return api.SpeechTranscribeParams{Audio: clip(tag, 300), Format: api.SpeechFormatPcm16}
}

// fakeModel is a model whose files are a few bytes each.
var fakeFiles = map[string]string{"encoder.int8.onnx": "enc", "decoder.int8.onnx": "dec", "joiner.int8.onnx": "joi", "tokens.txt": "a 0\n"}

func fakeArchive(t *testing.T) []byte {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "model/", Mode: 0o755, Typeflag: tar.TypeDir})
	for _, n := range []string{"tokens.txt", "joiner.int8.onnx", "decoder.int8.onnx", "encoder.int8.onnx"} {
		add("model/"+n, fakeFiles[n])
	}
	add("model/../../escape.txt", "never written outside")
	add("model/test_wavs/0.wav", strings.Repeat("x", 64<<10)) // skipped, but counted and hashed
	tw.Close()
	return tb.Bytes()
}

func gz(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

func modelFor(url string, archive []byte) Model {
	sum := sha256.Sum256(archive)
	return Model{Name: "fake-model", URL: url, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)),
		Files: Parakeet.Files, Attribution: "Fake model, for tests only."}
}

// serve serves archive. With a gate, it sends the first half, then waits for the gate.
func serve(t *testing.T, archive []byte, gate chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model.tar" {
			http.NotFound(w, r)
			return
		}
		half := len(archive) / 2
		w.Write(archive[:half])
		w.(http.Flusher).Flush()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		w.Write(archive[half:])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// installModel puts a complete fake model into state, as a finished download would.
func installModel(t *testing.T, state string, m Model) {
	t.Helper()
	dir := filepath.Join(state, "speech", m.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, body := range fakeFiles {
		os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600)
	}
	os.WriteFile(filepath.Join(dir, stampFile), []byte(m.SHA256+"\n"), 0o600)
}

type harness struct {
	e     *Engine
	log   string // the fake helper's event file
	state string
	logs  *syncLog
}

type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

func newHarness(t *testing.T, mode string, m Model, install bool, mod func(*Config)) *harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{state: t.TempDir(), logs: &syncLog{}}
	h.log = filepath.Join(t.TempDir(), "events")
	t.Setenv("GFF_SPEECH_FAKE", mode)
	t.Setenv("GFF_SPEECH_FAKE_LOG", h.log)
	if install {
		installModel(t, h.state, m)
	}
	cfg := Config{Helper: exe, State: h.state, Model: m, Threads: 1, Logf: h.logs.logf}
	if mod != nil {
		mod(&cfg)
	}
	h.e = New(cfg)
	t.Cleanup(h.e.Close)
	return h
}

func (h *harness) events() []string {
	b, _ := os.ReadFile(h.log)
	return strings.Fields(strings.ReplaceAll(string(b), " ", "_"))
}

func (h *harness) count(ev string) int {
	n := 0
	for _, e := range h.events() {
		if e == ev {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func kindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}

var testModel = modelFor("http://unused.invalid/model.tar", []byte("x"))

// TestTranscribe: the helper starts on the first clip, loads, and answers; prepare then says
// ready with the helper's formats.
func TestTranscribe(t *testing.T) {
	h := newHarness(t, "ok", testModel, true, nil)
	res, err := h.e.Transcribe(context.Background(), pcm(7))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "clip 7" || res.AudioMS != 300 || res.DecodeMS != 5 || res.Raw != "" {
		t.Fatalf("got %+v", res)
	}
	st, err := h.e.Prepare(context.Background())
	if err != nil || st.State != api.SpeechStateReady || !slices.Equal(st.Formats, []api.SpeechFormat{"opus", "pcm16"}) || st.Model != "fake-model" {
		t.Fatalf("prepare: %v %+v", err, st)
	}
	if h.count("start") != 1 {
		t.Fatalf("events %v", h.events())
	}
}

// TestPrepareLoads: prepare on a downloaded model starts the helper and returns loading at once.
func TestPrepareLoads(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_LOAD", "300")
	h := newHarness(t, "ok", testModel, true, nil)
	st, err := h.e.Prepare(context.Background())
	if err != nil || st.State != api.SpeechStateLoading || st.Progress != nil {
		t.Fatalf("prepare: %v %+v", err, st)
	}
	waitFor(t, "ready", func() bool {
		st, _ := h.e.Prepare(context.Background())
		return st.State == api.SpeechStateReady
	})
	if h.count("start") != 1 {
		t.Fatalf("a second prepare started another helper: %v", h.events())
	}
}

// TestQueue: clips decode one at a time, in the order they arrived, each with its own text.
func TestQueue(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_DECODE", "80")
	h := newHarness(t, "ok", testModel, true, nil)
	if _, err := h.e.Transcribe(context.Background(), pcm(0)); err != nil { // loaded
		t.Fatal(err)
	}
	const n = 5
	var wg sync.WaitGroup
	results := make([]string, n+1)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.e.Transcribe(context.Background(), pcm(int16(i)))
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = res.Text
		}()
		// The next clip arrives only once this one holds or waits for the decoder.
		waitFor(t, "clip queued", func() bool {
			h.e.mu.Lock()
			defer h.e.mu.Unlock()
			return h.e.busy && len(h.e.waiters) == i-1
		})
	}
	wg.Wait()
	for i := 1; i <= n; i++ {
		if results[i] != "clip "+string(rune('0'+i)) {
			t.Fatalf("clip %d got %q", i, results[i])
		}
	}
	var want []string
	want = append(want, "start", "begin_0", "end_0")
	for i := 1; i <= n; i++ {
		want = append(want, "begin_"+string(rune('0'+i)), "end_"+string(rune('0'+i)))
	}
	if got := h.events(); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// TestCancelKeepsOrder: a clip whose caller gives up keeps the decoder until the helper answers,
// so the next clip never runs beside it.
func TestCancelKeepsOrder(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_DECODE", "300")
	h := newHarness(t, "ok", testModel, true, nil)
	if _, err := h.e.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waitFor(t, "ready", func() bool { st, _ := h.e.Prepare(context.Background()); return st.State == api.SpeechStateReady })
	if _, err := h.e.Transcribe(ctx, pcm(1)); kindOf(err) != api.ErrorKindUnavailable {
		t.Fatalf("cancelled clip: %v", err)
	}
	res, err := h.e.Transcribe(context.Background(), pcm(2))
	if err != nil || res.Text != "clip 2" {
		t.Fatalf("next clip: %v %+v", err, res)
	}
	if got, want := h.events(), []string{"start", "begin_1", "end_1", "begin_2", "end_2"}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// TestIdleExit: the helper exits after the idle time with no request, and the next clip starts
// a new one.
func TestIdleExit(t *testing.T) {
	h := newHarness(t, "ok", testModel, true, func(c *Config) { c.IdleExit = 200 * time.Millisecond })
	if _, err := h.e.Transcribe(context.Background(), pcm(1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "helper exit", func() bool { return h.count("exit") == 1 })
	h.e.mu.Lock()
	running := h.e.proc != nil
	h.e.mu.Unlock()
	if running {
		t.Fatal("helper still recorded after idle exit")
	}
	st, _ := h.e.Prepare(context.Background())
	if st.Error != "" {
		t.Fatalf("an idle exit is not an error: %q", st.Error)
	}
	if _, err := h.e.Transcribe(context.Background(), pcm(2)); err != nil {
		t.Fatal(err)
	}
	if h.count("start") != 2 {
		t.Fatalf("events %v", h.events())
	}
}

// TestIdleWaitsForBusy: a long decode is not cut by the idle timer.
func TestIdleWaitsForBusy(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_DECODE", "400")
	h := newHarness(t, "ok", testModel, true, func(c *Config) { c.IdleExit = 100 * time.Millisecond })
	if _, err := h.e.Transcribe(context.Background(), pcm(1)); err != nil {
		t.Fatalf("decode longer than the idle time: %v", err)
	}
}

// TestHelperCrash: a helper that dies on a clip gives unavailable; the next clip starts a new
// helper.
func TestHelperCrash(t *testing.T) {
	h := newHarness(t, "crash", testModel, true, nil)
	_, err := h.e.Transcribe(context.Background(), pcm(1))
	if kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("crash: %v", err)
	}
	waitFor(t, "helper gone", func() bool { h.e.mu.Lock(); defer h.e.mu.Unlock(); return h.e.proc == nil })
	t.Setenv("GFF_SPEECH_FAKE", "ok")
	st, _ := h.e.Prepare(context.Background()) // starts a new helper; the error stays until it loads
	if !strings.Contains(st.Error, "exit status 3") {
		t.Fatalf("status after crash: %+v", st)
	}
	if res, err := h.e.Transcribe(context.Background(), pcm(2)); err != nil || res.Text != "clip 2" {
		t.Fatalf("after crash: %v %+v", err, res)
	}
}

// TestLoadFail: a model that does not load is unavailable, with the reason.
func TestLoadFail(t *testing.T) {
	h := newHarness(t, "load-fail", testModel, true, nil)
	_, err := h.e.Transcribe(context.Background(), pcm(1))
	if kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "did not load: the model did not load: no model") {
		t.Fatalf("load fail: %v", err)
	}
	waitFor(t, "error in status", func() bool {
		h.e.mu.Lock()
		defer h.e.mu.Unlock()
		return h.e.proc == nil && strings.Contains(h.e.lastErr, "no model")
	})
}

// TestInvalidClip: a clip the helper cannot decode is invalid.
func TestInvalidClip(t *testing.T) {
	h := newHarness(t, "invalid", testModel, true, nil)
	_, err := h.e.Transcribe(context.Background(), pcm(1))
	if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "not audio") {
		t.Fatalf("invalid: %v", err)
	}
}

// longOpus is a hand-made Ogg Opus file that claims 121 s; its packets are not real audio.
func longOpus(seconds int) []byte {
	b := oggopus.Page(0x02, 0, 1, 0, oggopus.Head(1, 0, 16000))
	b = append(b, oggopus.Page(0, 0, 1, 1, oggopus.Tags("test"))...)
	return append(b, oggopus.Page(0x04, int64(seconds)*oggopus.Rate, 1, 2, []byte{0xf8})...)
}

// lyingOpus is a hand-made Ogg Opus file whose pages say granule 0 but whose 1009 packets of
// 120 ms hold 121 s.
func lyingOpus() []byte {
	b := oggopus.Page(0x02, 0, 1, 0, oggopus.Head(1, 0, 16000))
	b = append(b, oggopus.Page(0, 0, 1, 1, oggopus.Tags("test"))...)
	pkt := []byte{31<<3 | 3, 6}
	for i := 0; i < 1009; i += 50 {
		var ps [][]byte
		for j := i; j < min(i+50, 1009); j++ {
			ps = append(ps, pkt)
		}
		typ := byte(0)
		if i+50 >= 1009 {
			typ = 0x04
		}
		b = append(b, oggopus.Page(typ, 0, 1, uint32(2+i/50), ps...)...)
	}
	return b
}

// TestLimits: the clip checks run before the helper sees a byte.
func TestLimits(t *testing.T) {
	h := newHarness(t, "ok", testModel, true, nil)
	for _, tc := range []struct {
		name string
		p    api.SpeechTranscribeParams
		want string
	}{
		{"empty", api.SpeechTranscribeParams{Format: "pcm16"}, "needs audio"},
		{"too large", api.SpeechTranscribeParams{Format: "pcm16", Audio: make([]byte, MaxAudioBytes+2)}, "at most 1048576"},
		{"too long", api.SpeechTranscribeParams{Format: "opus", Audio: longOpus(121)}, "at most 120000"},
		{"granule lies", api.SpeechTranscribeParams{Format: "opus", Audio: lyingOpus()}, "is 121080 ms by its packets"},
		{"language", api.SpeechTranscribeParams{Format: "pcm16", Audio: clip(1, 10), Language: "fr"}, "en only"},
		{"format", api.SpeechTranscribeParams{Format: "mp3", Audio: clip(1, 10)}, "use opus or pcm16"},
		{"odd", api.SpeechTranscribeParams{Format: "pcm16", Audio: []byte{1, 2, 3}}, "even number"},
		{"rate", api.SpeechTranscribeParams{Format: "pcm16", Audio: clip(1, 10), SampleRate: 4000}, "sample_rate 4000"},
		{"not ogg", api.SpeechTranscribeParams{Format: "opus", Audio: clip(1, 10)}, "not Ogg Opus"},
	} {
		_, err := h.e.Transcribe(context.Background(), tc.p)
		if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want invalid %q", tc.name, err, tc.want)
		}
	}
	if res, err := h.e.Transcribe(context.Background(), api.SpeechTranscribeParams{Format: "opus", Audio: longOpus(120)}); err != nil || res.AudioMS != 120000 {
		t.Fatalf("120 s opus: %v %+v", err, res)
	}
	begins := 0
	for _, ev := range h.events() {
		if strings.HasPrefix(ev, "begin_") {
			begins++
		}
	}
	if begins != 1 || h.count("start") != 1 {
		t.Fatalf("the helper saw a refused clip: %v", h.events())
	}
}

// TestFormats: a helper without Opus leaves it out of formats, and an opus clip is invalid.
func TestFormats(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_FORMATS", "pcm16")
	h := newHarness(t, "ok", testModel, true, nil)
	st, err := h.e.Prepare(context.Background())
	if err != nil || !slices.Equal(st.Formats, []api.SpeechFormat{"pcm16"}) {
		t.Fatalf("prepare: %v %+v", err, st)
	}
	_, err = h.e.Transcribe(context.Background(), api.SpeechTranscribeParams{Format: "opus", Audio: longOpus(1)})
	if kindOf(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "accepts [pcm16]") {
		t.Fatalf("opus: %v", err)
	}
}

// TestBadHelper: a helper of another protocol, or one that sends no hello, is unavailable.
func TestBadHelper(t *testing.T) {
	h := newHarness(t, "bad-proto", testModel, true, nil)
	if _, err := h.e.Prepare(context.Background()); kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "protocol 99") {
		t.Fatalf("bad protocol: %v", err)
	}
	m := newHarness(t, "ok", testModel, true, nil)
	m.e.Prepare(context.Background()) // -info passes as ok
	m.e.Close()
	m2 := newHarness(t, "ok", testModel, true, nil)
	if _, err := m2.e.Prepare(context.Background()); err != nil { // cache -info
		t.Fatal(err)
	}
	m2.e.mu.Lock()
	if p := m2.e.proc; p != nil {
		m2.e.proc = nil
		m2.e.mu.Unlock()
		p.stop()
		<-p.done
	} else {
		m2.e.mu.Unlock()
	}
	t.Setenv("GFF_SPEECH_FAKE", "mute")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := m2.e.Transcribe(ctx, pcm(1)); kindOf(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "while the model loads") {
		t.Fatalf("mute helper: %v", err)
	}
}

// TestVocabulary: with a session, the fix-up runs and raw keeps the engine's text.
func TestVocabulary(t *testing.T) {
	t.Setenv("GFF_SPEECH_FAKE_TEXT", "run link test dot go")
	h := newHarness(t, "ok", testModel, true, func(c *Config) {
		c.Vocabulary = func(_ context.Context, session string) ([]string, bool) {
			if session != "s1" {
				return nil, false
			}
			return Terms([]string{"internal/link/link_test.go"}), true
		}
	})
	p := pcm(1)
	p.Session = "s1"
	res, err := h.e.Transcribe(context.Background(), p)
	if err != nil || res.Text != "run link_test.go" || res.Raw != "run link test dot go" {
		t.Fatalf("with session: %v %+v", err, res)
	}
	p.Session = "unknown"
	if res, _ := h.e.Transcribe(context.Background(), p); res.Text != "run link test dot go" || res.Raw != "" {
		t.Fatalf("unknown session: %+v", res)
	}
	if res, _ := h.e.Transcribe(context.Background(), pcm(1)); res.Raw != "" {
		t.Fatalf("no session: %+v", res)
	}
}

// TestDownload: prepare starts the download and reports progress; a clip meanwhile is
// unavailable with the progress; then the model loads and clips decode.
func TestDownload(t *testing.T) {
	archive := gz(fakeArchive(t))
	gate := make(chan struct{})
	srv := serve(t, archive, gate)
	m := modelFor(srv.URL+"/model.tar", archive)
	h := newHarness(t, "ok", m, false, nil)
	if st, _ := h.e.Prepare(context.Background()); st.State != api.SpeechStateDownloading || st.Progress == nil {
		t.Fatalf("first prepare: %+v", st)
	}
	waitFor(t, "half the archive", func() bool {
		st, _ := h.e.Prepare(context.Background())
		return st.Progress != nil && *st.Progress >= 40
	})
	_, err := h.e.Transcribe(context.Background(), pcm(1))
	if kindOf(err) != api.ErrorKindUnavailable || !strings.HasPrefix(err.Error(), "downloading ") {
		t.Fatalf("clip while downloading: %v", err)
	}
	close(gate)
	waitFor(t, "ready", func() bool {
		st, _ := h.e.Prepare(context.Background())
		return st.State == api.SpeechStateReady
	})
	dir := filepath.Join(h.state, "speech", m.Name)
	for n, body := range fakeFiles {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err != nil || string(b) != body {
			t.Fatalf("%s: %v %q", n, err, b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, attributionFile)); !strings.Contains(string(b), "Fake model") {
		t.Fatalf("attribution %q", b)
	}
	if _, err := os.Stat(filepath.Join(h.state, "speech", "escape.txt")); err == nil {
		t.Fatal("an archive path left the model folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "0.wav")); err == nil {
		t.Fatal("a file outside the list was kept")
	}
	if res, err := h.e.Transcribe(context.Background(), pcm(3)); err != nil || res.Text != "clip 3" {
		t.Fatalf("after download: %v %+v", err, res)
	}
	found := false
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	for _, l := range h.logs.lines {
		found = found || strings.Contains(l, "downloading the model")
	}
	if !found {
		t.Fatal("no download log line with the attribution")
	}
	// A new engine on the same state finds the model and downloads nothing.
	e2 := New(Config{Helper: h.e.cfg.Helper, State: h.state, Model: m})
	defer e2.Close()
	if st, _ := e2.Prepare(context.Background()); st.State == api.SpeechStateDownloading || st.State == api.SpeechStateAbsent {
		t.Fatalf("second engine: %+v", st)
	}
}

// TestDownloadFails: a bad checksum, a short body or a missing archive leaves no model, says why,
// and the next prepare tries again.
func TestDownloadFails(t *testing.T) {
	archive := gz(fakeArchive(t))
	srv := serve(t, archive, nil)
	bad := modelFor(srv.URL+"/model.tar", archive)
	bad.SHA256 = strings.Repeat("0", 64)
	short := modelFor(srv.URL+"/model.tar", archive)
	short.Size++
	missing := modelFor(srv.URL+"/nothing.tar", archive)
	for _, tc := range []struct {
		name string
		m    Model
		want string
	}{
		{"checksum", bad, "checksum"},
		{"size", short, "bytes, want"},
		{"missing", missing, "404"},
	} {
		h := newHarness(t, "ok", tc.m, false, nil)
		h.e.Prepare(context.Background())
		var st api.SpeechStatus
		waitFor(t, tc.name+" failure", func() bool {
			st, _ = h.e.Prepare(context.Background())
			return st.Error != ""
		})
		if !strings.Contains(st.Error, tc.want) {
			t.Errorf("%s: %+v", tc.name, st)
		}
		entries, _ := os.ReadDir(filepath.Join(h.state, "speech"))
		for _, en := range entries {
			if !strings.HasPrefix(en.Name(), ".fake-model.download-") || st.State != api.SpeechStateDownloading {
				t.Errorf("%s: left %s (state %s)", tc.name, en.Name(), st.State)
			}
		}
	}
}

// TestDownloadBzip2: the real model is a bzip2 tar; the extractor reads one made by bzip2(1).
func TestDownloadBzip2(t *testing.T) {
	bz, err := exec.LookPath("bzip2")
	if err != nil {
		t.Skip("no bzip2")
	}
	cmd := exec.Command(bz, "-c")
	cmd.Stdin = bytes.NewReader(fakeArchive(t))
	archive, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	srv := serve(t, archive, nil)
	m := modelFor(srv.URL+"/model.tar", archive)
	dir := filepath.Join(t.TempDir(), "speech", m.Name)
	if err := fetch(context.Background(), http.DefaultClient, m, dir, nil); err != nil {
		t.Fatal(err)
	}
	if !present(dir, m) {
		t.Fatal("model not present after a bzip2 download")
	}
}

func TestSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	os.WriteFile(p, []byte("[catalog]\nalways_on = true\n[speech]\nenabled = false\nthreads = 2\nhelper = \"/opt/x\"\n"), 0o600)
	s, err := LoadSettings(p)
	if err != nil || s.Enabled == nil || *s.Enabled || s.Threads != 2 || s.Helper != "/opt/x" {
		t.Fatalf("got %+v %v", s, err)
	}
	if on, why := s.On(64 << 30); on || !strings.Contains(why, "config") {
		t.Fatalf("off in config: %v %s", on, why)
	}
	none, err := LoadSettings(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil || none.Enabled != nil {
		t.Fatalf("missing file: %+v %v", none, err)
	}
	if on, _ := none.On(8 << 30); !on {
		t.Fatal("8 GiB host is off by default")
	}
	if on, why := none.On(2 << 30); on || !strings.Contains(why, "2048 MiB") {
		t.Fatalf("2 GiB host: %v %s", on, why)
	}
	if on, _ := none.On(4 << 30); !on {
		t.Fatal("4 GiB host is off by default")
	}
	os.WriteFile(p, []byte("[speech]\nthreads = -1\n"), 0o600)
	if _, err := LoadSettings(p); err == nil {
		t.Fatal("negative threads passed")
	}
	os.WriteFile(p, []byte("[speech\n"), 0o600)
	if _, err := LoadSettings(p); err == nil {
		t.Fatal("broken file passed")
	}
}

func TestExampleSettings(t *testing.T) {
	s, err := LoadSettings(filepath.Join("..", "..", "docs", "examples", "hab.toml"))
	if err != nil || s.Enabled == nil || !*s.Enabled || s.Threads != 4 || s.Helper != "/usr/local/bin/gff-hab-speech" || s.ModelURL != "" {
		t.Fatalf("example: %+v %v", s, err)
	}
}

func TestFindHelper(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, HelperName)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	noenv := func(string) string { return "" }
	exe := func() (string, error) { return filepath.Join(dir, "gff-hab"), nil }
	if got := FindHelper(Settings{}, noenv, exe); got != bin {
		t.Fatalf("beside gff-hab: %q", got)
	}
	other := filepath.Join(t.TempDir(), "speech-helper")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	if got := FindHelper(Settings{Helper: other}, noenv, exe); got != other {
		t.Fatalf("setting: %q", got)
	}
	env := func(k string) string {
		if k == "GFF_HAB_SPEECH_HELPER" {
			return other
		}
		return ""
	}
	if got := FindHelper(Settings{}, env, exe); got != other {
		t.Fatalf("environment: %q", got)
	}
	if got := FindHelper(Settings{Helper: filepath.Join(dir, "none")}, noenv, exe); got != "" {
		t.Fatalf("a named helper that is missing falls back: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := FindHelper(Settings{}, noenv, func() (string, error) { return "/nonexistent/gff-hab", nil }); got != "" {
		t.Fatalf("none: %q", got)
	}
}
