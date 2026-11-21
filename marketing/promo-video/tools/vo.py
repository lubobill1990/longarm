# Voice-over, one file per script line: edge-tts (WordBoundary events = per-word timings) -> trim silence
# -> loudness-match every line to -17 LUFS. Output: audio/vo/<section>-<n>.wav (48 kHz mono) + audio/vo/lines.json.
#   uv run -p 3.12 --no-project --with edge-tts --with numpy --with soundfile --with pyloudnorm python -I tools/vo.py [--rate=-5%]
# TTS responses are cached in audio/vo/raw by (voice, rate, text), so a rerun only fetches changed lines.
import argparse, asyncio, hashlib, json, os, re, subprocess, time
import numpy as np, soundfile as sf, pyloudnorm as pyln
import edge_tts

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SR = 48000
TARGET_LUFS = -17.0
PRE = 0.03  # seconds kept before the first sound of each line (its onset inside the file)

# Spelling substitutions for the TTS text only (words.json keeps the script's words). Checked with a phoneme
# recognizer (wav2vec2 espeak): "QUIC" is spelled out Q-U-I-C, "tmux" reads "t-mucks", lowercase "longarm"
# gets an "n" ("long-narm"), "NAT" drifts to "net".
SUBS = [
    (r"\b[Ll]ongarm\b", lambda m: m.group(0)[0] + "ong-arm"),
    (r"\bQUIC\b", "quick"),
    (r"\btmux\b", "tee-mux"),
    (r"\bNAT\b", "gnat"),
]


def tts_text(text, lang="en"):
    # another language's voice reads the English terms its own way: only the brand keeps its spelling fix
    for pat, rep in SUBS if lang == "en" else SUBS[:1]:
        text = re.sub(pat, rep, text)
    return text


async def synth(text, voice, rate, mp3, meta):
    c = edge_tts.Communicate(text, voice, rate=rate, boundary="WordBoundary")
    words = []
    with open(mp3 + ".part", "wb") as f:
        async for ch in c.stream():
            if ch["type"] == "audio":
                f.write(ch["data"])
            elif ch["type"] == "WordBoundary":
                words.append({"w": ch["text"], "start": ch["offset"] / 1e7, "end": (ch["offset"] + ch["duration"]) / 1e7})
    os.replace(mp3 + ".part", mp3)
    with open(meta, "w", encoding="utf-8") as f:
        json.dump(words, f, indent=1)


def decode(path):
    pcm = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", str(SR), "-f", "f32le", "-"],
                         capture_output=True, check=True).stdout
    return np.frombuffer(pcm, np.float32).copy()


def trim_bounds(x, pre=PRE, post=0.08):
    # 5 ms RMS frames; speech = frames within 45 dB of the loudest frame
    hop = SR // 200
    n = len(x) // hop
    rms = np.sqrt(np.mean(x[: n * hop].reshape(n, hop) ** 2, axis=1) + 1e-12)
    db = 20 * np.log10(rms)
    on = np.where(db > db.max() - 45)[0]
    a = max(0, on[0] * hop - int(pre * SR))
    b = min(len(x), (on[-1] + 1) * hop + int(post * SR))
    return a, b


def orig_words(text):
    # script words without surrounding punctuation ("say:" -> "say", "it's" kept)
    return [re.sub(r"^[^\w]+|[^\w]+$", "", t) for t in text.split() if re.search(r"\w", t)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rate", default="-5%")  # ~160 wpm; also what puts the film at 2:51
    args = ap.parse_args()
    script = json.load(open(os.path.join(ROOT, "script.json"), encoding="utf-8"))
    voice = script["voice"]
    # "lang": a script in a language written without spaces (zh) keeps the voice's own word segments
    lang = script.get("lang", "en")
    raw = os.path.join(ROOT, "audio", "vo", "raw")
    os.makedirs(raw, exist_ok=True)
    meter = pyln.Meter(SR)
    out = []
    for sec in script["sections"]:
        for i, text in enumerate(sec["lines"]):
            spoken = tts_text(text, lang)
            key = hashlib.sha1(f"{voice}|{args.rate}|{spoken}".encode()).hexdigest()[:12]
            mp3, meta = os.path.join(raw, key + ".mp3"), os.path.join(raw, key + ".json")
            for attempt in range(8):  # the service drops a request now and then
                if os.path.exists(mp3) and os.path.exists(meta):
                    break
                try:
                    asyncio.run(synth(spoken, voice, args.rate, mp3, meta))
                except edge_tts.exceptions.NoAudioReceived:
                    if attempt == 7:
                        raise
                    time.sleep(2 + 3 * attempt)
            x = decode(mp3)
            a, b = trim_bounds(x)
            x = x[a:b]
            # pad short lines for the BS.1770 gate (400 ms blocks) when measuring only
            m = x if len(x) > SR else np.concatenate([x, x, x])
            x = (x * 10 ** ((TARGET_LUFS - meter.integrated_loudness(m)) / 20)).astype(np.float32)
            peak = np.abs(x).max()
            if peak > 0.97:
                x *= 0.97 / peak
            name = f"{sec['id']}-{i + 1}"
            sf.write(os.path.join(ROOT, "audio", "vo", name + ".wav"), x, SR, subtype="FLOAT")
            dur = len(x) / SR
            tw = json.load(open(meta, encoding="utf-8"))
            ow = orig_words(text) if lang == "en" else [w["w"] for w in tw]
            if len(tw) != len(ow):
                raise SystemExit(f"{name}: {len(tw)} TTS words vs {len(ow)} script words: {[w['w'] for w in tw]}")
            off = a / SR
            words = [{"w": o, "start": round(min(max(w["start"] - off, 0), dur), 3), "end": round(min(max(w["end"] - off, 0), dur), 3)}
                     for o, w in zip(ow, tw)]
            out.append({"section": sec["id"], "index": i, "file": f"audio/vo/{name}.wav", "text": text, "tts": spoken,
                        "duration": round(dur, 3), "words": words})
            print(f"{name:10s} {dur:5.2f}s  {spoken}")
    json.dump({"voice": voice, "rate": args.rate, "lufs": TARGET_LUFS, "onset": PRE, "lines": out},
              open(os.path.join(ROOT, "audio", "vo", "lines.json"), "w", encoding="utf-8"), indent=1)
    print(f"total speech {sum(l['duration'] for l in out):.1f}s in {len(out)} lines")


if __name__ == "__main__":
    main()
