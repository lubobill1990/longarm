# Lay the voice-over lines out on the 120 BPM grid -> audio/vo.wav, timeline.json (the timing contract for the
# visuals) and music-plan.json (what tools/music.py composes to).
#   uv run -p 3.12 --no-project --with numpy --with soundfile python -I tools/timeline.py
# Rules: the hook (music:false) starts at 0.4 s with natural 0.35 s gaps. Music starts one bar (a lead-in) before the
# drop, so "Meet Longarm" lands on the bar-1 downbeat. After that every line starts on the next beat at least 0.25 s
# after the previous line; a grid:4 section starts on a bar line; bars:N adds N music-only bars after the section;
# tail_bars rings the final chord out after the last line.
import json, math, os
import numpy as np, soundfile as sf

# FILM_ROOT: a language's folder (lang/zh: its script.json; its audio/ and timeline.json go there)
ROOT = os.environ.get("FILM_ROOT") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SR = 48000
HOOK_START, HOOK_GAP = 0.4, 0.35
LEAD_GAP = 0.45   # silence between the hook's last word and the music's lead-in bar
MIN_GAP = 0.35     # between lines (same breath as the hook)
SECTION_GAP = float(os.environ.get("SECTION_GAP", 0.75))  # before a grid:4 section downbeat: keeps the fill on the beat before it clear of speech

# Arrangement intent per section, read by tools/music.py.
ENERGY = {"hook": "drone", "drop": "full", "idea": "verse", "how": "verse", "run": "verse+", "agents": "verse+",
          "survive": "half", "reach": "verse", "toolbox": "full", "cta": "breakdown", "outro": "full"}
IMPACTS = {"drop", "survive", "toolbox", "outro"}   # impact on the section's first downbeat


def r3(x):
    return round(x + 1e-9, 3)


def main():
    script = json.load(open(os.path.join(ROOT, "script.json"), encoding="utf-8"))
    vo = json.load(open(os.path.join(ROOT, "audio", "vo", "lines.json"), encoding="utf-8"))
    bpm = script["bpm"]
    beat = 60 / bpm
    bar = 4 * beat
    pre = vo["onset"]
    lines = {(l["section"], l["index"]): l for l in vo["lines"]}

    placed = []      # (section, line, start)
    sections = []
    t = HOOK_START
    music_start = None
    prev_end = 0.0

    def ceil_to(x, step):
        return music_start + math.ceil((x - music_start) / step - 1e-6) * step

    for si, sec in enumerate(script["sections"]):
        s = {"id": sec["id"], "lines": [], "musicOnly": [], "music": sec.get("music", True)}
        for i, _ in enumerate(sec["lines"]):
            l = lines[(sec["id"], i)]
            if not s["music"]:
                start = t if i == 0 else prev_end + HOOK_GAP
            elif i == 0:
                if music_start is None:      # first music section: lead-in bar, then the downbeat
                    music_start = r3(prev_end + LEAD_GAP)
                    s["leadIn"] = music_start
                    start = music_start + bar
                else:
                    start = ceil_to(prev_end + SECTION_GAP, bar) if sec.get("grid") == 4 else ceil_to(prev_end + MIN_GAP, beat)
            else:
                start = ceil_to(prev_end + MIN_GAP, beat)
            end = start - pre + l["duration"]
            placed.append((l, start))
            s["lines"].append({"text": l["text"], "start": r3(start), "end": r3(end),
                               "words": [{"w": w["w"], "start": r3(max(start, start - pre + w["start"])),
                                          "end": r3(min(end, start - pre + w["end"]))} for w in l["words"]]})
            prev_end = end
        if sec.get("bars"):
            mo = ceil_to(prev_end + 0.05, bar)
            s["musicOnly"].append({"start": r3(mo), "end": r3(mo + sec["bars"] * bar)})
            prev_end = mo + sec["bars"] * bar - SECTION_GAP   # next section starts right on that bar
        if sec.get("tail_bars"):
            end_cue = ceil_to(prev_end + 0.05, bar)
            s["musicOnly"].append({"start": r3(end_cue), "end": r3(end_cue + sec["tail_bars"] * bar)})
            total = end_cue + sec["tail_bars"] * bar
        sections.append(s)

    # Section spans tile the film: hook from 0 to the lead-in, the drop from the lead-in, others from their first line.
    for i, s in enumerate(sections):
        s["start"] = 0.0 if i == 0 else s.get("leadIn", s["lines"][0]["start"])
    for i, s in enumerate(sections):
        s["end"] = sections[i + 1]["start"] if i + 1 < len(sections) else r3(total)

    # vo.wav
    out = np.zeros(int(round(total * SR)) + 1, np.float32)
    for l, start in placed:
        x, sr = sf.read(os.path.join(ROOT, l["file"]), dtype="float32")
        assert sr == SR
        a = int(round((start - pre) * SR))
        out[a:a + len(x)] += x
    sf.write(os.path.join(ROOT, "audio", "vo.wav"), out, SR, subtype="PCM_24")

    timeline = {"bpm": bpm, "beat": beat, "bar": bar, "musicStart": r3(music_start),
                "dropDownbeat": r3(music_start + bar), "endCue": r3(end_cue), "total": r3(total),
                "voice": vo["voice"], "rate": vo["rate"],
                "sections": [{"id": s["id"], "start": r3(s["start"]), "end": r3(s["end"]), "musicOnly": s["musicOnly"],
                              "lines": s["lines"]} for s in sections]}
    json.dump(timeline, open(os.path.join(ROOT, "timeline.json"), "w", encoding="utf-8"), indent=1)

    def barno(x):
        return round((x - music_start) / bar, 3)

    plan = {"bpm": bpm, "beat": beat, "bar": bar, "musicStart": r3(music_start), "total": r3(total),
            "note": "bar n starts at musicStart + 2n s; bar 0 is the lead-in, bar 1 the drop downbeat; negative bars = hook (no music)",
            "endCue": {"t": r3(end_cue), "bar": barno(end_cue), "what": "final resolving chord, rings to total"},
            "sections": [], "cues": []}
    for i, s in enumerate(sections):
        plan["sections"].append({
            "id": s["id"], "energy": ENERGY.get(s["id"], "verse"), "music": s["music"],
            "start": r3(s["start"]), "end": r3(s["end"]),
            "startBar": barno(s["start"]) if s["music"] else None, "endBar": barno(s["end"]),
            "speech": [[l["start"], l["end"]] for l in s["lines"]],
            "musicOnly": [{**m, "startBar": barno(m["start"]), "endBar": barno(m["end"])} for m in s["musicOnly"]]})
        if i == 0:
            continue
        first = s["lines"][0]["start"]
        if s["id"] in IMPACTS:
            plan["cues"].append({"t": r3(first), "bar": barno(first), "type": "impact", "section": s["id"]})
        if s["id"] == "drop":
            plan["cues"].append({"t": r3(music_start), "bar": 0, "type": "riser", "len_bars": 1, "section": "drop"})
        elif s["id"] in IMPACTS:
            plan["cues"].append({"t": r3(first - 2 * bar), "bar": barno(first - 2 * bar), "type": "riser", "len_bars": 2, "section": s["id"]})
        elif s["id"] == "cta":   # into the breakdown: fall, not rise
            plan["cues"].append({"t": r3(first), "bar": barno(first), "type": "downlifter", "len_bars": 1, "section": s["id"]})
        if s["id"] != "drop":
            plan["cues"].append({"t": r3(first - beat), "bar": barno(first - beat), "type": "fill", "section": s["id"]})
    plan["cues"].append({"t": r3(end_cue), "bar": barno(end_cue), "type": "final", "section": "outro"})
    plan["cues"].sort(key=lambda c: c["t"])
    json.dump(plan, open(os.path.join(ROOT, "music-plan.json"), "w", encoding="utf-8"), indent=1)

    print(f"musicStart {music_start:.3f}  drop {music_start + bar:.3f}  endCue {end_cue:.3f}  total {total:.3f}")
    for s in sections:
        mo = ", ".join(f"{m['start']:.1f}-{m['end']:.1f}" for m in s["musicOnly"])
        print(f"{s['id']:8s} {s['start']:7.3f}-{s['end']:7.3f}  first line {s['lines'][0]['start']:7.3f}  music-only {mo}")


if __name__ == "__main__":
    main()
