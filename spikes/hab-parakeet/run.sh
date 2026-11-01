#!/bin/bash
# Hab-side Parakeet spike: transcribe three spoken clips with the sherpa-onnx CLI and
# print load time, decode time, real-time factor, peak memory and word error rate.
# See README.md. Models and binaries live outside the repo; pass their paths in.
#
#   SHERPA_BIN    path to sherpa-onnx-offline (required)
#   PARAKEET_DIR  folder with encoder/decoder/joiner .onnx and tokens.txt (required)
#   WORK          output folder for clips and logs (default: a new temp folder)
#   THREADS       onnxruntime threads (default 4)
#   RUNS          runs per clip (default 3)
#   VOICE         macOS say voice (default: the system voice)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${SHERPA_BIN:?set SHERPA_BIN to the sherpa-onnx-offline binary}"
: "${PARAKEET_DIR:?set PARAKEET_DIR to the Parakeet model folder}"
WORK=${WORK:-$(mktemp -d)}
THREADS=${THREADS:-4}
RUNS=${RUNS:-3}
mkdir -p "$WORK"

enc=$(ls "$PARAKEET_DIR"/encoder*.onnx | head -1)
dec=$(ls "$PARAKEET_DIR"/decoder*.onnx | head -1)
joi=$(ls "$PARAKEET_DIR"/joiner*.onnx | head -1)

case $(uname) in
  Darwin) timecmd=(/usr/bin/time -l) ;;
  *) timecmd=(/usr/bin/time -v) ;;
esac

# Make the clips: 16 kHz mono 16-bit WAV, the model's input format. On Linux, put
# <name>.wav files in $WORK yourself (any TTS or a recording of clips.txt).
while IFS=$'\t' read -r name text; do
  wav=$WORK/$name.wav
  if [[ ! -f $wav ]]; then
    command -v say >/dev/null || { echo "no $wav and no say(1)" >&2; exit 1; }
    say ${VOICE:+-v "$VOICE"} --file-format=WAVE --data-format=LEI16@16000 -o "$wav" "$text"
  fi
  # The wire size for the API proposal: the same clip as 24 kbit/s AAC.
  if command -v afconvert >/dev/null; then
    afconvert -f m4af -d aac -b 24000 "$wav" "$WORK/$name.m4a" 2>/dev/null || true
  fi
done < "$here/clips.txt"

printf 'clip\taudio_s\tload_s\tdecode_s\trtf\tpeak_rss_mb\twer\terrors\tids_exact\twav_kb\taac_kb\n'
while IFS=$'\t' read -r name text; do
  wav=$WORK/$name.wav
  for run in $(seq 1 "$RUNS"); do
    log=$WORK/$name.$run.log
    "${timecmd[@]}" "$SHERPA_BIN" \
      --encoder="$enc" --decoder="$dec" --joiner="$joi" \
      --tokens="$PARAKEET_DIR/tokens.txt" --model-type=nemo_transducer \
      --num-threads="$THREADS" "$wav" >"$log" 2>&1
    load=$(sed -n 's/.*recognizer created in \([0-9.]*\) s.*/\1/p' "$log")
    decode=$(sed -n 's/^Elapsed seconds: \([0-9.]*\) s.*/\1/p' "$log")
    rtf=$(sed -n 's/^Real time factor (RTF): .* = \([0-9.]*\).*/\1/p' "$log")
    audio=$(sed -n 's/^Real time factor (RTF): [0-9.]* \/ \([0-9.]*\) .*/\1/p' "$log")
    hyp=$(python3 "$here/wer.py" --text "$log")
    if [[ $(uname) == Darwin ]]; then
      rss=$(awk '/maximum resident set size/ {printf "%.0f", $1/1048576}' "$log")
    else
      rss=$(awk -F: '/Maximum resident set size/ {printf "%.0f", $2/1024}' "$log")
    fi
    score=$(python3 "$here/wer.py" "$text" "$hyp")
    wavkb=$(( $(wc -c <"$wav") / 1024 ))
    aackb=-
    [[ -f $WORK/$name.m4a ]] && aackb=$(( $(wc -c <"$WORK/$name.m4a") / 1024 ))
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$audio" "$load" "$decode" "$rtf" "$rss" "$score" "$wavkb" "$aackb"
    [[ $run == 1 ]] && printf '%s\n' "$hyp" >"$WORK/$name.txt"
  done
done < "$here/clips.txt"
echo "transcripts and logs: $WORK" >&2
