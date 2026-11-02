# The score, composed in code to music-plan.json: synthwave / future-garage bed at 120 BPM in A minor
# (Am7 - Fmaj7 - Cadd9 - G, i.e. vi-IV-I-V of C, restarting at every section), rendered to audio/music.wav (48 kHz stereo).
#   uv run -p 3.12 --no-project --with numpy --with scipy --with soundfile --with pyloudnorm python -I tools/music.py
# Every oscillator is band-limited (PolyBLEP saws, additive plucks), the room is a convolution with a synthetic
# decaying-noise IR, and pads/bass/arp pump from the kick. Arrangement per bar comes from each section's "energy";
# music-only bars play the full arrangement plus the lead hook. Deterministic (fixed seed).
import json, os
import numpy as np, soundfile as sf, pyloudnorm as pyln
from scipy import signal
from scipy.ndimage import uniform_filter1d

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SR = 48000
rng = np.random.default_rng(20261102)

plan = json.load(open(os.path.join(ROOT, "music-plan.json"), encoding="utf-8"))
BEAT, BAR, M0, TOTAL = plan["beat"], plan["bar"], plan["musicStart"], plan["total"]
END_CUE = plan["endCue"]["t"]
N = int(round(TOTAL * SR)) + 1
T = np.arange(N) / SR


def bar_t(b):
    return M0 + b * BAR


def hz(m):
    return 440.0 * 2 ** ((np.asarray(m, float) - 69) / 12)


def noise(sec):
    return rng.standard_normal(int(sec * SR))


def sos(kind, f, order=2):
    return signal.butter(order, f, kind, fs=SR, output="sos")


def smooth(x, k):
    return uniform_filter1d(x, k, mode="nearest")


def filt(x, kind, f, order=2):
    return signal.sosfilt(sos(kind, f, order), x, axis=0)


# ---------------------------------------------------------------- tracks
class Track:
    def __init__(self, name, send=0.0, delay=0.0):
        self.name, self.send, self.delay = name, send, delay
        self.x = np.zeros((N, 2), np.float32)

    def add(self, t, sig, gain=1.0, pan=0.0):
        i = int(round(t * SR))
        if i >= N or gain == 0:
            return
        sig = np.asarray(sig, float)
        if sig.ndim == 1:
            a = (pan + 1) * np.pi / 4
            sig = np.stack([sig * np.cos(a), sig * np.sin(a)], axis=1) * np.sqrt(2)
        if i < 0:
            sig, i = sig[-i:], 0
        n = min(len(sig), N - i)
        self.x[i:i + n] += sig[:n] * gain


tracks = {k: Track(k, *v) for k, v in {
    "kick": (0.0,), "clap": (0.22,), "hat": (0.06,), "perc": (0.15,), "bass": (0.0,), "pad": (0.32,),
    "arp": (0.25, 0.3), "lead": (0.3, 0.32), "fx": (0.35,), "drone": (0.2,), "final": (0.45,)}.items()}


# ---------------------------------------------------------------- instruments
def env_ar(n, a, r):
    e = np.ones(n)
    na, nr = int(a * SR), int(r * SR)
    if na:
        e[:na] = np.linspace(0, 1, na)
    if nr:
        e[-nr:] *= np.linspace(1, 0, nr) ** 2
    return e


def kick_sample():
    t = np.arange(int(0.5 * SR)) / SR
    f = 45 + 115 * np.exp(-t / 0.032) + 60 * np.exp(-t / 0.005)
    body = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t / 0.3)
    click = filt(noise(0.5), "highpass", 1800) * np.exp(-t / 0.004) * 0.35
    x = np.tanh(1.8 * (body + click)) / np.tanh(1.8)
    return x * env_ar(len(t), 0.0008, 0.05)


def clap_sample():
    t = np.arange(int(0.45 * SR)) / SR
    n = filt(noise(0.45), "bandpass", [900, 3200])
    e = sum((t >= d) * np.exp(-np.clip(t - d, 0, None) / 0.006) for d in (0, 0.011, 0.023))
    e += 0.55 * (t >= 0.031) * np.exp(-np.clip(t - 0.031, 0, None) / 0.14)
    x = n * e
    return x / np.abs(x).max()


def hat_sample(decay):
    d = max(0.12, decay * 6)
    t = np.arange(int(d * SR)) / SR
    # metallic: a few detuned squares (808 style), band-passed, plus air noise
    ratios = [2, 3, 4.16, 5.43, 6.79, 8.21]
    met = sum(np.sign(np.sin(2 * np.pi * 330 * r * t + rng.uniform(0, 6))) for r in ratios)
    x = filt(0.4 * met + 1.2 * noise(d), "highpass", 7000, 4)
    x = x * np.exp(-t / decay)
    return x / np.abs(x).max()


def snare_sample():
    t = np.arange(int(0.3 * SR)) / SR
    tone = np.sin(2 * np.pi * (180 + 40 * np.exp(-t / 0.01)) * t) * np.exp(-t / 0.06)
    nz = filt(noise(0.3), "bandpass", [1200, 9000]) * np.exp(-t / 0.11)
    x = 0.6 * tone + nz
    return x / np.abs(x).max()


def crash_sample(dur=3.5):
    t = np.arange(int(dur * SR)) / SR
    x = filt(noise(dur), "highpass", 3500, 2) * np.exp(-t / 1.0)
    x += 0.3 * filt(noise(dur), "bandpass", [5000, 9000]) * np.exp(-t / 0.35)
    x *= env_ar(len(t), 0.002, 0.3)
    return x / np.abs(x).max()


def impact_sample(dur=3.0):
    t = np.arange(int(dur * SR)) / SR
    f = 28 + 60 * np.exp(-t / 0.11)
    boom = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t / 0.9)
    thump = filt(noise(dur), "lowpass", 1800) * np.exp(-t / 0.07)
    x = np.tanh(1.4 * (boom + 0.5 * thump)) * env_ar(len(t), 0.001, 0.4)
    return x / np.abs(x).max()


def riser(dur, top=8000):
    n = int(dur * SR)
    t = np.arange(n) / SR
    p = t / dur
    nz = noise(dur)
    centers = np.geomspace(180, top, 10)
    pos = p ** 1.4 * (len(centers) - 1)
    x = sum(filt(nz, "bandpass", [c / 1.4, min(c * 1.4, 20000)]) * np.exp(-0.5 * ((pos - i) / 1.1) ** 2)
            for i, c in enumerate(centers))
    tone = saw(110 * 2 ** (3 * p ** 1.2)) * 0.25
    tone = filt(tone, "lowpass", 3000)
    x = (x / np.abs(x).max() + tone) * p ** 2.2
    return x * env_ar(n, 0.05, 0.012)


def downlifter(dur):
    return riser(dur)[::-1] * np.linspace(1, 0.3, int(dur * SR)) ** 2


def saw(freq, phase0=None):
    """PolyBLEP saw for a per-sample (or constant) frequency array."""
    freq = np.broadcast_to(np.asarray(freq, float), np.shape(freq) or (1,))
    dt = freq / SR
    ph = (np.cumsum(dt) + (rng.uniform() if phase0 is None else phase0)) % 1.0
    y = 2 * ph - 1
    m = ph < dt
    x = ph[m] / dt[m]
    y[m] -= x + x - x * x - 1
    m = ph > 1 - dt
    x = (ph[m] - 1) / dt[m]
    y[m] -= x * x + x + x + 1
    return y


def pluck(f, dur, bright=1.0, decay=1.0):
    """Additive pluck: saw harmonics whose highs die faster (a closing filter, alias-free by construction)."""
    t = np.arange(int(dur * SR)) / SR
    x = np.zeros_like(t)
    for k in range(1, int(min(9000 / f, 40)) + 1):
        x += np.sin(2 * np.pi * k * f * t + 0.3 * k) / k * np.exp(-t * (4 + 2.2 * k / bright) / decay)
    return x * env_ar(len(t), 0.002, min(0.05, dur / 3))


def lead_note(f, dur):
    rel = 0.25
    t = np.arange(int((dur + rel) * SR)) / SR
    vib = 1 + 0.004 * np.sin(2 * np.pi * 5.2 * t) * np.clip((t - 0.2) / 0.3, 0, 1)
    ph = 2 * np.pi * np.cumsum(f * vib) / SR
    x = np.zeros_like(t)
    for k in range(1, int(min(8000 / f, 24)) + 1):
        x += np.sin(k * ph) / k ** 1.15 * np.exp(-t * (0.6 + 0.25 * k))
    e = np.minimum(1, t / 0.012) * np.where(t < dur, 1, np.exp(-(t - dur) / 0.08))
    return x * e


# ---------------------------------------------------------------- harmony
CHORDS = {  # pad voicing (MIDI), bass root (MIDI)
    "Am": ([57, 60, 64, 67], 33), "F": ([57, 60, 64, 65], 29), "C": ([55, 60, 62, 67], 36),
    "G": ([55, 59, 62, 67], 31), "Cmaj9": ([48, 55, 64, 71, 74], 24)}
PROG = ["Am", "F", "C", "G"]

secs = plan["sections"]
music_secs = [s for s in secs if s["music"]]
n_bars = int(round((END_CUE - M0) / BAR))          # bars 0 .. n_bars-1 before the final chord


def section_at(t):
    for s in secs:
        if s["start"] - 1e-6 <= t < s["end"] - 1e-6:
            return s
    return secs[-1]


def music_only(t):
    s = section_at(t)
    return any(m["start"] - 1e-6 <= t < m["end"] - 1e-6 for m in s["musicOnly"])


# chord per half bar: restart the progression at each section, a V lead-in, V before the outro, IV-V-I cadence
chart = []
for b in range(n_bars):
    t = bar_t(b)
    s = section_at(t)
    sb = int(round(s["startBar"])) if s["startBar"] is not None else 0
    if s["id"] == "drop":
        sb = 1
    c = PROG[(b - sb) % 4] if b >= 1 else "G"
    if b == n_bars - 3:      # last bar before the outro (the CTA's music-only bar)
        c = "G"
    if b == n_bars - 2:      # outro downbeat: the deceptive vi under "Longarm."
        c = "Am"
    chart.append((c, c))
    if b == n_bars - 1:
        chart[-1] = ("F", "G")
CHART_HALF = [c for pair in chart for c in pair]   # index = half bar from M0

PROFILES = {   # per-layer gains by energy
    "full":      dict(kick=1.0, clap=1.0, hat=1.0, ohat=1.0, hat16=0.5, bass=1.0, pad=0.9, arp=1.0, bright=1.0),
    "verse":     dict(kick=0.85, clap=0.45, hat=0.55, ohat=0.0, hat16=0.0, bass=0.85, pad=0.75, arp=0.55, bright=0.45),
    "verse+":    dict(kick=0.9, clap=0.7, hat=0.7, ohat=0.5, hat16=0.0, bass=0.9, pad=0.8, arp=0.7, bright=0.6),
    "half":      dict(kick=1.0, clap=0.9, hat=0.6, ohat=0.6, hat16=0.0, bass=0.9, pad=1.0, arp=0.5, bright=0.7, halftime=1),
    "breakdown": dict(kick=0.0, clap=0.0, hat=0.25, ohat=0.0, hat16=0.0, bass=0.6, pad=1.5, arp=0.4, bright=0.8, pulse=1),
}


def profile(b):
    t = bar_t(b)
    s = section_at(t)
    if b == 0:
        return dict(kick=0, clap=0, hat=0, ohat=0, hat16=0, bass=0, pad=0.0, arp=0, bright=0.2, leadin=1)
    if music_only(t) and s["id"] != "cta":
        return dict(PROFILES["full"], lead=1)
    if music_only(t) and s["id"] == "cta":
        return dict(PROFILES["breakdown"], build=1)
    return PROFILES[s["energy"]]


profs = [profile(b) for b in range(n_bars)]
cues = plan["cues"]
impact_bars = {int(round(c["bar"])) for c in cues if c["type"] == "impact"}
fill_cues = [c for c in cues if c["type"] == "fill"]

# ---------------------------------------------------------------- drums
K, CL, HC, HO, SN = kick_sample(), clap_sample(), hat_sample(0.035), hat_sample(0.22), snare_sample()
kick_times, pulse_times = [], []
# beats muted right before an impact downbeat (the pre-drop gap)
gap_beats = {round((bar_t(b) - BEAT - M0) / BEAT) for b in impact_bars if b > 1}
for b in range(1, n_bars):
    p = profs[b]
    for q in range(4):
        tb = bar_t(b) + q * BEAT
        if round((tb - M0) / BEAT) in gap_beats:
            continue
        if p.get("halftime"):
            if q in (0,):
                tracks["kick"].add(tb, K, p["kick"]); kick_times.append(tb)
            if q == 2:
                tracks["clap"].add(tb, CL, 0.5 * p["clap"])
            if q == 1:
                tracks["kick"].add(tb + BEAT / 2, K, 0.7 * p["kick"]); kick_times.append(tb + BEAT / 2)
        else:
            if p["kick"]:
                tracks["kick"].add(tb, K, p["kick"]); kick_times.append(tb)
            if q in (1, 3) and p["clap"]:
                tracks["clap"].add(tb, CL, 0.5 * p["clap"])
        if p.get("pulse"):
            pulse_times.append(tb)
        if p["hat"]:
            tracks["hat"].add(tb + BEAT / 2, HC, 0.28 * p["hat"], pan=0.25)
            tracks["hat"].add(tb, HC, 0.12 * p["hat"], pan=-0.2)
        if p["ohat"] and q % 2 == 1:
            tracks["hat"].add(tb + BEAT / 2, HO, 0.12 * p["ohat"], pan=0.3)
        if p["hat16"]:
            for k in (1, 3):
                tracks["hat"].add(tb + k * BEAT / 4, HC, 0.1 * p["hat16"], pan=-0.35)
        # future-garage shuffle: a ghost clap on the swung 16th before beat 3 in full bars
        if p["clap"] >= 1.0 and q == 1 and not p.get("halftime"):
            tracks["clap"].add(tb + BEAT * 0.75 + 0.03, CL, 0.12, pan=-0.3)

# fills: a 16th snare run on the last beat before a plain section change; impact sections get a gap instead
impact_secs = {c["section"] for c in cues if c["type"] == "impact"}
for c in fill_cues:
    if c["section"] in impact_secs:
        continue
    for k in range(4):
        tracks["perc"].add(c["t"] + k * BEAT / 4, SN, 0.12 + 0.07 * k, pan=0.1 * (k - 1.5))
# lead-in bar: snare roll building into the drop
for k in range(12):
    tracks["perc"].add(bar_t(0) + BEAT + k * BEAT / 4 if k < 4 else bar_t(0) + 2 * BEAT + (k - 4) * BEAT / 8,
                       SN, 0.05 + 0.25 * (k / 11) ** 1.5)

# risers, downlifters, impacts, crashes
CR, IMP = crash_sample(), impact_sample()
for c in cues:
    if c["type"] == "riser":
        d = c["len_bars"] * BAR
        tracks["fx"].add(c["t"], riser(d), 0.22 if c["len_bars"] > 1 else 0.3)
    elif c["type"] == "downlifter":
        tracks["fx"].add(c["t"], downlifter(c["len_bars"] * BAR), 0.2)
        tracks["fx"].add(c["t"], CR, 0.15)
    elif c["type"] == "impact":
        tracks["fx"].add(c["t"], IMP, 0.8)
        tracks["fx"].add(c["t"], CR, 0.32)
        tracks["fx"].add(c["t"] + BAR, downlifter(BAR), 0.08)
# a crash at every other section start too, quieter
for s in music_secs[1:]:
    if s["id"] not in {c["section"] for c in cues if c["type"] in ("impact", "downlifter")}:
        tracks["fx"].add(s["start"], CR, 0.14)

# ---------------------------------------------------------------- sidechain pump
def pump(times, depth, tau=0.16):
    times = np.asarray(sorted(times))
    if len(times) == 0:
        return np.ones(N)
    idx = np.searchsorted(times, T, side="right") - 1
    since = np.where(idx >= 0, T - times[np.clip(idx, 0, None)], 10.0)
    dip = depth * np.exp(-since / tau) * np.clip(since / 0.004, 0, 1)
    return 1 - dip


PUMP = np.minimum(pump(kick_times, 1.0), pump(pulse_times, 0.55, 0.22))

# ---------------------------------------------------------------- pads (continuous detuned saws, chord per half bar)
half_idx = np.clip(((T - M0) / (BAR / 2)).astype(int), 0, len(CHART_HALF) - 1)
chord_names = np.array(CHART_HALF)[half_idx]


def voice_freq(slot, octave=0):
    m = np.array([CHORDS[c][0][slot] for c in CHART_HALF], float) + 12 * octave
    f = hz(m)[half_idx]
    k = int(0.03 * SR)            # 30 ms glide between chords
    return smooth(f, k)


pad = np.zeros((N, 2))
for slot in range(4):
    fv = voice_freq(slot)
    for ch, dets in ((0, (-9, 4)), (1, (-4, 9))):
        for d in dets:
            pad[:, ch] += saw(fv * 2 ** (d / 1200))
        pad[:, ch] += 0.6 * saw(fv * 0.5 * 2 ** ((dets[0] + dets[1]) / 2400))  # octave-down body
dark = filt(pad, "lowpass", 900, 2)
bright = filt(pad, "lowpass", 3200, 2)
bright_curve = np.zeros(N)
pad_gain = np.zeros(N)
for b in range(n_bars):
    i0, i1 = int(bar_t(b) * SR), int(bar_t(b + 1) * SR)
    bright_curve[i0:i1] = profs[b]["bright"]
    pad_gain[i0:i1] = profs[b]["pad"]
# lead-in bar: pad swells in on the V chord
i0, i1 = int(bar_t(0) * SR), int(bar_t(1) * SR)
pad_gain[i0:i1] = np.linspace(0, 0.6, i1 - i0) ** 2
sm = int(0.25 * SR)
bright_curve = smooth(bright_curve, sm)
pad_gain = smooth(pad_gain, sm // 5)
mixpad = dark * (1 - bright_curve[:, None]) + bright * bright_curve[:, None]
pad_pump = 1 - 0.55 * (1 - PUMP)
tracks["pad"].x += filt(mixpad, "highpass", 140) * (pad_gain * pad_pump)[:, None] * 0.05

# ---------------------------------------------------------------- bass (sub sine + filtered saw, rolling 8ths)
bass = np.zeros(N)
for b in range(1, n_bars):
    p = profs[b]
    if not p["bass"]:
        continue
    for h in range(2):
        root = CHORDS[CHART_HALF[2 * b + h]][1]
        steps = [(0, 4)] if p.get("halftime") else ([(k, 1) for k in range(4)] if not p.get("pulse") else [(0, 2), (2, 2)])
        for k, ln in steps:
            tn = bar_t(b) + h * BAR / 2 + k * BEAT / 2
            if int((tn - M0) / BEAT + 1e-6) in gap_beats:
                continue
            dur = ln * BEAT / 2 * 0.92
            m = root + (12 if (k % 2 == 1 and p["kick"] >= 1.0 and not p.get("halftime")) else 0)
            f = hz(m + 12)
            n = int(dur * SR)
            t = np.arange(n) / SR
            note = 0.65 * np.sin(2 * np.pi * f / 2 * t) + 0.35 * filt(saw(np.full(n, f)), "lowpass", 380 + 500 * p["bright"])
            e = env_ar(n, 0.004, 0.03) * (0.75 + 0.25 * np.exp(-t / 0.08))
            i = int(tn * SR)
            bass[i:i + n] += (note * e * p["bass"])[: N - i]
tracks["bass"].add(0, bass * (1 - 0.65 * (1 - PUMP)), 0.42)

# ---------------------------------------------------------------- arp (16ths over chord tones, ping-pong delay)
ARP_PAT = [0, 2, 1, 3, 2, 4, 3, 1]
pluck_cache = {}
for b in range(1, n_bars):
    p = profs[b]
    if not p["arp"]:
        continue
    for s16 in range(16):
        if p.get("pulse") and s16 % 2:
            continue
        tn = bar_t(b) + s16 * BEAT / 4
        if round((tn - M0) / BEAT) in gap_beats:
            continue
        tones = sorted(CHORDS[CHART_HALF[2 * b + s16 // 8]][0])
        tones = tones + [tones[0] + 12]
        m = tones[ARP_PAT[s16 % 8] % len(tones)] + 12
        acc = 1.0 if s16 % 4 == 0 else 0.7
        key = (m, p["bright"])
        if key not in pluck_cache:
            pluck_cache[key] = pluck(float(hz(m)), 0.32, bright=0.4 + 0.8 * p["bright"], decay=0.8)
        x = pluck_cache[key]
        tracks["arp"].add(tn, x * (1 - 0.3 * (1 - PUMP[int(tn * SR): int(tn * SR) + len(x)])), 0.05 * acc * p["arp"],
                          pan=0.35 * np.sin(s16 * np.pi / 4))

# ---------------------------------------------------------------- lead hook in music-only bars
MOTIF = {  # (beat, beats, midi) per chord
    "Am": [(0, 1.5, 76), (1.5, 0.5, 74), (2, 1, 72), (3, 1, 71)],
    "F": [(0, 1.5, 72), (1.5, 0.5, 69), (2, 2, 72)],
    "C": [(0, 1, 67), (1, 0.5, 72), (1.5, 1, 74), (2.5, 1.5, 76)],
    "G": [(0, 1, 74), (1, 1, 71), (2, 1.5, 67), (3.5, 0.5, 69)],
}
for b in range(1, n_bars):
    if not profs[b].get("lead"):
        continue
    for bt, ln, m in MOTIF[CHART_HALF[2 * b]]:
        tracks["lead"].add(bar_t(b) + bt * BEAT, lead_note(float(hz(m)), ln * BEAT * 0.95), 0.06)

# ---------------------------------------------------------------- hook: faint drone + ticking clock on the grid
hook_end = bar_t(1)
i1 = int(hook_end * SR)
t = T[:i1]
grow = np.clip(t / M0, 0, 1) ** 2
drone = (np.sin(2 * np.pi * 55 * t) + 0.5 * np.sin(2 * np.pi * 82.41 * t + 1)
         + 0.25 * filt(saw(np.full(i1, 110.0)), "lowpass", 260))
drone *= (0.25 + 0.75 * grow) * env_ar(i1, 1.5, 0.15) * (1 + 0.15 * np.sin(2 * np.pi * 0.25 * t))
tracks["drone"].add(0, drone, 0.1)
tick_t = np.arange(int(0.05 * SR)) / SR
TICK = filt(noise(0.05), "bandpass", [2500, 6000]) * np.exp(-tick_t / 0.006) + 0.5 * np.sin(2 * np.pi * 1600 * tick_t) * np.exp(-tick_t / 0.01)
tb = M0 - BEAT
while tb > 0.2:
    k = round((M0 - tb) / BEAT)
    tracks["drone"].add(tb, TICK, (0.06 if k % 4 == 0 else 0.036) * (0.4 + 0.6 * (tb / M0)), pan=0.2 if k % 2 else -0.2)
    tb -= BEAT

# ---------------------------------------------------------------- final chord on the end cue
tf = END_CUE
dur = TOTAL - tf
n = int(dur * SR)
t = np.arange(n) / SR
fin = np.zeros((n, 2))
for m in CHORDS["Cmaj9"][0]:
    for ch, d in ((0, -6), (1, 6)):
        fin[:, ch] += saw(np.full(n, float(hz(m)) * 2 ** (d / 1200)))
fin = filt(fin, "lowpass", 2600) * (np.exp(-t / 1.4) * env_ar(n, 0.01, 0.8))[:, None]
tracks["final"].add(tf, fin, 0.05)
tracks["final"].add(tf, np.sin(2 * np.pi * hz(36) * t) * np.exp(-t / 1.2) * env_ar(n, 0.01, 0.5), 0.35)
for k, m in enumerate([72, 79, 84, 88]):
    tracks["final"].add(tf + k * 0.06, pluck(float(hz(m)), 2.5, bright=1.2, decay=6), 0.06, pan=(-0.4, 0.4, -0.2, 0.2)[k])
tracks["final"].add(tf, IMP, 0.45)
tracks["final"].add(tf, CR, 0.25)
# everything else stops at the cue (short fade so the reverb tail is the final chord's own)
cut = np.ones(N)
ic = int(tf * SR)
cut[ic:ic + int(0.03 * SR)] = np.linspace(1, 0, int(0.03 * SR))
cut[ic + int(0.03 * SR):] = 0
for k in ("kick", "clap", "hat", "perc", "bass", "pad", "arp", "lead"):
    tracks[k].x *= cut[:, None]

# ---------------------------------------------------------------- stem balance (dB), set by measuring the stems
STEM_DB = {"kick": -7.5, "clap": 3, "hat": 5, "perc": 0, "bass": 0, "pad": 3.5, "arp": 9, "lead": 8, "fx": 0, "drone": 0, "final": 0}
for k, tr in tracks.items():
    tr.x *= np.float32(10 ** (STEM_DB[k] / 20))

# ---------------------------------------------------------------- room: ping-pong delay + convolution reverb
def ping_pong(x, d=0.375, fb=0.42, taps=6):
    y = np.zeros_like(x)
    mono = x.mean(axis=1)
    mono = filt(filt(mono, "lowpass", 4500), "highpass", 300)
    for k in range(1, taps + 1):
        i = int(k * d * SR)
        y[i:, k % 2] += mono[: N - i] * fb ** k
    return y


def make_ir(rt60=2.6, pre=0.025):
    n = int(rt60 * SR)
    t = np.arange(n) / SR
    ir = np.zeros((n, 2))
    for ch in range(2):
        nz = rng.standard_normal(n)
        lo = filt(nz, "lowpass", 2500) * np.exp(-6.9 * t / rt60)
        hi = filt(nz, "highpass", 2500) * np.exp(-6.9 * t / (rt60 * 0.45))
        ir[:, ch] = lo + 0.5 * hi
    ir *= np.minimum(1, t / 0.01)[:, None]          # soft onset
    ir = np.concatenate([np.zeros((int(pre * SR), 2)), ir])
    return ir / np.sqrt((ir ** 2).sum(axis=0))


IR = make_ir()
send = np.zeros(N)
for tr in tracks.values():
    if tr.delay:
        dl = ping_pong(tr.x) * tr.delay
        tr.x += dl
    if tr.send:
        send += tr.x.mean(axis=1) * tr.send
send = filt(send, "highpass", 250)
wet = np.stack([signal.oaconvolve(send, IR[:, ch])[:N] for ch in range(2)], axis=1)

# ---------------------------------------------------------------- master
dry = sum(tr.x for tr in tracks.values())
mix = dry + 0.9 * wet
mix = filt(mix, "highpass", 28)
mix = np.tanh(mix * 1.3) / 1.3          # gentle glue / soft clip
fade = int(0.4 * SR)
mix[-fade:] *= np.linspace(1, 0, fade)[:, None] ** 2
peak = np.abs(mix).max()
mix *= 10 ** (-1 / 20) / peak
sf.write(os.path.join(ROOT, "audio", "music.wav"), mix.astype(np.float32), SR, subtype="FLOAT")

meter = pyln.Meter(SR)
print(f"music.wav {TOTAL:.3f}s, bars 0-{n_bars} + final chord at {END_CUE:.3f}s")
print("chart:", " ".join(f"{b}:{a}" + ("" if a == c else "/" + c) for b, (a, c) in enumerate(chart)))
for s in secs:
    a, b = int(s["start"] * SR), int(s["end"] * SR)
    print(f"  {s['id']:8s} {s['energy']:9s} {meter.integrated_loudness(mix[a:b]):6.1f} LUFS")
mo = next(m for s in secs if s["id"] == "toolbox" for m in s["musicOnly"])
a, b = int((mo["start"] - 4) * SR), int(mo["end"] * SR)
print("  stems over toolbox (pre-master, dry):", ", ".join(
    f"{k} {meter.integrated_loudness(tr.x[a:b].astype(float)):.1f}" for k, tr in tracks.items() if np.abs(tr.x[a:b]).max() > 0))
