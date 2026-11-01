# Speech helper protocol

The wire protocol between gff-hab and its speech helper, `gff-hab-speech`. The behaviour behind
it is in [`docs/specs/voice.md`](../specs/voice.md), Hab speech engine. The code is
`internal/speechproto`; gff-hab's side is `internal/speech`, the helper is `cmd/gff-hab-speech`.
This document and the code must agree.

## 1. Process

- gff-hab starts the helper as its child on the first `speech.prepare` or `speech.transcribe`
  that needs it: `gff-hab-speech -model <dir> -threads <n>`. `<dir>` is the model folder under
  the hab's state folder (`<state>/speech/<model>`).
- The protocol runs over the helper's stdin (gff-hab to helper) and stdout (helper to gff-hab).
  Nothing else is written to stdout.
- stderr carries the helper's log lines. gff-hab copies them into its own log, one line each,
  cut at 4 KiB. The helper never writes audio or text there.
- The helper exits when its stdin ends. gff-hab closes stdin to stop it (after 10 minutes with
  no request, or when gff-hab stops) and kills it 5 s later if it is still running.
- `gff-hab-speech -info` prints the hello (section 3) as one JSON line and exits, with no model
  loaded. gff-hab runs it once, to learn the formats before any model is downloaded.

## 2. Frames

A frame is a 1-byte kind, a 4-byte big-endian payload length, then the payload. A payload is at
most 2 MiB. Every payload is JSON, except the audio of a transcribe frame.

| Kind | Name | Direction | Payload |
|---|---|---|---|
| 1 | hello | helper to gff-hab | `{"protocol": 1, "formats": ["opus", "pcm16"], "engine": "sherpa-onnx 2.0.1", "model": "<dir name>"}` |
| 2 | ready | helper to gff-hab | `{"load_ms": n}` |
| 3 | transcribe | gff-hab to helper | 4-byte big-endian header length, the JSON header `{"id": n, "format": "pcm16", "sample_rate": 16000}`, then the audio bytes |
| 4 | result | helper to gff-hab | `{"id": n, "text": "...", "audio_ms": n, "decode_ms": n}` |
| 5 | error | helper to gff-hab | `{"id": n, "kind": "invalid" or "failed", "message": "..."}` |

## 3. Order

1. The helper sends hello at once, before it loads the model. gff-hab refuses a helper whose
   `protocol` differs from its own and stops it.
2. The helper loads the model, then sends ready, or an error with `id` 0 and exits with a non-zero
   status.
3. gff-hab sends one transcribe frame at a time and waits for its answer: a result or an error
   with the same `id`. `id` counts up from 1 per gff-hab process.
4. An error of kind `invalid` means the clip did not decode (a bad Ogg stream, an odd pcm16
   length). Kind `failed` means the engine failed on a good clip. Either way the helper keeps
   running.

## 4. Audio

- `pcm16`: little-endian signed 16-bit mono samples at `sample_rate` (default 16000). sherpa-onnx
  resamples to the model's 16 kHz.
- `opus`: an Ogg Opus file (RFC 7845) with one logical stream, mono or stereo, mapping family 0.
  libopus decodes it at 16 kHz, mixing stereo down. The pre-skip is dropped and the end is cut at
  the last page's granule position.
- `audio_ms` in a result is the decoded length. gff-hab checks the limits (1 MiB, 120 s) before
  it sends a clip: from the byte count (pcm16), or from the last granule position and the
  packets' own lengths (opus). The helper also stops decoding an Opus clip past 120 s plus its
  pre-skip and answers `invalid`.
- gff-hab kills a helper that does not answer a clip within 30 s plus twice the clip's length,
  or does not send ready within 60 s of its start.

## 5. Versions

`protocol` is 1. A change that an older peer would misread raises it. gff-hab and the helper ship
together, so there is no translation: gff-hab refuses a helper of another version, and
`speech.prepare` answers `unavailable` with the reason.

## 6. Tests

- `internal/speechproto`: the frame codec, cut and oversized frames.
- `internal/speech`: gff-hab's side against a fake helper (`internal/speech/speechtest`, the test
  binary run as the helper): load, ready, results, errors, a crash, a wrong protocol, a helper with
  no hello, the queue, the idle exit.
- `cmd/gff-hab-speech` (`make test-speech`): the helper's serve loop in process, a missing model,
  an Opus round trip, and real recognition when `$GFF_SPEECH_MODEL_DIR` names the model (a
  clip from macOS `say`, else the WAV `$GFF_SPEECH_TEST_WAV` names).
