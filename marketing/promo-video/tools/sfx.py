# Synthesized SFX kit for the editor -> audio/sfx/*.wav (48 kHz stereo) + audio/sfx/index.json
# (duration, attack = first sound, peak = loudest moment; align the peak with the visual hit).
#   uv run -p 3.12 --no-project --with numpy --with scipy --with soundfile python -I tools/sfx.py
# Peaks at -3 dBFS (whooshes/riser -6): place them 3-10 dB over the music bed and keep them light under speech.
import json, os
import numpy as np, soundfile as sf
from scipy import signal

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "audio", "sfx")
SR = 48000
rng = np.random.default_rng(11)


def tt(sec):
    return np.arange(int(sec * SR)) / SR


def noise(sec):
    return rng.standard_normal(int(sec * SR))


def filt(x, kind, f, order=2):
    return signal.sosfilt(signal.butter(order, f, kind, fs=SR, output="sos"), x, axis=0)


def pan(x, p):
    p = np.broadcast_to(p, x.shape)
    a = (p + 1) * np.pi / 4
    return np.stack([x * np.cos(a), x * np.sin(a)], axis=1) * np.sqrt(2)


def room(x, rt60=1.2, wet=0.25):
    n = int(rt60 * SR)
    t = np.arange(n) / SR
    ir = np.stack([filt(rng.standard_normal(n), "lowpass", 5000) * np.exp(-6.9 * t / rt60) for _ in range(2)], axis=1)
    ir /= np.sqrt((ir ** 2).sum(axis=0))
    x = np.pad(x, ((0, n), (0, 0)))
    w = np.stack([signal.oaconvolve(x[:, c], ir[:, c])[: len(x)] for c in range(2)], axis=1)
    return x + wet * w


def sweep_bands(nz, pos, centers):
    return sum(filt(nz, "bandpass", [c / 1.5, min(c * 1.5, 20000)]) * np.exp(-0.5 * ((pos - i) / 1.0) ** 2)
               for i, c in enumerate(centers))


def whoosh(dur):
    t = tt(dur)
    p = t / dur
    apex = 0.6
    bell = np.where(p < apex, (p / apex) ** 2.5, np.exp(-((p - apex) / (1 - apex)) * 4))
    centers = np.geomspace(250, 7000, 9)
    pos = (8 * np.where(p < apex, p / apex, 1 - 0.6 * (p - apex) / (1 - apex)))
    x = sweep_bands(noise(dur), pos, centers) * bell
    return pan(x / np.abs(x).max(), -0.7 + 1.4 * p)


def pop():
    t = tt(0.18)
    f = 300 + 700 * np.exp(-t / 0.012)
    x = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t / 0.035) * np.minimum(1, t / 0.001)
    x += 0.15 * filt(noise(0.18), "highpass", 3000) * np.exp(-t / 0.002)
    return pan(x, 0.0)


def click():
    t = tt(0.06)
    x = 0.6 * filt(noise(0.06), "bandpass", [2000, 7000]) * np.exp(-t / 0.0015)
    x += 0.5 * np.sin(2 * np.pi * 2800 * t) * np.exp(-t / 0.008)
    return pan(x, 0.0)


def key_tap():
    t = tt(0.14)
    def hit(level, d):
        y = level * filt(noise(0.14), "bandpass", [1500, 9000]) * np.exp(-np.clip(t - d, 0, None) / 0.004) * (t >= d)
        y += 0.7 * level * np.sin(2 * np.pi * 210 * (t - d)) * np.exp(-np.clip(t - d, 0, None) / 0.018) * (t >= d)
        return y
    return pan(hit(1.0, 0.0) + hit(0.35, 0.055), 0.1)


def impact():
    dur = 3.0
    t = tt(dur)
    f = 28 + 65 * np.exp(-t / 0.1)
    boom = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t / 0.8)
    thump = filt(noise(dur), "lowpass", 2000) * np.exp(-t / 0.06)
    crash = 0.25 * filt(noise(dur), "highpass", 4000) * np.exp(-t / 0.7)
    x = np.tanh(1.5 * (boom + 0.6 * thump)) + crash
    x *= np.minimum(1, t / 0.0008) * np.minimum(1, (dur - t) / 0.3)
    return room(pan(x, 0.0), 2.2, 0.3)


def riser(bars=2, bpm=120):
    dur = bars * 4 * 60 / bpm
    t = tt(dur)
    p = t / dur
    centers = np.geomspace(180, 9000, 10)
    x = sweep_bands(noise(dur), p ** 1.4 * 9, centers)
    x /= np.abs(x).max()
    f = 110 * 2 ** (3 * p ** 1.2)
    ph = np.cumsum(f) / SR
    tone = sum(np.sin(2 * np.pi * k * ph) / k for k in range(1, 12)) * 0.25
    x = (x + tone) * p ** 2.2 * np.minimum(1, (dur - t) / 0.01)
    # ends exactly at the downbeat: put the hit at len(riser)
    return pan(x, 0.25 * np.sin(2 * np.pi * 2 * t))


def glitch():
    dur = 0.32
    x = np.zeros(int(dur * SR))
    i = 0
    while i < len(x):
        n = int(rng.uniform(0.012, 0.04) * SR)
        t = np.arange(n) / SR
        kind = rng.integers(3)
        if kind == 0:
            seg = np.sign(np.sin(2 * np.pi * rng.uniform(200, 1800) * t))
        elif kind == 1:
            seg = filt(rng.standard_normal(n), "highpass", 2000)
        else:
            seg = np.zeros(n)
        hold = int(rng.integers(4, 24))      # sample-and-hold = crunchy decimation
        seg = np.repeat(seg[::hold], hold)[:n]
        x[i:i + n] = seg[: len(x) - i] * rng.uniform(0.4, 1.0)
        i += n
    x = filt(x, "lowpass", 12000) * np.minimum(1, (dur - np.arange(len(x)) / SR) / 0.01)
    return pan(x, np.repeat(rng.uniform(-0.6, 0.6, 8), len(x) // 8 + 1)[: len(x)])


def ding():
    t = tt(2.2)
    f0 = 1318.5  # E6
    x = sum(a * np.sin(2 * np.pi * f0 * r * t) * np.exp(-t / d)
            for r, a, d in [(1, 1, 0.9), (2.0, 0.35, 0.5), (3.01, 0.18, 0.3), (4.17, 0.08, 0.15)])
    x *= np.minimum(1, t / 0.002)
    x += 0.2 * filt(noise(2.2), "highpass", 5000) * np.exp(-t / 0.003)
    return room(pan(x, 0.0), 1.6, 0.25)


KIT = {
    "whoosh-short": (lambda: whoosh(0.5), -6, "quick swipe / cut; peak ~37 ms after the cut"),
    "whoosh-long": (lambda: whoosh(1.2), -6, "slow pan / camera move"),
    "pop": (pop, -3, "element pops in"),
    "click-soft": (click, -3, "UI click / cursor"),
    "key-tap": (key_tap, -3, "single keystroke; repeat ~90-140 ms apart for typing"),
    "impact": (impact, -3, "boom + crash for a hit"),
    "riser-2bar": (riser, -6, "4.0 s at 120 BPM; ends exactly on the target downbeat"),
    "glitch": (glitch, -3, "digital stutter"),
    "ding": (ding, -3, "success / done"),
}


def main():
    os.makedirs(OUT, exist_ok=True)
    index = []
    for name, (make, peak_db, use) in KIT.items():
        x = make()
        x = x / np.abs(x).max() * 10 ** (peak_db / 20)
        sf.write(os.path.join(OUT, name + ".wav"), x.astype(np.float32), SR, subtype="FLOAT")
        env = np.abs(x).max(axis=1)
        attack = int(np.argmax(env > env.max() * 0.01))       # -40 dB below peak
        hop = SR // 40                                        # loudest 25 ms window (long enough for the boom)
        rms = np.sqrt(np.convolve(env ** 2, np.ones(hop) / hop, mode="same"))
        pk = int(np.argmax(rms >= 0.9 * rms.max()))         # first moment within 1 dB of the loudest
        index.append({"name": name, "file": f"audio/sfx/{name}.wav", "use": use,
                      "duration": round(len(x) / SR, 3), "attack": round(attack / SR, 3),
                      "peak": round(pk / SR, 3), "peakDBFS": peak_db})
        print(f"{name:13s} {len(x) / SR:5.2f}s  attack {attack / SR:.3f}  peak {pk / SR:.3f}")
    json.dump({"sampleRate": SR, "note": "attack/peak are seconds from the file start; place so peak = visual hit time",
               "sfx": index}, open(os.path.join(OUT, "index.json"), "w", encoding="utf-8"), indent=1)


if __name__ == "__main__":
    main()
