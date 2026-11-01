package speechproto

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := WriteJSON(&b, KindHello, Hello{Protocol: Version, Formats: []string{"pcm16"}, Engine: "fake"}); err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeTranscribe(Request{ID: 7, Format: "pcm16", SampleRate: 16000}, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(&b, KindTranscribe, payload); err != nil {
		t.Fatal(err)
	}
	f, err := Read(&b)
	if err != nil || f.Kind != KindHello || !bytes.Contains(f.Payload, []byte(`"protocol":1`)) {
		t.Fatalf("hello: %v %+v", err, f)
	}
	f, err = Read(&b)
	if err != nil || f.Kind != KindTranscribe {
		t.Fatalf("transcribe: %v %+v", err, f)
	}
	req, audio, err := DecodeTranscribe(f.Payload)
	if err != nil || req.ID != 7 || req.Format != "pcm16" || req.SampleRate != 16000 || !bytes.Equal(audio, []byte{1, 2, 3, 4}) {
		t.Fatalf("decode: %v %+v %v", err, req, audio)
	}
	if _, err := Read(&b); err != io.EOF {
		t.Fatalf("end: %v, want io.EOF", err)
	}
}

func TestFrameErrors(t *testing.T) {
	if err := Write(io.Discard, KindResult, make([]byte, MaxPayload+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write too large: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte{4, 0xff, 0xff, 0xff, 0xff})); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read too large: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte{4, 0, 0})); err == nil || err == io.EOF {
		t.Fatalf("cut header: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte{4, 0, 0, 0, 5, 'a'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("cut payload: %v", err)
	}
	for _, p := range [][]byte{nil, {0, 0, 0, 9, '{'}, {0, 0, 0, 1, 'x'}} {
		if _, _, err := DecodeTranscribe(p); err == nil {
			t.Fatalf("DecodeTranscribe(%v) passed", p)
		}
	}
}
