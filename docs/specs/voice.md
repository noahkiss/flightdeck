# Voice mode: models and pipeline

This spec picks the speech models and the pipeline behind voice mode. The user experience is in
`app.md` 13. Researched 2026-10-31. Facts marked *(unverified)* come from blogs or aggregators, not primary sources.

## Decisions

| Question | Decision |
|---|---|
| Cloud features | On device by default and free. Cloud summaries, cloud voices and realtime speech APIs are opt-in settings that use the user's own key. No Flightdeck service sits in the path |
| Spoken summary | In voice mode the app appends a request for a two-sentence spoken summary to each spoken prompt. The app reads that summary aloud and skips code, diffs and tables |
| Devices | iPhone and iPad in the first build; the Mac follows with the same speech APIs |
| Languages | English only (owner, 2026-10-31). So the engine is Parakeet TDT 0.6B v2, the English model: more accurate than the multilingual v3 and slightly smaller |
| Speech engine | NVIDIA Parakeet first, on device through a Core ML port, because it handles software-engineering speech better (owner, 2026-10-31). The model downloads on first use (about 440 MB int8 for the English v2 model), not with the app. Apple SpeechAnalyzer is the fallback before the download finishes and on devices that cannot run Parakeet. A hosted Parakeet endpoint with the user's key (about $0.0015 per audio minute in October 2026) is an opt-in option. All engines sit behind one interface |

## Recommendation

First build: an on-device pipeline with no extra model in the loop.

1. **Speech-to-text:** NVIDIA Parakeet through a Core ML port, downloaded on first use. Fallback: Apple SpeechAnalyzer (no download, live partial results). A small streaming Parakeet variant can show live partials while the larger model gives the final text; test both on a device.
2. **Identifier fix-up:** the hab sends a session vocabulary (file names, symbols). The app passes it as recognizer hints and fuzzy-matches the transcript against it.
3. **Turn in:** the cleaned transcript goes straight into the session as a normal prompt.
4. **Turn out:** speak only the agent's final text. A deterministic trimmer drops code, diffs and tables ("3 files changed"). Voice mode may append "end with a two-sentence spoken summary" to each prompt, so the agent summarizes from full context at no extra cost.
5. **Text-to-speech:** Apple premium system voices. Barge-in stops playback when speech starts.
6. **Approvals:** a fixed grammar, never an LLM. The app reads back the command and accepts only "allow" or "deny".

Upgrade path, each a setting: Kokoro-82M voice; a long-reply summarizer (Apple Foundation Models, then a cloud model with the user's key); a realtime speech API as an optional front end.

## Where transcription runs

Three engines sit behind one interface, chosen per device:

| Engine | Runs on | Memory cost to the app | Notes |
|---|---|---|---|
| Parakeet on device | The phone's Neural Engine | The model loads into the app: about 0.5 to 0.8 GB peak for a 0.6B model (a streaming 0.6B port measured 757 MB peak on an iPhone 15 Pro Max) | Best accuracy on software speech. Load only while voice mode is open; unload on exit and on a memory warning |
| Apple SpeechAnalyzer | A system process | Almost none: the model lives outside the app | Always available; the fallback |
| Transcription on the hab | The host | None on the phone | The app sends a short compressed clip (a few kilobytes per second) over the existing SSH connection; the hab runs Parakeet (Core ML on a Mac, an ONNX or GPU runtime on Linux) and returns text. Adds one network round trip |

Rule: a device spike decides the default before the build. It measures peak memory, load time,
speed and battery for Parakeet on the oldest supported iPhone and on the owner's devices. If peak
memory exceeds the budget (default 1 GB, or the app is killed in testing), that device class
defaults to the hab engine or Apple's. Voice mode never runs Parakeet in the background: a
suspended app gets far less memory, so the model unloads when the app leaves the foreground.

## Speech-to-text

WER is LibriSpeech test-clean / test-other, mostly from one M2 Pro benchmark (Lyonesse, July 2026). iPhone latency is largely unpublished.

| Engine | Size on disk | RAM | Speed | WER (LS) | Streaming | Languages | License | Apple runtime |
|---|---|---|---|---|---|---|---|---|
| Apple SpeechAnalyzer (iOS/macOS 26+) | System asset | Unpublished | ~3x faster than Whisper Small on M2 Pro | 2.12 / 4.56 | Yes, live partials | ~30 locales | Apple SDK | Native Swift |
| NVIDIA Parakeet TDT 0.6B v2 | ~443 MB int8 | Unpublished | Very fast: "150x realtime" on Apple silicon *(unverified)* | 2.01 / 3.40 | No (batch) | English | CC-BY-4.0 | FluidAudio (Core ML, ANE) |
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
