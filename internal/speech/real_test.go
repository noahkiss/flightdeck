package speech

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestRealHelper runs the engine against the real helper and model: $GFF_SPEECH_HELPER_BIN
// (build/gff-hab-speech from `make speech-helper`) and $GFF_SPEECH_MODEL_DIR (the extracted
// Parakeet archive). It skips when either is unset. The model files are linked into the state
// folder with the checksum stamp, so nothing is downloaded or copied.
func TestRealHelper(t *testing.T) {
	bin, model := os.Getenv("GFF_SPEECH_HELPER_BIN"), os.Getenv("GFF_SPEECH_MODEL_DIR")
	if bin == "" || model == "" {
		t.Skip("GFF_SPEECH_HELPER_BIN or GFF_SPEECH_MODEL_DIR is unset")
	}
	state := t.TempDir()
	dir := filepath.Join(state, "speech", Parakeet.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range Parakeet.Files {
		if err := os.Symlink(filepath.Join(model, f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, stampFile), []byte(Parakeet.SHA256+"\n"), 0o600)
	e := New(Config{Helper: bin, State: state, Threads: 4, Logf: t.Logf,
		Vocabulary: func(context.Context, string) ([]string, bool) {
			return Terms([]string{"internal/holderreg/registry.go"}), true
		}})
	defer e.Close()
	st, err := e.Prepare(context.Background())
	if err != nil || st.State != api.SpeechStateLoading || len(st.Formats) != 2 {
		t.Fatalf("prepare: %v %+v", err, st)
	}
	waitFor(t, "ready", func() bool {
		st, _ := e.Prepare(context.Background())
		return st.State == api.SpeechStateReady
	})
	p := api.SpeechTranscribeParams{Audio: clip(0, 500), Format: api.SpeechFormatPcm16, Session: "s"}
	want := ""
	if say, err := exec.LookPath("say"); err == nil {
		wav := filepath.Join(t.TempDir(), "c.wav")
		if exec.Command(say, "--file-format=WAVE", "--data-format=LEI16@16000", "-o", wav, "Look in holder reg first.").Run() == nil {
			b, _ := os.ReadFile(wav)
			if i := bytes.Index(b, []byte("data")); i >= 0 {
				p.Audio, want = b[i+8:], "holderreg"
			}
		}
	}
	start := time.Now()
	res, err := e.Transcribe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text %q raw %q, %d ms of audio, decode %d ms, call %v", res.Text, res.Raw, res.AudioMS, res.DecodeMS, time.Since(start))
	if want != "" && !strings.Contains(res.Text, want) {
		t.Errorf("text %q lacks %q", res.Text, want)
	}
}
