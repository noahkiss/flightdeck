//go:build speech

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/oggopus"
	"github.com/noahkiss/flightdeck/internal/speechproto"
)

// tone is hand-made audio: ms milliseconds of a 440 Hz tone at 16 kHz.
func tone(ms int) []int16 {
	s := make([]int16, 16*ms)
	for i := range s {
		s[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	return s
}

func le(s []int16) []byte {
	b := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func rms(f []float32) float64 {
	var sum float64
	for _, v := range f {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(f)))
}

func TestPCM16(t *testing.T) {
	f, err := pcm16([]byte{0x00, 0x80, 0xff, 0x7f, 0, 0})
	if err != nil || len(f) != 3 || f[0] != -1 || f[1] < 0.9999 || f[2] != 0 {
		t.Fatalf("got %v %v", f, err)
	}
	if _, err := pcm16([]byte{1}); err == nil {
		t.Fatal("odd length passed")
	}
}

// TestOpusRoundTrip: a clip encoded as the app sends it (Ogg Opus, mono, 16 kHz, 20 ms packets,
// 24 kbit/s) decodes to the same length and about the same level.
func TestOpusRoundTrip(t *testing.T) {
	src := tone(1000)
	ogg, err := encodeOgg(src, 24000)
	if err != nil {
		t.Fatal(err)
	}
	s, err := oggopus.Parse(ogg)
	if err != nil || s.DurationMS() != 1000 || len(s.Packets) != 51 { // 50 plus one for the lookahead
		t.Fatalf("parse: %v, %d ms, %d packets", err, s.DurationMS(), len(s.Packets))
	}
	got, err := decodeOpus(ogg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(src) {
		t.Fatalf("decoded %d samples, want %d", len(got), len(src))
	}
	want, _ := pcm16(le(src))
	if r, w := rms(got[1600:]), rms(want[1600:]); math.Abs(r-w)/w > 0.3 {
		t.Fatalf("level %.3f, want about %.3f", r, w)
	}
	t.Logf("1 s at 24 kbit/s: %d bytes", len(ogg))
}

// TestOpusCap: the helper refuses to decode past the 120 s limit, whatever gff-hab let through.
func TestOpusCap(t *testing.T) {
	ogg, err := encodeOgg(make([]int16, 16000*125), 16000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOpus(ogg); err == nil || !strings.Contains(err.Error(), "longer than 120000 ms") {
		t.Fatalf("125 s clip: %v", err)
	}
	ogg, _ = encodeOgg(make([]int16, 16000*119), 16000)
	if got, err := decodeOpus(ogg); err != nil || len(got) != 16000*119 {
		t.Fatalf("119 s clip: %d samples, %v", len(got), err)
	}
}

func TestTranscribeRefuses(t *testing.T) {
	for _, req := range []speechproto.Request{{ID: 1, Format: "opus"}, {ID: 2, Format: "pcm16"}, {ID: 3, Format: "mp3"}} {
		audio := []byte{1, 2, 3}
		_, perr := transcribe(nil, req, audio)
		if perr == nil || perr.ID != req.ID || perr.Kind != speechproto.ErrInvalid {
			t.Errorf("%+v: %+v", req, perr)
		}
	}
}

// helperConn runs serve in process over pipes and returns its frames and a writer.
type helperConn struct {
	in   *io.PipeWriter
	out  *bufio.Reader
	code chan int
}

func startServe(t *testing.T, model string) *helperConn {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &helperConn{in: inW, out: bufio.NewReader(outR), code: make(chan int, 1)}
	go func() {
		h.code <- serve(inR, outW, speechproto.Hello{Protocol: speechproto.Version, Formats: []string{"opus", "pcm16"}, Engine: engineName()}, model, 4)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return h
}

func (h *helperConn) read(t *testing.T, want speechproto.Kind, v any) {
	t.Helper()
	f, err := speechproto.Read(h.out)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != want {
		t.Fatalf("frame kind %d (%s), want %d", f.Kind, f.Payload, want)
	}
	if err := json.Unmarshal(f.Payload, v); err != nil {
		t.Fatal(err)
	}
}

// TestServeNoModel: a folder with no model gives a hello, then a load error with id 0.
func TestServeNoModel(t *testing.T) {
	h := startServe(t, t.TempDir())
	var hello speechproto.Hello
	h.read(t, speechproto.KindHello, &hello)
	if hello.Protocol != speechproto.Version || !strings.HasPrefix(hello.Engine, "sherpa-onnx ") {
		t.Fatalf("hello %+v", hello)
	}
	var e speechproto.Error
	h.read(t, speechproto.KindError, &e)
	if e.ID != 0 || !strings.Contains(e.Message, "encoder.int8.onnx") {
		t.Fatalf("error %+v", e)
	}
	if code := <-h.code; code != 1 {
		t.Fatalf("exit %d", code)
	}
}

// spoken makes a clip of synthetic speech with macOS say, 16 kHz pcm16. It skips without say.
func spoken(t *testing.T, text string) []int16 {
	t.Helper()
	say, err := exec.LookPath("say")
	if err != nil {
		t.Skip("no say(1) to make a spoken clip")
	}
	wav := filepath.Join(t.TempDir(), "clip.wav")
	if out, err := exec.Command(say, "--file-format=WAVE", "--data-format=LEI16@16000", "-o", wav, text).CombinedOutput(); err != nil {
		t.Skipf("say: %v %s", err, out)
	}
	b, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte("data"))
	if i < 0 || len(b) < i+8 {
		t.Fatal("no data chunk")
	}
	data := b[i+8:]
	s := make([]int16, len(data)/2)
	for j := range s {
		s[j] = int16(binary.LittleEndian.Uint16(data[2*j:]))
	}
	return s
}

// TestRecognize is the real engine end to end: the model in $GFF_SPEECH_MODEL_DIR (the
// extracted Parakeet archive) transcribes one spoken clip sent as pcm16 and as Ogg Opus.
// It skips when the variable is unset.
func TestRecognize(t *testing.T) {
	model := os.Getenv("GFF_SPEECH_MODEL_DIR")
	if model == "" {
		t.Skip("GFF_SPEECH_MODEL_DIR is unset")
	}
	samples := spoken(t, "Please run the tests again and then open the pull request.")
	ogg, err := encodeOgg(samples, 24000)
	if err != nil {
		t.Fatal(err)
	}
	h := startServe(t, model)
	var hello speechproto.Hello
	h.read(t, speechproto.KindHello, &hello)
	var ready speechproto.Ready
	h.read(t, speechproto.KindReady, &ready)
	t.Logf("model loaded in %d ms", ready.LoadMS)
	for i, c := range []struct {
		format string
		audio  []byte
	}{{"pcm16", le(samples)}, {"opus", ogg}} {
		p, _ := speechproto.EncodeTranscribe(speechproto.Request{ID: uint64(i + 1), Format: c.format, SampleRate: 16000}, c.audio)
		if err := speechproto.Write(h.in, speechproto.KindTranscribe, p); err != nil {
			t.Fatal(err)
		}
		var r speechproto.Result
		h.read(t, speechproto.KindResult, &r)
		t.Logf("%s: %q, %d ms of audio in %d ms", c.format, r.Text, r.AudioMS, r.DecodeMS)
		low := strings.ToLower(r.Text)
		if r.ID != uint64(i+1) || !strings.Contains(low, "run the tests") || !strings.Contains(low, "pull request") {
			t.Errorf("%s: %+v", c.format, r)
		}
		if want := len(samples) * 1000 / 16000; r.AudioMS < want-20 || r.AudioMS > want+20 {
			t.Errorf("%s: audio_ms %d, want about %d", c.format, r.AudioMS, want)
		}
	}
	h.in.Close()
	if code := <-h.code; code != 0 {
		t.Fatalf("exit %d after stdin ended", code)
	}
}
