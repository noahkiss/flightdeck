package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/speech"
	"github.com/noahkiss/flightdeck/internal/speech/speechtest"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The test binary is also the fake speech helper: gff-hab serve, started by a test with
// speechtest.Env in its environment, runs this binary as its helper.
func init() { speechtest.RunIfFake() }

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestSpeechSetup: the engine runs only with a helper installed and the host setting on; the
// default follows the host's memory; a broken config file is an error.
func TestSpeechSetup(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "hab.toml")
	exe := testExe(t)
	env := func(helper string, mem uint64) speechEnv {
		return speechEnv{
			getenv:     func(k string) string { return map[string]string{"GFF_HAB_SPEECH_HELPER": helper}[k] },
			executable: func() (string, error) { return filepath.Join(d, "gff-hab"), nil },
			memory:     mem,
		}
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	check := func(name, config string, e speechEnv, want bool, why string) {
		t.Helper()
		os.WriteFile(cfg, []byte(config), 0o600)
		logs = nil
		sp, err := speechSetup(cfg, d, nil, e, logf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sp != nil {
			sp.Close()
		}
		if (sp != nil) != want || len(logs) != 1 || !strings.Contains(logs[0], why) {
			t.Errorf("%s: engine %v, logs %q; want %v %q", name, sp != nil, logs, want, why)
		}
	}
	check("default on", "", env(exe, 16<<30), true, "on by default")
	check("small host", "", env(exe, 2<<30), false, "under 3584 MiB")
	check("small host, turned on", "[speech]\nenabled = true\n", env(exe, 2<<30), true, "on in the config file")
	check("turned off", "[speech]\nenabled = false\n", env(exe, 64<<30), false, "off in the config file")
	check("no helper", "", env(filepath.Join(d, "missing"), 16<<30), false, "gff-hab-speech is not installed")
	os.WriteFile(cfg, []byte("[speech\n"), 0o600)
	if _, err := speechSetup(cfg, d, nil, env(exe, 16<<30), logf); err == nil {
		t.Fatal("a broken config file passed")
	}
}

// TestSpeechVocab: a session's vocabulary is the file and folder names git lists in its
// project's folder; a session with no project, or an unknown one, has none.
func TestSpeechVocab(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	repo := filepath.Join(d, "repo")
	os.MkdirAll(filepath.Join(repo, "internal", "holderreg"), 0o700)
	os.WriteFile(filepath.Join(repo, "internal", "holderreg", "registry.go"), []byte("package holderreg\n"), 0o600)
	os.WriteFile(filepath.Join(repo, "fetchUser.ts"), nil, 0o600)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("secret.env\n"), 0o600)
	os.WriteFile(filepath.Join(repo, "secret.env"), nil, 0o600)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	fh := structure.NewFakeHolder(structure.HolderPane{ID: "p1", Cols: 80, Rows: 24}, structure.HolderPane{ID: "p2", Cols: 80, Rows: 24})
	st, err := structure.Open(structure.Config{Dir: filepath.Join(d, "structure"), Holders: fh})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	proj, err := st.CreateProject("repo", "example.com/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectDir(proj.ID, repo); err != nil {
		t.Fatal(err)
	}
	filed, err := st.CreateSession(structure.SessionSpec{Name: "filed", Project: proj.ID, Pane: "p1", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	unfiled, err := st.CreateSession(structure.SessionSpec{Name: "unfiled", Pane: "p2", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	v := &speechVocab{store: st, cache: map[string]vocabEntry{}}
	terms, ok := v.terms(t.Context(), filed.ID)
	for _, want := range []string{"holderreg", "registry.go", "fetchUser.ts", "fetchUser", ".gitignore"} {
		if !ok || !slices.Contains(terms, want) {
			t.Errorf("terms %q lack %q", terms, want)
		}
	}
	if slices.Contains(terms, "secret.env") {
		t.Errorf("an ignored file is in the vocabulary: %q", terms)
	}
	if _, ok := v.terms(t.Context(), unfiled.ID); ok {
		t.Error("an unfiled session has a vocabulary")
	}
	if _, ok := v.terms(t.Context(), "nope"); ok {
		t.Error("an unknown session has a vocabulary")
	}
}

// installFakeModel puts files where the Parakeet model goes, with its checksum stamp, so gff-hab
// finds the model downloaded. The fake helper only checks tokens.txt.
func installFakeModel(t *testing.T, state string) {
	t.Helper()
	dir := filepath.Join(state, "speech", speech.Parakeet.Name)
	os.MkdirAll(dir, 0o700)
	for _, f := range speech.Parakeet.Files {
		os.WriteFile(filepath.Join(dir, f), []byte("fake"), 0o600)
	}
	os.WriteFile(filepath.Join(dir, ".complete"), []byte(speech.Parakeet.SHA256+"\n"), 0o600)
}

// tone is the hand-made fixture clip: ms milliseconds of a 440 Hz tone, pcm16 at 16 kHz.
func tone(ms int) []byte {
	s := make([]int16, 16*ms)
	for i := range s {
		s[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	s[0] = 42
	return speech.PCM16(s)
}

// TestServeSpeech: gff-hab serve with the helper installed lists the speech capability and
// transcribes through it; with the engine turned off in the config file it lists no capability
// and the methods answer unavailable.
func TestServeSpeech(t *testing.T) {
	dir := shortDir(t)
	state := filepath.Join(dir, "state")
	installFakeModel(t, state)
	sock := filepath.Join(dir, "s")
	env := []string{"GFF_HAB_SPEECH_HELPER=" + testExe(t), speechtest.Env + "=ok", "GFF_SPEECH_FAKE_TEXT=hello from the hab"}
	s := startServeWith(t, sock, state, env)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "speech") {
		t.Fatalf("capabilities %v lack speech", w.Capabilities)
	}
	var st api.SpeechStatus
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "speech.prepare")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Model != speech.Parakeet.Name || !slices.Equal(st.Formats, []api.SpeechFormat{"opus", "pcm16"}) ||
		(st.State != api.SpeechStateLoading && st.State != api.SpeechStateReady) {
		t.Fatalf("prepare: %+v", st)
	}
	params := fmt.Sprintf(`{"audio":%q,"format":"pcm16"}`, base64.StdEncoding.EncodeToString(tone(400)))
	var tr api.SpeechTranscript
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "speech.transcribe", params)), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Text != "hello from the hab" || tr.AudioMS != 400 {
		t.Fatalf("transcribe: %+v", tr)
	}
	s.stop(t)

	dir2 := shortDir(t)
	cfg := filepath.Join(dir2, "hab.toml")
	os.WriteFile(cfg, []byte("[speech]\nenabled = false\n"), 0o600)
	sock2 := filepath.Join(dir2, "s")
	startServeWith(t, sock2, filepath.Join(dir2, "state"), env, "-config", cfg)
	if w := welcome(t, sock2); slices.Contains(w.Capabilities, "speech") {
		t.Fatalf("capabilities %v list speech with the engine off", w.Capabilities)
	}
	err := exec.Command(bin, "api", "call", "-socket", sock2, "speech.prepare").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("speech.prepare with the engine off: %v, want exit 3", err)
	}
}
