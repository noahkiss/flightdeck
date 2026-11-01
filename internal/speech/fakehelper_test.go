package speech

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/speechproto"
)

// The fake helper: when GFF_SPEECH_FAKE is set, the test binary is a speech helper speaking
// internal/speechproto, with behaviour set by the environment. Tests point Config.Helper at
// the test binary and set the variables with t.Setenv, so only the child processes see them.
//
//	GFF_SPEECH_FAKE          a mode: ok, load-fail, crash (exit on a clip), invalid (refuse each
//	                         clip), bad-proto (a hello of another protocol), mute (no hello)
//	GFF_SPEECH_FAKE_FORMATS  comma-separated formats (default pcm16,opus)
//	GFF_SPEECH_FAKE_LOAD     load time in ms
//	GFF_SPEECH_FAKE_DECODE   decode time in ms
//	GFF_SPEECH_FAKE_LOG      a file the fake appends its events to: start, exit, begin <n>, end <n>
//	GFF_SPEECH_FAKE_TEXT     the text of each clip (default "clip <first sample>")

func init() {
	mode := os.Getenv("GFF_SPEECH_FAKE")
	if mode == "" {
		return
	}
	os.Exit(fakeHelper(mode, os.Args[1:]))
}

func fakeLog(format string, args ...any) {
	p := os.Getenv("GFF_SPEECH_FAKE_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, format+"\n", args...)
	f.Close()
}

func envMS(k string) time.Duration {
	var n int
	fmt.Sscan(os.Getenv(k), &n)
	return time.Duration(n) * time.Millisecond
}

func fakeHelper(mode string, args []string) int {
	formats := []string{"pcm16", "opus"}
	if f := os.Getenv("GFF_SPEECH_FAKE_FORMATS"); f != "" {
		formats = strings.Split(f, ",")
	}
	hello := speechproto.Hello{Protocol: speechproto.Version, Formats: formats, Engine: "fake"}
	if mode == "bad-proto" {
		hello.Protocol = 99
	}
	if len(args) > 0 && args[0] == "-info" {
		json.NewEncoder(os.Stdout).Encode(hello)
		return 0
	}
	model := ""
	for i, a := range args {
		if a == "-model" && i+1 < len(args) {
			model = args[i+1]
		}
	}
	fakeLog("start")
	defer fakeLog("exit")
	out := bufio.NewWriter(os.Stdout)
	flush := func() { out.Flush() }
	if mode == "mute" {
		io.Copy(io.Discard, os.Stdin)
		return 0
	}
	speechproto.WriteJSON(out, speechproto.KindHello, hello)
	flush()
	time.Sleep(envMS("GFF_SPEECH_FAKE_LOAD"))
	if _, err := os.Stat(model + "/tokens.txt"); err != nil || mode == "load-fail" {
		speechproto.WriteJSON(out, speechproto.KindError, speechproto.Error{Kind: speechproto.ErrFailed, Message: "no model"})
		flush()
		return 1
	}
	fmt.Fprintln(os.Stderr, "loaded") // a log line, passed to gff-hab's log
	speechproto.WriteJSON(out, speechproto.KindReady, speechproto.Ready{LoadMS: 1})
	flush()
	in := bufio.NewReader(os.Stdin)
	for {
		f, err := speechproto.Read(in)
		if err != nil {
			return 0 // stdin ended: gff-hab is done with us
		}
		req, audio, err := speechproto.DecodeTranscribe(f.Payload)
		if err != nil {
			return 2
		}
		var first int16
		if len(audio) >= 2 {
			first = int16(binary.LittleEndian.Uint16(audio))
		}
		fakeLog("begin %d", first)
		switch mode {
		case "crash":
			return 3
		case "invalid":
			speechproto.WriteJSON(out, speechproto.KindError, speechproto.Error{ID: req.ID, Kind: speechproto.ErrInvalid, Message: "not audio"})
			flush()
			continue
		}
		time.Sleep(envMS("GFF_SPEECH_FAKE_DECODE"))
		text := os.Getenv("GFF_SPEECH_FAKE_TEXT")
		if text == "" {
			text = fmt.Sprintf("clip %d", first)
		}
		fakeLog("end %d", first)
		speechproto.WriteJSON(out, speechproto.KindResult, speechproto.Result{ID: req.ID, Text: text, DecodeMS: 5})
		flush()
	}
}
