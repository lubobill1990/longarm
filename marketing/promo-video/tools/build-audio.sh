#!/bin/sh
# Rebuild the promo's audio from script.json, in pipeline order (run from anywhere; needs uv, ffmpeg, an NVIDIA GPU
# for the ASR step or patience on the CPU). VO responses are cached in audio/vo/raw.
# Another language: FILM_ROOT=lang/zh (its script.json; its audio/, timeline.json, music-plan.json are written
# there). The ASR listen-check is English only: SKIP_ASR=1 (or another language) skips it.
set -e
cd "$(dirname "$0")/.."
if [ -n "$FILM_ROOT" ]; then FILM_ROOT=$(cd "$FILM_ROOT" && { pwd -W 2>/dev/null || pwd; }); export FILM_ROOT; fi  # absolute (Windows python: pwd -W)
PY="uv run -p 3.12 --no-project"
$PY --with edge-tts --with numpy --with soundfile --with pyloudnorm python -I -X utf8 tools/vo.py "$@"   # audio/vo/*.wav
$PY --with numpy --with soundfile python -I -X utf8 tools/timeline.py                                    # vo.wav, timeline.json, music-plan.json
$PY --with numpy --with scipy --with soundfile --with pyloudnorm python -I -X utf8 tools/music.py         # audio/music.wav
$PY --with numpy --with scipy --with soundfile python -I -X utf8 tools/sfx.py                            # audio/sfx/
$PY --with numpy --with scipy --with soundfile --with pyloudnorm python -I -X utf8 tools/mix.py           # music-ducked.wav, mix.wav
$PY --with numpy --with soundfile python -I -X utf8 tools/check.py                                       # grid table, PNGs
if [ -z "$SKIP_ASR" ] && [ -z "$FILM_ROOT" ]; then
  $PY --with faster-whisper --with numpy --with nvidia-cublas-cu12 --with nvidia-cudnn-cu12 \
    python -I -X utf8 tools/asr.py audio/mix.wav audio/asr-mix.json                                      # listen-check
fi
