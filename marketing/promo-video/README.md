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
node tools/capture-site.mjs https://<the hub aka.ms/longarm leads to>   # the front page stills, as that hub
                                               # (its release and sign-in providers; the film shows HUB_LINK)
node tools/build-film.mjs                      # film/
cd film && npx --yes hyperframes@0.8.200 check
npx --yes hyperframes@0.8.200 snapshot --at 5,20,60 --no-end   # look before rendering
npx --yes hyperframes@0.8.200 render -o ../renders/longarm-promo.mp4 --quality delivery --video-frame-format png
```

Changing a line in `script.json` means regenerating the audio first: the film
reads every time from `timeline.json`, and `build-film.mjs` fails loudly when a
word it anchors to is no longer spoken.
