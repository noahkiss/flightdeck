# Parakeet spike

A one-screen iOS and iPadOS test app. It measures whether NVIDIA Parakeet TDT 0.6B v2
(English) runs well on a phone or tablet, compared with Apple SpeechAnalyzer. The result
sets the per-device default engine in `docs/specs/voice.md` ("Where transcription runs").

This is a throwaway spike, not part of the Flightdeck app.

## What it does

- Downloads the Parakeet v2 Core ML model on first use, with progress. The model is
  `FluidInference/parakeet-tdt-0.6b-v2-coreml` on Hugging Face, through the FluidAudio
  package (pinned to 0.17.7 in `project.yml`). It shows download time and each load time.
- Push-to-talk: hold the button, speak, release. Both engines transcribe the same clip.
  The app shows each text, the audio length, the transcription time and the real-time
  factor (RTF: processing time divided by audio time; below 1 is faster than real time).
- Shows the memory footprint live (`phys_footprint`, the number the iOS memory limit
  uses), the peak since launch, and the memory iOS still allows the app.
- Logs each memory warning and unloads the model when one arrives.
- Unloads the model when you leave the spike screen, and (toggle, on by default) when
  the app goes to the background.
- Phrase test: shows 20 coding phrases one at a time. Read each one. The app scores each
  engine with word error rate (WER) against the phrase, after it normalises case,
  punctuation and symbols ("link_test.go" and "link underscore test dot go" match).
- "Copy results as Markdown" puts every measurement, the phrase table and the event log
  on the clipboard.

SpeechAnalyzer needs iOS 26 or later. On older systems the app runs Parakeet only.

## Build

You need a Mac with Xcode 26 or later and [XcodeGen](https://github.com/yonaskolb/XcodeGen).

```sh
cd swift/Spikes/ParakeetSpike
cp Config/Local.xcconfig.example Config/Local.xcconfig
# Edit Config/Local.xcconfig: set DEVELOPMENT_TEAM to your team id
# (Xcode > Settings > Accounts). Set PRODUCT_BUNDLE_IDENTIFIER too if the
# default dev.flightdeck.spike.parakeet is taken on your account.
xcodegen generate
open ParakeetSpike.xcodeproj
```

`Config/Local.xcconfig` and the generated `ParakeetSpike.xcodeproj` are gitignored. Run
`xcodegen generate` again after you pull changes to `project.yml`.

Simulator build, no signing needed:

```sh
xcodebuild -project ParakeetSpike.xcodeproj -scheme ParakeetSpike \
  -destination 'generic/platform=iOS Simulator' build
```

## Install on a device

1. Connect the iPhone or iPad by cable (or pair it over Wi-Fi in Xcode > Devices).
2. On the device, turn on Developer Mode (Settings > Privacy & Security) if it asks.
3. In Xcode, pick the device as the run destination and press Run.
4. The first time, trust the developer profile on the device (Settings > General >
   VPN & Device Management).
5. Use a Release build for timing: Product > Scheme > Edit Scheme > Run > Build
   Configuration > Release. Debug builds are slower.

## Measurement protocol

Do each step on every device. Copy the results after each step and paste them somewhere
safe, because the numbers live only in memory until the app quits.

1. **Download and first load.** On Wi-Fi, open the spike screen and tap "Download and
   load model". Note the download time and the load time. Then tap "Unload model" and
   "Load model" twice more; later loads are usually faster than the first.
2. **Speed.** In "Free clips", record three clips of about 5, 15 and 30 seconds. Note
   time and RTF per engine. The first clip after a load includes warm-up; record it twice.
3. **Accuracy.** Switch to "Phrase test". Read all 20 phrases at a normal pace, in a
   quiet room. Redo a phrase with Previous if you stumbled. Note the "All" WER per engine.
4. **Peak memory.** Read the peak since launch after steps 1 to 3. For a second opinion,
   run the app from Xcode with Instruments (Product > Profile > Allocations or VM
   Tracker) during a load and a 30-second clip.
5. **Another heavy app open.** Open a heavy app (a large game, the camera recording 4K,
   or Safari with many tabs), then switch back to the spike. Load the model and record a
   clip. Note whether a memory warning arrived, whether iOS killed the spike (it restarts
   at the launch screen with no results), and the peak.
6. **After backgrounding.** With the model loaded, go to the home screen for a minute,
   then come back. The event log shows the unload and the footprint. Load again and note
   the load time. Repeat once with the background toggle off, and note whether the app
   survives and whether a transcription still works.
7. **Battery, 10 minutes.** Charge above 80 %, unplug, and note the battery percentage
   (Settings > Battery). Keep the spike screen open and record a clip about every 20
   seconds for 10 minutes, alternating free clips and phrases. Note the percentage at
   the end, and whether the device felt warm. Settings > Battery shows usage by app.
8. Copy the results and paste them back with the device model and battery numbers.

## Simulator test hook

`SPIKE_AUDIO_FILE` runs the pipeline with no microphone: the app downloads and loads the
model, transcribes the file with both engines, and prints a report to stdout.
`SPIKE_PHRASE=<n>` also scores it against phrase n.

```sh
say -o phrase.aiff "open internal slash link slash link underscore test dot go"
afconvert -f WAVE -d LEI16@16000 -c 1 phrase.aiff phrase.wav
xcrun simctl install booted <path to ParakeetSpike.app>
cp phrase.wav "$(xcrun simctl get_app_container booted dev.flightdeck.spike.parakeet data)/Documents/"
SIMCTL_CHILD_SPIKE_AUDIO_FILE=phrase.wav SIMCTL_CHILD_SPIKE_PHRASE=2 \
  xcrun simctl launch --console-pty booted dev.flightdeck.spike.parakeet
```

Simulator numbers are not device numbers: Core ML runs on the Mac's CPU there, not on the
Neural Engine, and the simulator's memory footprint does not match a device's.
