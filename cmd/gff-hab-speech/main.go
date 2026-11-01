//go:build speech

// Command gff-hab-speech is gff-hab's speech helper (docs/specs/voice.md, Hab speech engine).
// gff-hab starts it as a child and speaks docs/protocol/speech-helper.md over its stdin and
// stdout. It loads one speech model through sherpa-onnx's C API, decodes pcm16 and Ogg Opus
// clips (Opus through libopus), and returns their text. It never writes audio or text anywhere
// but its stdout, and exits when its stdin ends.
//
// It needs cgo, so it builds only with the speech tag: `make speech-helper`.
//
//	gff-hab-speech -info                       print the hello (protocol, formats) and exit
//	gff-hab-speech -model dir [-threads n]     serve on stdin and stdout
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/speechproto"
)

func main() {
	info := flag.Bool("info", false, "print the hello as JSON and exit")
	model := flag.String("model", "", "the model folder: encoder, decoder and joiner .onnx files and tokens.txt")
	threads := flag.Int("threads", 4, "decode threads")
	flag.Parse()
	hello := speechproto.Hello{Protocol: speechproto.Version, Formats: []string{"opus", "pcm16"}, Engine: engineName()}
	if *info {
		json.NewEncoder(os.Stdout).Encode(hello)
		return
	}
	if *model == "" {
		fmt.Fprintln(os.Stderr, "gff-hab-speech: -model is required")
		os.Exit(2)
	}
	hello.Model = filepath.Base(*model)
	os.Exit(serve(os.Stdin, os.Stdout, hello, *model, *threads))
}

// serve speaks the protocol until in ends, and returns the exit code.
func serve(in io.Reader, out io.Writer, hello speechproto.Hello, model string, threads int) int {
	w := bufio.NewWriter(out)
	send := func(k speechproto.Kind, v any) error {
		if err := speechproto.WriteJSON(w, k, v); err != nil {
			return err
		}
		return w.Flush()
	}
	if send(speechproto.KindHello, hello) != nil {
		return 1
	}
	start := time.Now()
	rec, err := newRecognizer(model, threads)
	if err != nil {
		send(speechproto.KindError, speechproto.Error{Kind: speechproto.ErrFailed, Message: err.Error()})
		return 1
	}
	defer rec.close()
	if send(speechproto.KindReady, speechproto.Ready{LoadMS: int(time.Since(start).Milliseconds())}) != nil {
		return 1
	}
	r := bufio.NewReader(in)
	for {
		f, err := speechproto.Read(r)
		if err == io.EOF {
			return 0
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "gff-hab-speech: %v\n", err)
			return 1
		}
		if f.Kind != speechproto.KindTranscribe {
			fmt.Fprintf(os.Stderr, "gff-hab-speech: unexpected frame kind %d\n", f.Kind)
			return 1
		}
		req, audio, err := speechproto.DecodeTranscribe(f.Payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gff-hab-speech: %v\n", err)
			return 1
		}
		res, perr := transcribe(rec, req, audio)
		if perr != nil {
			err = send(speechproto.KindError, *perr)
		} else {
			err = send(speechproto.KindResult, res)
		}
		if err != nil {
			return 1
		}
	}
}

// transcribe decodes one clip and runs the recognizer over it.
func transcribe(rec *recognizer, req speechproto.Request, audio []byte) (speechproto.Result, *speechproto.Error) {
	var (
		samples []float32
		rate    int
		err     error
	)
	switch req.Format {
	case "pcm16":
		rate = req.SampleRate
		if rate == 0 {
			rate = 16000
		}
		samples, err = pcm16(audio)
	case "opus":
		rate = opusRate
		samples, err = decodeOpus(audio)
	default:
		err = fmt.Errorf("format %q", req.Format)
	}
	if err != nil {
		return speechproto.Result{}, &speechproto.Error{ID: req.ID, Kind: speechproto.ErrInvalid, Message: err.Error()}
	}
	start := time.Now()
	text := rec.recognize(samples, rate)
	return speechproto.Result{
		ID:       req.ID,
		Text:     strings.TrimSpace(text),
		AudioMS:  int(int64(len(samples)) * 1000 / int64(rate)),
		DecodeMS: int(time.Since(start).Milliseconds()),
	}, nil
}

// pcm16 turns little-endian 16-bit samples into floats in [-1, 1).
func pcm16(b []byte) ([]float32, error) {
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("a pcm16 clip has an even number of bytes")
	}
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = float32(int16(uint16(b[2*i])|uint16(b[2*i+1])<<8)) / 32768
	}
	return out, nil
}
