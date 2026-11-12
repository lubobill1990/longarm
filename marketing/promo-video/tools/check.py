# Self-check of the rendered audio against timeline.json: every line's real onset in vo.wav, its grid position
# (hook: free; first line of a grid:4 section: on a bar line; other lines: on a beat, >= 0.25 s after the previous
# line), durations, and a waveform + spectrogram PNG of the mix (audio/mix-wave.png, audio/mix-spectrum.png).
#   uv run -p 3.12 --no-project --with numpy --with soundfile python -I tools/check.py
import json, os, subprocess
import numpy as np, soundfile as sf

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
tl = json.load(open(os.path.join(ROOT, "timeline.json"), encoding="utf-8"))
script = {s["id"]: s for s in json.load(open(os.path.join(ROOT, "script.json"), encoding="utf-8"))["sections"]}
vo, sr = sf.read(os.path.join(ROOT, "audio", "vo.wav"))
M0, BEAT, BAR = tl["musicStart"], tl["beat"], tl["bar"]


def onset(t):
    a = int((t - 0.2) * sr)
    seg = np.abs(vo[a:a + int(0.6 * sr)])
    return a / sr + np.argmax(seg > 0.01) / sr


def grid_pos(t):
    beats = round((t - M0) / BEAT, 3)
    return f"bar {int(beats // 4):2d} beat {beats % 4 + 1:.2f}"


bad = 0
prev_end = None
print(f"{'section':8s} {'#':>2s} {'start':>8s} {'onset':>8s} {'grid':18s} {'gap':>5s}  rule")
for s in tl["sections"]:
    for i, l in enumerate(s["lines"]):
        st, on = l["start"], onset(l["start"])
        gap = st - prev_end if prev_end is not None else 0
        if s["id"] == "hook":
            rule, ok = "free (no music)", True
        elif i == 0 and script[s["id"]].get("grid") == 4:
            rule, ok = "bar line", abs(((st - M0) / BAR) - round((st - M0) / BAR)) < 1e-3
        else:
            rule, ok = "beat, gap>=0.25", abs(((st - M0) / BEAT) - round((st - M0) / BEAT)) < 1e-3 and gap >= 0.25
        ok = ok and abs(on - st) < 0.02
        bad += not ok
        print(f"{s['id']:8s} {i + 1:2d} {st:8.3f} {on:8.3f} {grid_pos(st) if s['id'] != 'hook' else '-':18s} {gap:5.2f}  {rule} {'ok' if ok else 'FAIL'}")
        prev_end = l["end"]
    for m in s["musicOnly"]:
        print(f"{'':8s}    music-only {m['start']:.3f}-{m['end']:.3f} ({(m['end'] - m['start']) / BAR:.0f} bars, {grid_pos(m['start'])})")
print(f"musicStart {M0:.3f} ({grid_pos(M0)}), drop downbeat {tl['dropDownbeat']:.3f}, end cue {tl['endCue']:.3f}")

for f in ("vo.wav", "music.wav", "music-ducked.wav", "mix.wav"):
    d = float(subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0",
                              os.path.join(ROOT, "audio", f)], capture_output=True, text=True).stdout)
    print(f"{f:17s} {d:8.3f} s  ({int(d // 60)}:{d % 60:06.3f})  timeline total {tl['total']:.3f}")
print("grid check:", "all ok" if not bad else f"{bad} FAIL")

mix = os.path.join(ROOT, "audio", "mix.wav")
subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", mix, "-filter_complex",
                "showwavespic=s=2400x500:split_channels=1:colors=0x4fc3f7|0x81c784", "-frames:v", "1",
                os.path.join(ROOT, "audio", "mix-wave.png")], check=True)
subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", mix, "-lavfi",
                "showspectrumpic=s=2400x600:legend=1:scale=log:fscale=log", os.path.join(ROOT, "audio", "mix-spectrum.png")], check=True)
print("wrote audio/mix-wave.png, audio/mix-spectrum.png")
