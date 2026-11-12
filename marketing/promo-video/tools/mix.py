# Duck the music under the voice (voice as sidechain key) and master the film's audio.
#   uv run -p 3.12 --no-project --with numpy --with scipy --with soundfile --with pyloudnorm python -I tools/mix.py
# -> audio/music-ducked.wav, audio/mix.wav (48 kHz stereo, -14 LUFS integrated, true peak <= -1 dBTP),
#    audio/mix.json (gains and per-section loudness). music-ducked.wav is at the mix's final scale:
#    mix = music-ducked + vo.wav * voGain (exactly, except where the peak limiter acted).
import json, os
import numpy as np, soundfile as sf, pyloudnorm as pyln
from scipy import signal
from scipy.ndimage import maximum_filter1d, minimum_filter1d

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SR = 48000
TARGET = -14.0
CEILING_DBTP = -1.3          # limiter ceiling; leaves margin to the -1 dBTP spec
MUSIC_ONLY_LUFS = -17.0      # unducked music level (before the final gain) relative to the -17 LUFS voice
DUCK_DB = -8.5
LOOKAHEAD, HOLD, ATTACK, RELEASE = 0.06, 0.18, 0.04, 0.35

tl = json.load(open(os.path.join(ROOT, "timeline.json"), encoding="utf-8"))
vo, sr = sf.read(os.path.join(ROOT, "audio", "vo.wav"), dtype="float64")
mu, sr2 = sf.read(os.path.join(ROOT, "audio", "music.wav"), dtype="float64")
assert sr == sr2 == SR
n = max(len(vo), len(mu))
vo = np.pad(vo, (0, n - len(vo)))
mu = np.pad(mu, ((0, n - len(mu)), (0, 0)))
meter = pyln.Meter(SR)


def spans(kind):
    out = []
    for s in tl["sections"]:
        if kind == "musicOnly":
            out += [(m["start"], m["end"]) for m in s["musicOnly"]]
        elif kind == "underVoice" and s["id"] != "hook":
            out += [(l["start"], l["end"]) for l in s["lines"]]
        elif kind == "hook" and s["id"] == "hook":
            out += [(s["start"], s["end"])]
    return out


def lufs(x, sp):
    seg = np.concatenate([x[int(a * SR):int(b * SR)] for a, b in sp])
    return meter.integrated_loudness(seg)


# music level: put the music-only bars at MUSIC_ONLY_LUFS
mu *= 10 ** ((MUSIC_ONLY_LUFS - lufs(mu, spans("musicOnly"))) / 20)

# sidechain key: voice level in 1 ms frames, looked ahead so the dip lands before the first syllable
hop = SR // 1000
frames = n // hop + 1
v = np.pad(vo, (0, frames * hop - n)).reshape(frames, hop)
rms = np.sqrt(np.convolve((v ** 2).mean(axis=1), np.ones(20) / 20, mode="same")) + 1e-9
db = 20 * np.log10(rms)
active = np.clip((db + 50) / 15, 0, 1)                        # -50 dBFS -> 0, -35 dBFS -> 1
active = np.roll(active, -int(LOOKAHEAD * 1000))
hold = int(HOLD * 1000)
active = maximum_filter1d(active, hold, origin=(hold - 1) // 2)   # max over the last HOLD: rides through word gaps
target = DUCK_DB * active
g = np.zeros(frames)
cur = 0.0
a_c, r_c = np.exp(-1 / (ATTACK * 1000)), np.exp(-1 / (RELEASE * 1000))
for i in range(frames):
    c = a_c if target[i] < cur else r_c
    cur = target[i] + (cur - target[i]) * c
    g[i] = cur
gain = 10 ** (np.interp(np.arange(n) / hop, np.arange(frames), g) / 20)
ducked = mu * gain[:, None]


def true_peak_env(x):
    """Per-sample true peak (max over channels of a 4x oversampled signal)."""
    pk = np.abs(signal.resample_poly(x, 4, 1, axis=0)).max(axis=1)
    pk = np.pad(pk, (0, max(0, 4 * len(x) - len(pk))))[: 4 * len(x)]
    return pk.reshape(-1, 4).max(axis=1)


def limiter_gain(x, ceiling_db=CEILING_DBTP, lookahead=0.003, release=0.08):
    need = np.minimum(1, 10 ** (ceiling_db / 20) / np.maximum(true_peak_env(x), 1e-9))
    need = minimum_filter1d(need, 2 * int(lookahead * SR) + 1)   # reach each peak a little early
    lim = np.empty_like(need)
    cur, rel = 1.0, np.exp(-64 / (release * SR))
    for i in range(0, len(need), 64):                           # 64-sample blocks keep the release loop fast
        cur = min(need[i:i + 64].min(), 1 - (1 - cur) * rel)
        lim[i:i + 64] = cur
    return np.minimum(lim, need)


# loudness gain and limiter together, iterated so the limited mix lands on the target
raw = ducked + vo[:, None]
g_db = TARGET - meter.integrated_loudness(raw)
for _ in range(4):
    x = raw * 10 ** (g_db / 20)
    lim = np.ones(n)
    for _ in range(2):                                          # second pass catches inter-sample overs
        lim *= limiter_gain(x * lim[:, None])
    mix = x * lim[:, None]
    err = TARGET - meter.integrated_loudness(mix)
    if abs(err) < 0.05:
        break
    g_db += err
vo_gain = 10 ** (g_db / 20)
ducked = ducked * vo_gain * lim[:, None]
mix_l = meter.integrated_loudness(mix)

sf.write(os.path.join(ROOT, "audio", "music-ducked.wav"), ducked.astype(np.float32), SR, subtype="FLOAT")
sf.write(os.path.join(ROOT, "audio", "mix.wav"), mix.astype(np.float32), SR, subtype="FLOAT")

report = {
    "integratedLUFS": round(mix_l, 2),
    "truePeakDBTP": round(20 * np.log10(true_peak_env(mix).max()), 2),
    "voGain": round(vo_gain, 4), "duckDB": DUCK_DB,
    "limiterMaxReductionDB": round(-20 * np.log10(lim.min()), 2),
    "sections": {
        "hook (music only, no voice)": round(lufs(ducked, spans("hook")), 1),
        "music-only bars, mix": round(lufs(mix, spans("musicOnly")), 1),
        "music under voice": round(lufs(ducked, spans("underVoice")), 1),
        "voice": round(lufs(vo[:, None] * vo_gain * np.ones((1, 2)), spans("underVoice")), 1),
        "speech sections, mix": round(lufs(mix, spans("underVoice")), 1),
    },
}
for s in tl["sections"]:
    report["sections"][f"section {s['id']}"] = round(lufs(mix, [(s["start"], s["end"])]), 1)
json.dump(report, open(os.path.join(ROOT, "audio", "mix.json"), "w", encoding="utf-8"), indent=1)
print(json.dumps(report, indent=1))
