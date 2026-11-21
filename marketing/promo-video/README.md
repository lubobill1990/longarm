# Longarm promo film

A 1920x1080 English promo (about three minutes): voice-over first, a beat grid
for the music, then a HyperFrames (HTML + GSAP) project whose every cue is
anchored to a spoken word. Generated media is not committed (`.gitignore`);
everything here rebuilds it.

| File | What |
|---|---|
| `script.json` | the narration: sections, lines, music-only bars, the 120 BPM grid |
| `tools/` (audio) | voice-over (edge-tts), word timings, timeline on the grid, the music (synthesized), sound effects, ducking and the mix |
| `timeline.json`, `music-plan.json` | the timing contract: section, line and word times in seconds |
| `assets/` | illustrations (gpt-image-2.5, from the Longarm icon; the icon itself comes from packaging/icons); `site-*.png` are captured by `tools/capture-site.mjs` |
| `tools/build-film.mjs` | writes `film/`: one composition per section, captions, the mix and the sound effects |

## Rebuild

```sh
npm install                                    # gsap (vendored into film/)
sh tools/build-audio.sh                        # voice-over, timeline, music, sfx, mix (uv, ffmpeg, network for edge-tts)
(cd ../../web && npx vite --port 5288) &       # then:
node tools/capture-site.mjs https://<the hub FILM_HUB_LINK leads to>   # the front page stills, as that hub
                                               # (its release and sign-in providers)
FILM_HUB_LINK=<short link to the hub> FILM_REPO=github.com/<owner>/<repo> \
  node tools/build-film.mjs                    # film/ (the film shows both; defaults: placeholders)
cd film && npx --yes hyperframes@0.8.200 check
npx --yes hyperframes@0.8.200 snapshot --at 5,20,60 --no-end   # look before rendering
npx --yes hyperframes@0.8.200 render -o ../renders/longarm-promo.mp4 --quality delivery --video-frame-format png
```

## Other languages

`lang/<code>/script.json` is the film in another language (`lang/zh`: Chinese, voice
`zh-CN-YunxiNeural`): the same sections and as many lines in each, `"lang"` set. The
same tools build it with `FILM_ROOT` pointing there - its `audio/`, `timeline.json`,
`music-plan.json` and `film/` are written next to its script; the assets are shared:

```sh
FILM_ROOT=lang/zh sh tools/build-audio.sh      # (the ASR listen-check is English only)
FILM_ROOT=lang/zh FILM_HUB_LINK=... FILM_REPO=... node tools/build-film.mjs
cd lang/zh/film && npx --yes hyperframes@0.8.200 render -o ../../../renders/longarm-promo.zh.mp4 --quality delivery --video-frame-format png
```

In `tools/build-film.mjs`, `tr(en, zh)` gives each anchor and on-screen line its
translation (the product's own UI, commands and names stay as they are); a
language written without spaces is anchored on the run of the voice's
segments (`atChars`). Its CJK text renders in the system's font (Microsoft
YaHei on Windows, where the film is rendered).

Changing a line in `script.json` means regenerating the audio first: the film
reads every time from `timeline.json`, and `build-film.mjs` fails loudly when a
word it anchors to is no longer spoken.
