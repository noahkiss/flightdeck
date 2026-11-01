# Hab Parakeet spike

Measures NVIDIA Parakeet TDT 0.6B v2 (English) on a hab host, run through the
[sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) command-line tool. The result sizes
the hab speech engine, the default in `docs/specs/voice.md`. The on-device option has its
own spike in `swift/Spikes/ParakeetSpike`.

This is a throwaway spike, not part of gff-hab. It adds nothing to `go.mod`.

## What it does

`run.sh` speaks the three sentences in `clips.txt` with macOS `say` into 16 kHz mono WAV
files (about 5 s, 15 s and 60 s), with code identifiers in them. It runs
`sherpa-onnx-offline` on each clip three times and prints a table with:

- the audio length, the model load time and the decode time;
- the real-time factor (RTF: decode time divided by audio time);
- peak resident memory of the process, from `/usr/bin/time`;
- word error rate (WER) from `wer.py`, after it normalises case, punctuation and symbols
  ("link_test.go" and "link test go" match), and how many identifiers came out written
  as code;
- the clip size as WAV and as 24 kbit/s AAC (the size a client would send).

## Run it

Models and binaries stay outside the repo. Download them into a scratch folder:

```sh
mkdir -p /tmp/parakeet && cd /tmp/parakeet
rel=https://github.com/k2-fsa/sherpa-onnx/releases/download
curl -LO $rel/v2.0.1/sherpa-onnx-v2.0.1-osx-arm64-static-no-tts.tar.bz2
curl -LO $rel/asr-models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2
for f in *.tar.bz2; do tar xjf "$f"; done
```

On Linux, take `sherpa-onnx-v2.0.1-linux-x64-static-no-tts.tar.bz2` or
`sherpa-onnx-v2.0.1-linux-aarch64-static.tar.bz2` instead. Then, from the repo root:

```sh
SHERPA_BIN=/tmp/parakeet/sherpa-onnx-v2.0.1-osx-arm64-static-no-tts/bin/sherpa-onnx-offline \
PARAKEET_DIR=/tmp/parakeet/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8 \
WORK=/tmp/parakeet/work \
spikes/hab-parakeet/run.sh
```

`THREADS` (default 4) and `RUNS` (default 3) change the run. Linux has no `say`: put
`5s.wav`, `15s.wav` and `60s.wav` (16 kHz mono) in `$WORK` first, read from `clips.txt`.

## Result (2026-11-01)

Apple M1 Pro, 16 GB, macOS 27, sherpa-onnx 2.0.1 CLI, CPU provider, 4 threads, int8 model.
The machine was busy with other work (load average 14 to 57), so decode times are upper
bounds. Load times are with the model files in the file cache; a first load from disk is
slower and was not measured.

| Clip | Audio | Load | Decode | RTF | Peak memory | WER | Identifiers as code |
|---|---|---|---|---|---|---|---|
| 5 s | 4.8 s | 0.94 to 1.12 s | 0.48 to 0.94 s | 0.10 to 0.20 | 1.12 to 1.14 GB | 0 / 16 | 0 / 2 |
| 15 s | 15.2 s | 0.93 to 1.00 s | 1.52 to 2.26 s | 0.10 to 0.15 | 1.19 to 1.20 GB | 0 / 53 | 0 / 2 |
| 60 s | 59.7 s | 0.92 to 1.02 s | 5.63 to 6.13 s | 0.09 to 0.10 | 1.49 to 1.52 GB | 13 / 194 (6.7%) | 0 / 5 |

Findings:

- **Speed is fine.** A 15 s turn decodes in about 2 s on a busy laptop. A hab that keeps
  the model loaded answers in the decode time alone.
- **Memory is the cost.** The int8 model takes about 1.1 GB resident once loaded, and
  grows with clip length (1.5 GB at 60 s). The files are 661 MB on disk (encoder 652 MB).
  So the hab loads the model on demand and unloads it when idle; a load takes about 1 s.
- **Plain English is exact; jargon is not.** Every error in the 60 s clip is a project
  word: "hab" as "hat", "holderreg" as "holder egg", "gff-hab" as "gfff hat", "go vet" as
  "govet", "race" as "erase", "changelog" as "change log". No identifier came out written
  as code: `fetchUser` is "fetch user". The session vocabulary fix-up (`voice.md`) is
  needed on every engine. `say` is a synthetic voice, so real speech will score worse.
- **Threads.** On the 15 s clip: 1 thread RTF 0.30, 2 threads 0.15, 4 threads 0.10 to
  0.15, 8 threads 0.12. The Core ML provider (`--provider=coreml`) gave RTF 0.087 with the
  same load time and memory: no clear gain on a busy machine.
- **Wire size.** As 24 kbit/s AAC the clips are 19 KB, 54 KB and 188 KB (WAV: 153 KB,
  479 KB, 1.8 MB). About 3 KB per second of speech.

## Linux

The same sherpa-onnx release ships static CLI builds for `linux-x64` and `linux-aarch64`
(CPU, onnxruntime linked in), plus CUDA builds. The model files are the same on every
platform. Nothing in the spike is macOS-only except `say` and `afconvert`. A Linux run is
still to do; expect a similar memory figure and a decode speed that depends on the CPU.

## Licences

| Part | Licence |
|---|---|
| sherpa-onnx | Apache-2.0 |
| onnxruntime (linked into the sherpa-onnx binaries) | MIT |
| Parakeet TDT 0.6B v2 model | CC-BY-4.0 (attribution to NVIDIA) |
| Silero VAD (for streaming input; not used here) | MIT |

All allow redistribution. CC-BY-4.0 needs an attribution line wherever the model ships or
is downloaded.

## What this means for gff-hab

- sherpa-onnx's Go bindings need cgo. gff-hab must build with `CGO_ENABLED=0`
  (`docs/layout.md`), so the engine runs as a separate helper process that gff-hab starts
  on demand and talks to over stdio. A crash or the memory cost stays out of gff-hab.
- The model downloads on first use into the hab's state folder, not with the release.
