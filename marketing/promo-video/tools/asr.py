# Listen-check by ASR: faster-whisper large-v3 word timestamps of a rendered wav, compared with timeline.json
# (word error rate over the script, onset drift of each line's first word).
#   uv run -p 3.12 --no-project --with faster-whisper --with numpy --with nvidia-cublas-cu12 --with nvidia-cudnn-cu12 \
#     python -I tools/asr.py audio/vo.wav [audio/asr-vo.json]
# Without a CUDA GPU it falls back to the CPU (int8, slower).
import difflib, json, os, re, subprocess, sys

try:  # pip CUDA wheels put their DLLs under nvidia/*/bin; Windows doesn't search there by itself
    import nvidia
    for p in nvidia.__path__:
        for sub in os.listdir(p):
            d = os.path.join(p, sub, "bin")
            if os.path.isdir(d):
                os.add_dll_directory(d)
                os.environ["PATH"] = d + os.pathsep + os.environ["PATH"]
except ImportError:
    pass
import numpy as np
from faster_whisper import WhisperModel

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.stdout.reconfigure(encoding="utf-8")


def norm(w):
    w = w.lower().replace("thirty-two", "32").replace("-", " ")
    return re.sub(r"[^a-z0-9' ]", "", w).split()


def main():
    src = sys.argv[1]
    dst = sys.argv[2] if len(sys.argv) > 2 else None
    pcm = subprocess.run(["ffmpeg", "-v", "error", "-i", src, "-ac", "1", "-ar", "16000", "-f", "f32le", "-"],
                         capture_output=True, check=True).stdout
    audio = np.frombuffer(pcm, np.float32).copy()
    try:
        m = WhisperModel("large-v3", device="cuda", compute_type="float16")
    except Exception:
        m = WhisperModel("large-v3", device="cpu", compute_type="int8")
    # The prompt only helps spelling of product names; pronunciation is judged on the words it hears.
    segs, _ = m.transcribe(audio, language="en", word_timestamps=True, vad_filter=False,
                           initial_prompt="Longarm, Claude Code, Copilot CLI, Codex, Gemini, Tailscale, QUIC, tmux, VS Code.")
    heard = []
    for s in segs:
        print(f"[{s.start:7.2f}-{s.end:7.2f}] {s.text.strip()}")
        heard += [{"w": w.word.strip(), "start": round(w.start, 3), "end": round(w.end, 3)} for w in s.words]
    if dst:
        json.dump(heard, open(dst, "w", encoding="utf-8"), indent=1)

    tl = json.load(open(os.path.join(ROOT, "timeline.json"), encoding="utf-8"))
    ref = [(t, l) for s in tl["sections"] for l in s["lines"] for t in norm(l["text"])]
    hyp = [(t, w) for w in heard for t in norm(w["w"])]
    sm = difflib.SequenceMatcher(a=[r[0] for r in ref], b=[h[0] for h in hyp], autojunk=False)
    errs = 0
    for op, a0, a1, b0, b1 in sm.get_opcodes():
        if op != "equal":
            errs += max(a1 - a0, b1 - b0)
            print(f"  {op:8s} script: {' '.join(r[0] for r in ref[a0:a1])!r:40s} heard: {' '.join(h[0] for h in hyp[b0:b1])!r}")
    print(f"WER {errs / len(ref):.1%} ({errs} of {len(ref)} words)")

    # first-word onset drift (ASR start vs timeline) for lines whose first word was matched
    first = {}
    for a, b, n in sm.get_matching_blocks():
        for k in range(n):
            r, h = ref[a + k], hyp[b + k]
            if r[1]["words"] and norm(r[1]["words"][0]["w"])[:1] == [r[0]] and id(r[1]) not in first:
                first[id(r[1])] = h[1]["start"] - r[1]["start"]
    d = np.array(list(first.values()))
    print(f"line onsets matched {len(d)}; ASR - timeline: median {np.median(d) * 1000:.0f} ms "
          "(whisper word starts run early after silence; tools/check.py measures the real onsets)")


if __name__ == "__main__":
    main()
