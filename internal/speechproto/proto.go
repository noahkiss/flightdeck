// Package speechproto is the wire protocol between gff-hab and its speech helper,
// gff-hab-speech. gff-hab starts the helper as a child and speaks this protocol over the
// helper's stdin and stdout. Spec: docs/protocol/speech-helper.md.
//
// A frame is a 1-byte kind, a 4-byte big-endian payload length, then the payload. Every payload
// is JSON except the audio of a transcribe frame. The helper exits when its stdin ends.
package speechproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version. The helper sends it in its hello; gff-hab refuses another.
const Version = 1

// MaxPayload bounds one frame's payload: a 1 MiB clip, its header and room to spare.
const MaxPayload = 2 << 20

// Kind is the first byte of a frame.
type Kind byte

const (
	// KindHello (helper to hab) is the helper's first frame, sent before it loads the model.
	KindHello Kind = 1
	// KindReady (helper to hab) says the model is loaded.
	KindReady Kind = 2
	// KindTranscribe (hab to helper) is one clip: a 4-byte big-endian header length, the JSON
	// header (Request), then the audio bytes.
	KindTranscribe Kind = 3
	// KindResult (helper to hab) is the text of one clip.
	KindResult Kind = 4
	// KindError (helper to hab) is a failed load (id 0) or a failed clip.
	KindError Kind = 5
)

// Hello is the payload of a hello frame, and what `gff-hab-speech -info` prints.
type Hello struct {
	Protocol int      `json:"protocol"`
	Formats  []string `json:"formats"`         // the audio formats the helper decodes: opus, pcm16
	Engine   string   `json:"engine"`          // the runtime and its version, such as sherpa-onnx 2.0.1
	Model    string   `json:"model,omitempty"` // the model it loads, when started with one
}

// Ready is the payload of a ready frame.
type Ready struct {
	LoadMS int `json:"load_ms"`
}

// Request is the header of a transcribe frame.
type Request struct {
	ID         uint64 `json:"id"`
	Format     string `json:"format"`                // opus or pcm16
	SampleRate int    `json:"sample_rate,omitempty"` // for pcm16
}

// Result is the payload of a result frame.
type Result struct {
	ID       uint64 `json:"id"`
	Text     string `json:"text"`
	AudioMS  int    `json:"audio_ms"`
	DecodeMS int    `json:"decode_ms"`
}

// Error kinds.
const (
	ErrInvalid = "invalid" // the clip does not decode
	ErrFailed  = "failed"  // the engine failed: a load, or a decode of a good clip
)

// Error is the payload of an error frame. ID 0 means the model load failed; the helper exits.
type Error struct {
	ID      uint64 `json:"id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Frame is one decoded frame.
type Frame struct {
	Kind    Kind
	Payload []byte
}

// ErrTooLarge is returned for a frame whose length is over MaxPayload.
var ErrTooLarge = errors.New("speechproto: frame too large")

// Write writes one frame.
func Write(w io.Writer, k Kind, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrTooLarge
	}
	var h [5]byte
	h[0] = byte(k)
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON writes one frame with v as its JSON payload.
func WriteJSON(w io.Writer, k Kind, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return Write(w, k, b)
}

// Read reads one frame. It returns io.EOF only when the stream ends between frames.
func Read(r io.Reader) (Frame, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Frame{}, fmt.Errorf("speechproto: cut frame header: %w", err)
		}
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxPayload {
		return Frame{}, ErrTooLarge
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, fmt.Errorf("speechproto: cut frame: %w", err)
	}
	return Frame{Kind: Kind(h[0]), Payload: p}, nil
}

// EncodeTranscribe builds a transcribe frame's payload.
func EncodeTranscribe(req Request, audio []byte) ([]byte, error) {
	hdr, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	p := make([]byte, 4+len(hdr)+len(audio))
	binary.BigEndian.PutUint32(p, uint32(len(hdr)))
	copy(p[4:], hdr)
	copy(p[4+len(hdr):], audio)
	return p, nil
}

// DecodeTranscribe splits a transcribe frame's payload into its header and audio. The audio
// aliases p.
func DecodeTranscribe(p []byte) (Request, []byte, error) {
	var req Request
	if len(p) < 4 {
		return req, nil, errors.New("speechproto: transcribe frame shorter than its header length")
	}
	n := binary.BigEndian.Uint32(p)
	if uint64(n) > uint64(len(p)-4) {
		return req, nil, errors.New("speechproto: transcribe header runs past the frame")
	}
	if err := json.Unmarshal(p[4:4+n], &req); err != nil {
		return req, nil, fmt.Errorf("speechproto: transcribe header: %w", err)
	}
	return req, p[4+n:], nil
}
