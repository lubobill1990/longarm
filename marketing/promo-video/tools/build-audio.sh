#!/bin/sh
# Rebuild the promo's audio from script.json, in pipeline order (run from anywhere; needs uv, ffmpeg, an NVIDIA GPU
# for the ASR step or patience on the CPU). VO responses are cached in audio/vo/raw.
set -e
cd "$(dirname "$0")/.."
PY="uv run -p 3.12 --no-project"
$PY --with edge-tts --with numpy --with soundfile --with pyloudnorm python -I tools/vo.py "$@"   # audio/vo/*.wav
$PY --with numpy --with soundfile python -I tools/timeline.py                                    # vo.wav, timeline.json, music-plan.json
$PY --with numpy --with scipy --with soundfile --with pyloudnorm python -I tools/music.py         # audio/music.wav
$PY --with numpy --with scipy --with soundfile python -I tools/sfx.py                            # audio/sfx/
$PY --with numpy --with scipy --with soundfile --with pyloudnorm python -I tools/mix.py           # music-ducked.wav, mix.wav
$PY --with numpy --with soundfile python -I tools/check.py                                       # grid table, PNGs
$PY --with faster-whisper --with numpy --with nvidia-cublas-cu12 --with nvidia-cudnn-cu12 \
  python -I tools/asr.py audio/mix.wav audio/asr-mix.json                                        # listen-check
