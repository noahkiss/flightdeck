# Voice mode: models and pipeline

This spec picks the speech models and the pipeline behind voice mode. The user experience is in
`app.md` 13. Researched 2026-10-31; engine order changed 2026-11-01 (hab first). Facts marked *(unverified)* come from blogs or aggregators, not primary sources.

## Decisions

| Question | Decision |
|---|---|
| Cloud features | On the owner's own hosts and devices by default, and free. Cloud summaries, cloud voices and realtime speech APIs are opt-in settings that use the user's own key. No Flightdeck service sits in the path |
| Spoken summary | In voice mode the app appends a request for a two-sentence spoken summary to each spoken prompt. The app reads that summary aloud and skips code, diffs and tables |
| Devices | iPhone and iPad in the first build; the Mac follows with the same speech APIs |
| Languages | English only (owner, 2026-10-31). So the engine is Parakeet TDT 0.6B v2, the English model: more accurate than the multilingual v3 and slightly smaller |
| Speech engine | NVIDIA Parakeet, because it handles software-engineering speech better (owner, 2026-10-31). It runs **on the hab by default**, as Paseo does (owner, 2026-11-01): a hab is always connected when the owner talks to an agent, no device carries a 440 MB model, and the app, `gff` and the Mac get one quality. Options, in order: Parakeet on the device (Core ML, downloaded on first use), then Apple SpeechAnalyzer. A hosted Parakeet endpoint with the user's key (about $0.0015 per audio minute in October 2026) is an opt-in option. All engines sit behind one interface |

## Recommendation

First build: transcription on the hab, with no extra model in the loop.

1. **Speech-to-text:** the app records the turn and sends it to the session's hab (`speech.transcribe`, below). The hab runs Parakeet TDT 0.6B v2 and returns the text. While the owner speaks, Apple SpeechAnalyzer shows live words on screen (a system process, so it costs the app almost no memory); the hab's text replaces them when the turn ends. Options: Parakeet on the device, then Apple's engine alone.
2. **Identifier fix-up:** the session vocabulary (file names, symbols) lives on the hab. With the hab engine, the hab applies it before it returns the text. With an on-device engine, the hab sends the vocabulary; the app passes it as recognizer hints and fuzzy-matches the transcript against it.
3. **Turn in:** the cleaned transcript goes straight into the session as a normal prompt.
4. **Turn out:** speak only the agent's final text. A deterministic trimmer drops code, diffs and tables ("3 files changed"). Voice mode may append "end with a two-sentence spoken summary" to each prompt, so the agent summarizes from full context at no extra cost.
5. **Text-to-speech:** Apple premium system voices. Barge-in stops playback when speech starts.
6. **Approvals:** a fixed grammar, never an LLM. The app reads back the command and accepts only "allow" or "deny".

Upgrade path, each a setting: Kokoro-82M voice; a long-reply summarizer (Apple Foundation Models, then a cloud model with the user's key); a realtime speech API as an optional front end.

## Where transcription runs

Three engines sit behind one interface. The default is the hab; the app falls back down the list.

| Order | Engine | Runs on | Memory cost to the app | Notes |
|---|---|---|---|---|
| 1 (default) | Transcription on the hab | The session's host | None on the phone | The app sends the recorded turn, compressed (about 3 KB per second), over the existing SSH connection. The hab runs Parakeet through sherpa-onnx (CPU on Linux and macOS; a Core ML port is an option on a Mac) and returns text. Adds one round trip plus the decode: about 2 s for a 15 s turn (spike below) |
| 2 | Parakeet on device | The phone's Neural Engine | The model loads into the app: about 0.5 to 0.8 GB peak for a 0.6B model (a streaming 0.6B port measured 757 MB peak on an iPhone 15 Pro Max) | For a hab that cannot run the engine, or an owner who wants the device independent of the hab. Load only while voice mode is open; unload on exit and on a memory warning |
| 3 | Apple SpeechAnalyzer | A system process | Almost none: the model lives outside the app | Always available. It gives the live words during a turn whatever the engine, and it is the last fallback |

Rules:

- **Fallback.** A hab without the `speech` capability, or one that answers `unavailable`, sends
  the app to the next engine the owner enabled. The voice view's footer names the engine that
  wrote the text. The app never sends audio to a cloud service unless the owner turned that option on.
- **On-device sizing.** The on-device test app (`swift/Spikes/ParakeetSpike`) still runs. It
  measures peak memory, load time, speed and battery on the oldest supported iPhone and on the
  owner's devices. If peak memory exceeds the budget (default 1 GB, or the app is killed in
  testing), that device class does not offer the on-device option. Voice mode never runs Parakeet
  on the device in the background: a suspended app gets far less memory, so the model unloads when
  the app leaves the foreground.
- **Turn end.** The app ends a turn on silence (1.2 s, `app.md` 13.1) or a tap, as before. Apple's
  live recognizer and the audio level are enough for that. Paseo runs Silero voice detection on
  its host, because its clients stream raw audio. Ours send whole turns, so the hab needs voice
  detection only for streaming (below).

## Hab speech engine

Paseo's design (`landscape.md` 2.10): its daemon host runs Parakeet TDT 0.6B through sherpa-onnx,
with Silero voice detection, and Kokoro for speech. We take the same engine in the same place and
write our own code.

### On-device result (2026-11-01)

The owner ran `swift/Spikes/ParakeetSpike` (Release, Parakeet TDT 0.6B v2 through Core ML) on an
iPhone 15 Pro. It worked well with a low footprint. On-device Parakeet stays a supported option on
recent iPhones; the hab engine stays the default. No iPad run yet.

### Spike result (2026-11-01)

`spikes/hab-parakeet` ran the sherpa-onnx 2.0.1 CLI with the int8 v2 model on an Apple M1 Pro
(16 GB, CPU, 4 threads, a busy machine). macOS `say` spoke the clips, which hold code identifiers.

| Clip | Load | Decode | Real-time factor | Peak memory | WER |
|---|---|---|---|---|---|
| 4.8 s | 0.94 to 1.12 s | 0.48 to 0.94 s | 0.10 to 0.20 | 1.12 to 1.14 GB | 0 / 16 |
| 15.2 s | 0.93 to 1.00 s | 1.52 to 2.26 s | 0.10 to 0.15 | 1.19 to 1.20 GB | 0 / 53 |
| 59.7 s | 0.92 to 1.02 s | 5.63 to 6.13 s | 0.09 to 0.10 | 1.49 to 1.52 GB | 13 / 194 |

- Speed is no problem. Memory is: about 1.1 GB resident once loaded, more for long clips. The
  model files take 661 MB on disk.
- Every error was a project word ("hab" as "hat", "holderreg" as "holder egg", "go vet" as
  "govet"). No identifier came out written as code. The vocabulary fix-up stays necessary.
- The Core ML provider showed no clear gain over CPU. Thread count matters: 1 thread was about
  three times slower than 4.
- Linux: the same release has static CLI builds for linux-x64 and linux-aarch64 (CPU, with
  onnxruntime linked in), and CUDA builds. A Linux run is still to do.
- Licences: sherpa-onnx Apache-2.0, onnxruntime MIT, Silero VAD MIT, the Parakeet model
  CC-BY-4.0 (an attribution line wherever the model is downloaded). The helper's full list is in
  `NOTICE`.

### Design

- **A helper process.** sherpa-onnx's Go bindings need cgo, and gff-hab builds with
  `CGO_ENABLED=0` (`layout.md`). So gff-hab starts a speech helper (the sherpa-onnx runtime) as a
  child on the first request and talks to it over stdio. The helper holds the model. It exits
  after 10 minutes with no request, the idle limit of voice mode (`app.md` 13.1), so an idle hab
  gives the memory back. A load takes about 1 s, so the first turn waits that long.
- **Warm-up.** The app calls `speech.prepare` when voice mode or dictation opens, so the model is
  loaded before the first turn ends.
- **The model.** It downloads on first use into the hab's state folder, with a checksum check,
  not with the release. A host setting turns the engine off. Default: on for a host sold as
  4 GB or more, else off. A smaller model (Parakeet TDT-CTC 110M) may serve small hosts;
  it is not measured yet.
- **Audio in.** The app sends the turn as one compressed clip: Opus at about 24 kbit/s (a 15 s
  turn is about 50 KB). The hab also accepts 16 kHz mono 16-bit PCM, for a client with no
  encoder. The helper decodes and resamples; gff-hab passes the bytes through. `opus` means an
  Ogg Opus file (RFC 7845): the app writes mono, 16 kHz input, 20 ms packets, with the end
  granule set so the decoder trims the padding (`app.md` 13.7).
- **Any client.** `gff` and the Mac app can call the same method, so dictation gives the same
  text everywhere.

### API addition

In the style of `hab-logic-api.md` 3.9. A new capability `speech` says the hab runs the engine:
it is installed and turned on, though the model may still need its download. In the revision 1
schema since 2026-11-01.

| Method | Purpose | Built |
|---|---|---|
| `speech.prepare` | Load the engine now, and start the model download if needed; returns the engine state | Yes (Part 6, voice mode) |
| `speech.transcribe` | Turn one recorded clip into text | Yes (Part 6, voice mode) |

- **`speech.prepare`** takes `{}` and returns `state` (`absent`, `downloading`, `loading`,
  `ready`; open), `progress` (whole percent, while downloading), `model` (its name) and `formats`
  (the audio formats the hab accepts: `opus`, `pcm16`; open). It also returns `error`, why the
  last download or load failed, until one succeeds. It returns at once and does not wait for the
  load.
- **`speech.transcribe`** takes `audio` (the clip, base64) and `format` (`opus` or `pcm16`), and
  the optional `sample_rate` (for `pcm16`; default 16000), `session` (whose vocabulary to apply)
  and `language` (default `en`). It returns `text`, `audio_ms` and `decode_ms`, and `raw` (the
  text before the vocabulary fix-up) when the fix-up changed anything.
- **Limits.** A clip is at most 120 s and 1 MiB before base64; a larger one is `invalid`. The
  control connection carries it like any other request. It runs off the read loop, so a long
  decode does not hold up other calls. The hab runs one decode at a time; others wait in order.
- **Errors.** No `speech` capability, the engine off, or the model not downloaded yet:
  `unavailable`, with a hint such as `downloading 40%`. A clip that does not decode: `invalid`.
- **Nothing stored.** gff-hab keeps no audio and no text from this call, and logs neither. The
  text reaches the session only when the client sends it (`session.send`), as typed text does.
- **Streaming, later.** Live words from the hab need a stream: a data connection of a new kind
  (`speech`, opened with a ticket as in `hab-logic-api.md` 5.2), with audio frames in, partial
  text out, and Silero voice detection on the hab. Apple's live words make it unnecessary for the
  first build.

### Built choices (2026-11-01)

The code: `internal/speech` (gff-hab's side), `internal/speechproto` (the helper protocol,
`docs/protocol/speech-helper.md`), `internal/oggopus` (the Ogg reader), `cmd/gff-hab-speech` (the
helper), `internal/control/speech.go` (the methods). Layout and build: `layout.md`, The speech
helper.

- **The helper.** `gff-hab-speech` links sherpa-onnx's C API from its pinned static release
  (2.0.1) and libopus from the system, through cgo. It builds only with the `speech` tag
  (`make speech-helper`), so `make check-pure`, `make build` and `make test-pure` never compile it.
  It decodes `pcm16` and Ogg Opus; both are in `formats`. First run (Apple M1 Pro, busy, 4
  threads): the model loads in 1.2 to 1.4 s; a 3 s spoken clip decodes in about 0.3 s, with the
  same text from `pcm16` and from Opus at 24 kbit/s.
- **Finding it.** gff-hab looks for the helper at `[speech] helper` in the hab's config file, then
  `$GFF_HAB_SPEECH_HELPER`, then beside the gff-hab binary, then on PATH. A path named in the file
  or the variable that is not an executable file means no helper: gff-hab does not fall back.
- **On or off.** `[speech] enabled` turns the engine on or off. With no setting it is on for a
  host sold as 4 GB or more: the code's line is 3.5 GiB, since such a host reports a little less
  (Linux keeps some back from `MemTotal`). The capability is on when the engine is on and the helper is found.
  gff-hab reads the section at start only.
- **Formats.** gff-hab runs `gff-hab-speech -info` once, on the first speech call, and reports
  the formats it prints. A helper that does not run makes both methods `unavailable`.
- **The model.** Parakeet TDT 0.6B v2 int8 from the sherpa-onnx `asr-models` release (482 MB
  archive, SHA-256 `157c157b...61e1ad`), into `<state>/speech/parakeet-tdt-0.6b-v2-int8`. One
  pass: the bytes go through the checksum and the extractor together into a temporary folder,
  renamed into place only when the size and checksum match. Only the four model files are kept,
  by base name, so no archive path can leave the folder. `ATTRIBUTION.txt` beside them holds the
  CC-BY-4.0 line, and the log line that starts each download carries it too. `[speech] model_url`
  names a mirror; the checksum still applies.
- **Download guards.** The download fails when no bytes arrive for 60 s (a dropped connection
  after sleep or a network change), when the archive runs past its known size, or when one
  extracted file grows past that size (a compressed file can expand without bound). Each download
  first removes the temporary folders an earlier one left when gff-hab restarted mid-download;
  one gff-hab owns a state folder.
- **Retries.** A failed download or load leaves `error` set until one succeeds. The next
  `speech.prepare` or `speech.transcribe` tries again. An idle exit or a stop is not an error.
- **Queue.** One decode at a time across all connections, in arrival order. At most 16 clips wait;
  one more is `unavailable`. A clip whose caller gives up (timeout, closed connection) keeps the
  decoder until the helper answers, so two decodes never overlap. One connection may run 8 speech
  calls at once; each call has 5 minutes.
- **Watchdogs.** A model load has 60 s (measured: 1.3 s). A clip's answer has 30 s plus twice
  the clip's length. A helper that misses either is killed: the caller gets `unavailable`, the
  decoder is free, and the next clip starts a fresh helper (a helper whose process ended is never
  handed to a clip, even before its monitor clears it). The watchdog runs even after the
  caller gave up, so a hang never holds the decoder until gff-hab restarts.
- **Limits.** gff-hab checks the clip before the helper sees it: 1 MiB, 120 s, `sample_rate` 8000
  to 48000, `language` absent or `en`. A pcm16 clip's length comes from its byte count; a pcm16
  clip of 1 MiB lasts at most 65 s, so only Opus can reach the 120 s limit. An Opus clip's length
  comes from the last granule position, and its packets' own lengths (from each packet's TOC
  byte, RFC 6716 3.1) bound it too: a granule that understates the clip by more than one packet
  is refused, and so is a granule more than one packet past them (it would also overflow the
  length arithmetic). A last page with no granule position is `invalid`. The helper also stops
  decoding past 120 s plus the pre-skip, whatever gff-hab let through.
- **Vocabulary.** With `session`, gff-hab lists the files git knows in the session's project
  folder (the newest one that exists): tracked, plus untracked files that are not ignored. Each path
  element and each file name without its extension is a term. The list is kept for one minute;
  git has 3 s. A term replaces a run of up to 6 words when the words, joined and lowercased,
  spell the term with its separators and case taken out ("link test dot go" to `link_test.go`,
  "holder reg" to `holderreg`). "dot", "underscore", "dash", "hyphen" and "slash" inside a run
  count as separators. A term must have 5 letters or digits. Ordinary words stay as spoken:
  - A single word changes only to a term that looks like code (a separator, or a capital after
    the first letter), so "server" never becomes `Server`.
  - A name in all capitals (`NOTICE`, `LICENSE`, `README`) is never a term: it is also a plain word.
  - A leading dot is dropped (`.github` is the term `github`).
  - A plain term that is also an English compound or phrasal verb (setup, login, backup, update,
    checkout, timeout, backend, filename and about 50 more) never joins two words: "set up"
    stays "set up". The list is finite: a repository term that is a compound not on it ("lock
    file" with `lockfile.go`) still joins. That change is usually what the speaker meant.
  - A capitalized first word ("Back up", "Fetch user") keeps the words, since a lowercase term
    would lose the capital of a sentence start or a name.
  - Two terms with the same spelling leave the words alone.

  A session with no project, or an unknown session, gets no fix-up rather than an error. When the
  fix-up changes anything it joins the words with single spaces, so runs of spaces and line breaks
  in the engine's text become one space. Parakeet writes neither, so this is a documented limit,
  not a fix.
- **Nothing kept.** gff-hab and the helper keep no audio and no text, and log neither. The
  helper's stderr reaches gff-hab's log, one line each, cut at 4 KiB; the helper writes only load
  messages there.

### Open items

- **Linux.** The helper builds from the `linux-x64` and `linux-aarch64` static releases (the
  Makefile pins both, and links the archives as a group), but no Linux build or run has happened.
  The dev box does it next: build, `make test-speech`, the speed and memory figures.
- **Shipping the helper.** `gff-hab install` does not copy the helper yet, and the macOS helper
  bundles do not include it. For now it is installed by hand: install libopus (Homebrew `opus`,
  Debian `libopus-dev`), run `make speech-helper`, then copy `build/gff-hab-speech` beside
  gff-hab or name it in `[speech] helper`, and restart gff-hab. The helper links libopus
  dynamically from the system package; a release build should link it statically.
- **Symbols.** The vocabulary holds file and folder names only. Symbols (function and type names)
  need a source the hab can read cheaply, such as the agent's transcript; not built.
- **Small hosts.** Parakeet TDT-CTC 110M for hosts under 4 GB is not measured.
- **Hotwords.** sherpa-onnx can bias a transducer towards a word list (modified beam search with
  hotwords). The text fix-up is enough for now; hotwords may help where the engine mishears a
  term beyond what the fix-up can join.

## Speech-to-text

WER is LibriSpeech test-clean / test-other, mostly from one M2 Pro benchmark (Lyonesse, July 2026). iPhone latency is largely unpublished.

| Engine | Size on disk | RAM | Speed | WER (LS) | Streaming | Languages | License | Apple runtime |
|---|---|---|---|---|---|---|---|---|
| Apple SpeechAnalyzer (iOS/macOS 26+) | System asset | Unpublished | ~3x faster than Whisper Small on M2 Pro | 2.12 / 4.56 | Yes, live partials | ~30 locales | Apple SDK | Native Swift |
| NVIDIA Parakeet TDT 0.6B v2 | ~443 MB int8 | Unpublished | Very fast: "150x realtime" on Apple silicon *(unverified)* | 2.01 / 3.40 | No (batch) | English | CC-BY-4.0 | FluidAudio (Core ML, ANE); sherpa-onnx on the hab |
| Parakeet TDT 0.6B v3 | ~467 MB int8 | Unpublished | As v2 | 2.51 / 4.28 | No | 25 European | CC-BY-4.0 | FluidAudio |
| NVIDIA Nemotron Speech Streaming 0.6B | ~564 MB int8 | 757 MB peak on iPhone 15 Pro Max | 52x realtime on A17 Pro | 1.99 (clean, 1.12 s chunks) | Yes | English; a multilingual 3.5 build exists | Unclear: NVIDIA eval license or OpenMDW *(check)* | FluidAudio Core ML ports |
| Moonshine streaming Tiny / Small / Medium | 48 MB / 189 MB / 509 MB | Small | Built for low latency | 4.5 / 2.53 / 2.16 (clean only) | Yes | English (others in separate models) | MIT | moonshine-swift (SPM) |
| Whisper Small (WhisperKit) | ~460 MB | ~1 GB *(unverified)* | 12-40x realtime on M2 Pro | 3.74 / 7.95 | Chunked only | ~99 | MIT | WhisperKit, whisper.cpp |
| IBM Granite Speech 5.0 (2B; 470M turbo CTC) | ~1 GB+ for 2B | — | — | 5.68 avg on Open ASR leaderboard (#2) | No | 6 | Apache-2.0 | No Core ML port found |

**The open model most praised on Hacker News** is **NVIDIA Parakeet**. In July 2026, commenters said it had "been out for a year" and beat Whisper (thread 48894752). IBM Granite Speech 5.0 shipped this month and ranks high, but has no Core ML port yet. Parakeet is the open model to beat on Apple hardware.

**Code-heavy speech:** every engine writes `useSuspenseQuery` as "use suspense query". Recognizer hints help *(unverified)*; the session vocabulary is the reliable fix.

## Text-to-speech

| Option | Quality | Latency | Size | License / cost |
|---|---|---|---|---|
| Apple AVSpeechSynthesizer, premium voices | Good, clearly synthetic | Instant, streams | System download per voice | Free, offline |
| Kokoro-82M (Core ML ports) | Near cloud quality, English-first | Real-time on ANE; first load compiles for seconds *(unverified)* | 80-724 MB depending on port *(sources conflict)* | Apache-2.0 |
| Cloud TTS (OpenAI, ElevenLabs, Gemini) | Best | Network round trip | None | Paid, user's key; audio text leaves device |

## Conversation layer

| Option | Pros | Cons |
|---|---|---|
| A. Direct, trimmed output | Simple; agent keeps full context | Relies on trimmer and summary line |
| B. Apple Foundation Models on device | Private, free | 4,096-token context (WWDC26; one blog says 8,192); Apple Intelligence devices only |
| C. Small model on the hab host | Bigger context | Needs a local model server per host |
| D. Cloud model, user's key | Best summaries | Session text leaves the hosts |
| E. Realtime speech API | Natural turn-taking, barge-in | A second agent speaking for the real one. OpenAI ~$14/hour (mini ~$4.50); Gemini Live ~$1.40/hour, rising as context is rebilled each turn *(aggregator figures)* |

Recommendation: **A** first; then **B** for long replies, **D** opt-in, **E** an experiment.

## Sources

- https://news.ycombinator.com/item?id=48894752 (SpeechAnalyzer benchmark thread)
- https://lyonesse.app/blog/apple-speech-api-benchmark.html
- https://lyonesse.app/blog/parakeet-moss-apple-speech-benchmark.html
- https://news.ycombinator.com/item?id=47666292 (Parakeet vs Whisper)
- https://news.ycombinator.com/item?id=47145321 (Parakeet, Canary-Qwen vs Moonshine)
- https://news.ycombinator.com/item?id=47406323 (Parakeet speed on Apple silicon)
- https://news.ycombinator.com/item?id=50193043 and https://huggingface.co/ibm-granite/granite-speech-5.0-2b
- https://github.com/k2-fsa/sherpa-onnx (Apache-2.0) and its `asr-models` release (`sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8`)
- https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2 (CC-BY-4.0)
- https://github.com/FluidInference/FluidAudio
- https://huggingface.co/FluidInference/parakeet-tdt-0.6b-v3-coreml
- https://huggingface.co/coder543/nemotron-speech-streaming-en-0.6b-coreai
- https://huggingface.co/FluidInference/Nemotron-3.5-ASR-Streaming-Multilingual-0.6b-CoreML
- https://huggingface.co/handy-computer/moonshine-streaming-small-gguf
- https://arxiv.org/html/2602.12241 (Moonshine v2)
- https://huggingface.co/mattmireles/kokoro-coreml
- https://huggingface.co/mlboydaisuke/Kokoro-82M-CoreML
- https://developer.apple.com/videos/play/wwdc2026/8121/ (Foundation Models)
- https://dev.to/hariharanjagan/whats-new-in-apples-foundation-models-framework-at-wwdc-2026-5227
- https://aireiter.com/blog/openai-realtime-api-pricing
- https://tech-insider.org/openai-vs-google-vs-qwen-voice-ai-apis-2026/
- https://huggingface.co/spaces/hf-audio/open_asr_leaderboard
