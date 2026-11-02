// Generates the HyperFrames project film/ from timeline.json (the voice-over
// timing contract written by tools/make-audio) and assets/: one sub-composition
// per script section, every cue anchored to a spoken word (at()), captions,
// the mix and the sound effects.
//   node tools/build-film.mjs        then: cd film && npx hyperframes@0.8.200 check / render
import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const film = process.env.FILM ?? join(root, 'film')
const web = join(root, '..', '..', 'web')
const T = JSON.parse(readFileSync(process.env.TIMELINE ?? join(root, 'timeline.json'), 'utf8'))
const SFX = existsSync(join(root, 'audio/sfx/index.json')) ? JSON.parse(readFileSync(join(root, 'audio/sfx/index.json'), 'utf8')) : null
// where viewers open the hub (a short link to its front page); the site stills
// come from tools/capture-site.mjs run against the hub it points to
const HUB_LINK = 'aka.ms/longarm'
const W = 1920
const H = 1080
const BEAT = T.beat ?? 60 / T.bpm
const r3 = (x) => Math.round(x * 1000) / 1000

// ---- timing ---------------------------------------------------------------

const norm = (s) => s.toLowerCase().replace(/[^a-z0-9]+/g, '')
const sections = T.sections.map((s, i) => ({ ...s, a: s.start, b: i + 1 < T.sections.length ? T.sections[i + 1].start : T.total }))
const sec = (id) => sections.find((s) => s.id === id) ?? fail(`no section ${id}`)
function fail(m) {
  throw new Error(m)
}

/** Section-local time of a spoken word (or of the first of several words). */
function at(s, text, { line, after = 0, end = false } = {}) {
  const want = text.split(/\s+/).map(norm).filter(Boolean)
  const lines = line === undefined ? s.lines : [s.lines[line]]
  const words = lines.flatMap((l) => l.words)
  for (let i = 0; i < words.length; i++) {
    if (words[i].start - s.a < after - 0.05) continue
    // prefix match ("thirty" in "thirty-two"), exact for short words ("a" is not "agents")
    const hit = (w, k) => words[i + k] && (w.length <= 2 ? norm(words[i + k].w) === w : norm(words[i + k].w).startsWith(w))
    if (want.every(hit)) {
      const wd = end ? words[i + want.length - 1] : words[i]
      return r3((end ? wd.end : wd.start) - s.a)
    }
  }
  return fail(`${s.id}: "${text}" not spoken${line !== undefined ? ` in line ${line}` : ''}`)
}
const L = (s, i) => ({ a: r3(s.lines[i].start - s.a), b: r3(s.lines[i].end - s.a) })
const dur = (s) => r3(s.b - s.a)
/** Next beat (absolute grid) at or after section-local t. */
const onBeat = (s, t) => r3(Math.ceil((s.a + t - T.musicStart) / BEAT - 1e-6) * BEAT + T.musicStart - s.a)

// ---- GSAP snippets (strings; t = section-local seconds) ----------------------

const J = (o) => JSON.stringify(o).replace(/"([a-zA-Z_]+)":/g, '$1:')
const ft = (sel, from, to, t) => `tl.fromTo(${JSON.stringify(sel)},${J(from)},${J({ ...to })},${r3(t)});\n`
const to = (sel, v, t) => `tl.to(${JSON.stringify(sel)},${J(v)},${r3(t)});\n`
const set = (sel, v, t) => `tl.set(${JSON.stringify(sel)},${J(v)},${r3(t)});\n`
const pop = (sel, t, d = 0.5, extra = {}) => ft(sel, { opacity: 0, scale: 0.6, y: 30 }, { opacity: 1, scale: 1, y: 0, duration: d, ease: 'back.out(2)', ...extra }, t)
const blurIn = (sel, t, d = 0.35, extra = {}) => ft(sel, { opacity: 0, filter: 'blur(14px)', y: 24 }, { opacity: 1, filter: 'blur(0px)', y: 0, duration: d, ease: 'power3.out', ...extra }, t)
const out = (sel, t, d = 0.25) => to(sel, { opacity: 0, filter: 'blur(10px)', duration: d, ease: 'power2.in' }, t)
const slam = (sel, t) => ft(sel, { opacity: 0, scale: 1.35, filter: 'blur(18px)' }, { opacity: 1, scale: 1, filter: 'blur(0px)', duration: 0.28, ease: 'expo.out' }, t)
const shake = (sel, t, a = 14) =>
  [0, 1, 2, 3, 4, 5].map((k) => to(sel, { x: k === 5 ? 0 : (k % 2 ? -1 : 1) * a * (1 - k / 6), y: k === 5 ? 0 : (k % 2 ? 1 : -1) * a * 0.5 * (1 - k / 6), duration: 0.045, ease: 'none' }, t + k * 0.045)).join('')
const chars = (text) => [...text].map((c) => `<span class="ch">${esc(c)}</span>`).join('')
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
/** A number that changes at given times: one span per value, shown in turn. */
function steps(id, values, times) {
  const html = `<span class="num" id="${id}">${values.map((v, i) => `<span class="v v${i}"${i ? ' style="opacity:0"' : ''}>${v}</span>`).join('')}</span>`
  const js = values
    .map((_, i) => (i ? set(`#${id} .v${i - 1}`, { opacity: 0 }, times[i]) + set(`#${id} .v${i}`, { opacity: 1 }, times[i]) : ''))
    .join('')
  return { html, js }
}

// ---- shared look ------------------------------------------------------------

const C = {
  paper: '#f4f3ff',
  paper2: '#e9e7ff',
  ink: '#13133b',
  ink2: '#4b4c76',
  ink3: '#8a8bb0',
  line: '#dddbf5',
  blue: '#3c46f2',
  blue2: '#6b5cff',
  green: '#1fc46a',
  orange: '#ff8a1f',
  purple: '#8b5cf6',
  sky: '#27a6f5',
  red: '#ff4d4f',
  night: '#0c0d2c',
  night2: '#15163d',
  nline: '#2a2b5c',
}
const BASE_CSS = `
@font-face{font-family:'Bricolage';font-weight:200 800;src:url('fonts/bricolage.woff2') format('woff2-variations')}
@font-face{font-family:'Onest';font-weight:100 900;src:url('fonts/onest.woff2') format('woff2-variations')}
@font-face{font-family:'JetBrains Mono';font-weight:100 800;src:url('fonts/jbmono.woff2') format('woff2-variations')}
*{margin:0;padding:0;box-sizing:border-box}
html,body{width:${W}px;height:${H}px;overflow:hidden;background:${C.night};font-family:'Onest',sans-serif;-webkit-font-smoothing:antialiased}
.scene{position:absolute;left:0;top:0;width:${W}px;height:${H}px}
.stage{position:absolute;inset:0;overflow:hidden}
.stage.paper{background:radial-gradient(circle at 85% 10%,#dcdcff 0,transparent 40%),radial-gradient(circle at 5% 90%,#d7f7e6 0,transparent 35%),${C.paper};color:${C.ink}}
.stage.night{background:radial-gradient(circle at 50% -10%,#2b2a7a 0,transparent 55%),${C.night};color:#e8e8ff}
.grid{position:absolute;inset:0;background-image:linear-gradient(rgba(107,92,255,.14) 1px,transparent 1px),linear-gradient(90deg,rgba(107,92,255,.14) 1px,transparent 1px);background-size:60px 60px;-webkit-mask-image:radial-gradient(ellipse at 50% 30%,#000 15%,transparent 70%)}
.abs{position:absolute}
.h{opacity:0}
.disp{font-family:'Bricolage',sans-serif;font-weight:800;letter-spacing:-.035em;line-height:.98}
.mono{font-family:'JetBrains Mono',monospace}
.kicker{font:700 22px/1 'JetBrains Mono',monospace;letter-spacing:.14em;text-transform:uppercase;color:${C.blue};background:rgba(60,70,242,.1);padding:10px 16px;border-radius:10px;display:inline-block}
.night .kicker{color:#b3abff;background:rgba(107,92,255,.2)}
.card{background:#fff;border:2px solid ${C.line};border-radius:28px;box-shadow:0 40px 80px -50px rgba(19,19,59,.45)}
.night .card{background:${C.night2};border-color:${C.nline};box-shadow:0 40px 80px -40px rgba(0,0,0,.6)}
.term{background:${C.night};border-radius:22px;overflow:hidden;box-shadow:0 40px 90px -30px rgba(12,13,44,.75);border:1px solid #23245a}
.term .bar{display:flex;align-items:center;gap:9px;padding:14px 18px;background:#17183d}
.term .bar i{width:14px;height:14px;border-radius:50%;background:#ff5f57}.term .bar i:nth-child(2){background:#febc2e}.term .bar i:nth-child(3){background:#28c840}
.term .bar span{margin-left:10px;color:#8a8bb0;font:500 18px/1 'JetBrains Mono',monospace}
.term pre{padding:22px 26px;font:500 25px/1.6 'JetBrains Mono',monospace;color:#d8d9ff;white-space:pre-wrap}
.ok{color:#5cf09a}.dim{color:#8a8bb0}.win{color:#ffd166;font-weight:700}.cmd{color:#fff}.prompt{color:#ff8a5c}
.chip{display:inline-flex;align-items:center;gap:10px;padding:12px 20px;border-radius:999px;background:#fff;border:2px solid ${C.line};font:600 24px/1 'Onest',sans-serif;color:${C.ink}}
.tile{width:96px;height:96px;border-radius:26px;display:grid;place-items:center;color:#fff;font:800 44px/1 'JetBrains Mono',monospace;box-shadow:0 8px 0 rgba(0,0,0,.18),0 24px 40px -20px rgba(0,0,0,.5)}
.t-green{background:${C.green}}.t-orange{background:${C.orange}}.t-purple{background:${C.purple}}.t-sky{background:${C.sky}}.t-blue{background:${C.blue}}
.dot{width:16px;height:16px;border-radius:50%;background:${C.green};display:inline-block}
.bar-track{height:18px;border-radius:999px;background:${C.paper2};overflow:hidden}
.night .bar-track{background:${C.nline}}
.bar-fill{height:100%;width:100%;border-radius:inherit;transform-origin:0 50%;transform:scaleX(0)}
.num{position:relative;display:inline-block}
.num .v{position:absolute;left:0;top:0}
.num .v0{position:relative}
.img{display:block}
.fade-edges{-webkit-mask-image:radial-gradient(ellipse 52% 52% at 50% 50%,#000 70%,transparent 100%)}
`

// ---- scenes -------------------------------------------------------------------
// Each returns { bg, html, css, js }; ids are prefixed with the section id.

const scenes = {}
const PFX = { hook: 'hk', drop: 'dr', idea: 'id', how: 'hw', run: 'rn', agents: 'ag', survive: 'sv', reach: 're', toolbox: 'tb', cta: 'ct', outro: 'ou' }

scenes.hook = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tBuild = at(s, 'full build')
  const tTest = at(s, 'test suite')
  const tAgents = at(s, 'three more agents')
  const tMelt = at(s, 'melting')
  const cpu = steps('hk-cpu', ['12%', '38%', '54%', '71%', '86%', '97%', '100%'], [0, tBuild, tBuild + 0.6, tTest, tAgents, tAgents + 0.5, tMelt])
  const term = (id, x, y, w, title, lines) =>
    `<div class="term abs h" id="${id}" style="left:${x}px;top:${y}px;width:${w}px"><div class="bar"><i></i><i></i><i></i><span>${title}</span></div><pre>${lines}</pre></div>`
  const html = `
  <div class="grid"></div>
  ${term('hk-t1', 140, 150, 980, '~/app · claude', `<span class="prompt">●</span> <span class="cmd">Bash(pnpm build)</span>\n<span class="dim">  building 1,284 modules…</span>\n<span id="hk-spin" class="ok">  ⠋ transforming</span>`)}
  ${term('hk-t2', 260, 330, 900, '~/app · pnpm test', `<span class="cmd">$ pnpm test</span>\n<span class="dim">  RUN  v5.0.5  ~/app</span>\n<span class="dim">  ⠙ 0/777 tests</span>`)}
  ${['claude -p "fix the flaky test"', 'copilot -p "add pagination"', 'claude -p "migrate to vite 8"']
    .map((c, i) => term(`hk-a${i}`, 380 + i * 90, 470 + i * 70, 760, `agent ${i + 2}`, `<span class="prompt">●</span> <span class="cmd">${esc(c)}</span>\n<span class="dim">  thinking…</span>`))
    .join('')}
  <div class="abs" id="hk-meter" style="left:1300px;top:250px;width:480px">
    <div class="mono" style="font-size:26px;color:#8a8bb0;letter-spacing:.12em">YOUR LAPTOP · CPU</div>
    <div class="disp" id="hk-pct" style="font-size:170px;color:#fff;margin:14px 0 24px">${cpu.html}</div>
    <div class="bar-track" style="height:26px"><div class="bar-fill" id="hk-cpubar" style="background:linear-gradient(90deg,${C.orange},${C.red})"></div></div>
    <div class="mono" style="font-size:24px;color:#8a8bb0;margin-top:22px">fan <span id="hk-fan">${steps('hk-fanv', ['1,800', '3,900', '5,600', '6,800 rpm'], [0, tTest, tAgents, tMelt]).html}</span></div>
  </div>
  <div class="abs h" id="hk-red" style="inset:0;background:radial-gradient(ellipse at 50% 55%,transparent 30%,rgba(255,60,40,.55) 100%)"></div>
  <img class="abs img h fade-edges" id="hk-melt" src="assets/melting.webp" style="left:560px;top:90px;width:800px;height:800px">`
  let js = cpu.js + steps('hk-fanv', ['1,800', '3,900', '5,600', '6,800 rpm'], [0, tTest, tAgents, tMelt]).js
  js += ft('#hk-t1', { opacity: 0, y: 60, scale: 0.94 }, { opacity: 1, y: 0, scale: 1, duration: 0.5, ease: 'power3.out' }, Math.max(0, l0.a - 0.3))
  js += ft('#hk-cpubar', { scaleX: 0.12 }, { scaleX: 0.38, duration: 0.6, ease: 'power2.out' }, tBuild)
  js += ft('#hk-t2', { opacity: 0, y: 80, scale: 0.92 }, { opacity: 1, y: 0, scale: 1, duration: 0.42, ease: 'back.out(1.6)' }, tTest - 0.1)
  js += to('#hk-cpubar', { scaleX: 0.71, duration: 0.5, ease: 'power2.out' }, tTest)
  ;[0, 1, 2].forEach((i) => (js += ft(`#hk-a${i}`, { opacity: 0, y: 90, scale: 0.9 }, { opacity: 1, y: 0, scale: 1, duration: 0.3 - i * 0.06, ease: 'back.out(1.8)' }, tAgents + i * (0.28 - i * 0.05))))
  js += to('#hk-cpubar', { scaleX: 0.97, duration: 0.6, ease: 'power2.out' }, tAgents)
  js += ft('#hk-pct', { color: '#ffffff' }, { color: C.red, duration: 0.4 }, tAgents + 0.5)
  // melting: everything blurs back, the laptop slams in, the frame shakes
  js += to('#hk-t1,#hk-t2,#hk-a0,#hk-a1,#hk-a2', { opacity: 0.25, filter: 'blur(6px)', scale: 0.96, duration: 0.3, ease: 'power2.in' }, tMelt - 0.15)
  js += to('#hk-cpubar', { scaleX: 1, duration: 0.2 }, tMelt)
  js += ft('#hk-red', { opacity: 0 }, { opacity: 1, duration: 0.3 }, tMelt)
  js += slam('#hk-melt', tMelt)
  js += shake('#hk-stage', tMelt + 0.05, 16)
  js += to('#hk-melt', { y: -10, duration: 0.6, repeat: 3, yoyo: true, ease: 'sine.inOut' }, tMelt + 0.4)
  return { bg: 'night', html, js, sfx: [['impact', tMelt], ['whoosh_short', tTest - 0.15], ['pop', tAgents], ['pop', tAgents + 0.28], ['pop', tAgents + 0.5]] }
}

scenes.drop = (s) => {
  const l1 = L(s, 1)
  const tLongarm = at(s, 'longarm', { line: 0 })
  const words = 'Give your coding agents a long arm.'.split(' ')
  const wt = words.map((w, i) => (i < 5 ? at(s, w, { line: 1 }) : at(s, i === 5 ? 'long' : 'arm', { line: 1 })))
  const musicEnd = dur(s)
  const tHero = Math.max(l1.b + 0.25, onBeat(s, l1.b))
  const html = `
  <div class="abs h" id="dr-flash" style="inset:0;background:#fff"></div>
  <div class="abs" id="dr-brand" style="left:0;right:0;top:240px;display:flex;flex-direction:column;align-items:center">
    <div style="position:relative;width:300px;height:300px">
      <div class="abs h" id="dr-ring" style="inset:-30px;border-radius:50%;border:6px solid ${C.blue2}"></div>
      <img class="abs img h" id="dr-icon" src="assets/icon.png" style="inset:0;width:300px;height:300px">
    </div>
    <div class="disp" id="dr-word" style="font-size:170px;margin-top:20px;color:${C.ink}">${[...'Longarm'].map((c) => `<span class="lt h" style="display:inline-block">${c}</span>`).join('')}</div>
  </div>
  <div class="abs disp" id="dr-tag" style="left:0;right:0;top:880px;text-align:center;font-size:76px;color:${C.ink}">
    ${words
      .slice(0, 5)
      .map((w, i) => `<span class="tw h" id="dr-w${i}" style="display:inline-block;margin:0 .14em">${w}</span>`)
      .join('')}<span style="position:relative;display:inline-block">${words
      .slice(5)
      .map((w, i) => `<span class="tw h" id="dr-w${i + 5}" style="display:inline-block;margin:0 .14em;color:${C.blue}">${w.replace('.', '')}</span>`)
      .join('')}<svg class="abs" viewBox="0 0 300 40" preserveAspectRatio="none" style="left:.1em;right:.1em;bottom:-.2em;width:calc(100% - .2em);height:.4em;overflow:visible"><path id="dr-under" d="M4 26 C 60 6, 120 38, 180 20 S 270 8, 296 22" fill="none" stroke="${C.orange}" stroke-width="8" stroke-linecap="round" stroke-dasharray="340" stroke-dashoffset="340"/></svg></span><span class="h" id="dr-dotp">.</span>
  </div>
  <img class="abs img h fade-edges" id="dr-hero" src="assets/hero.webp" style="left:300px;top:110px;width:1320px;height:880px">
  ${['pnpm test → devbox-01', 'cargo build → tower-32c', 'claude -p → cloud-vm', 'go test → old-thinkpad']
    .map((c, i) => `<div class="chip abs h mono" id="dr-c${i}" style="font-size:22px;left:${[160, 1380, 230, 1320][i]}px;top:${[250, 300, 760, 820][i]}px;border-left:8px solid ${[C.green, C.orange, C.purple, C.sky][i]}">${esc(c)}</div>`)
    .join('')}`
  let js = ft('#dr-flash', { opacity: 1 }, { opacity: 0, duration: 0.5, ease: 'power2.out' }, 0)
  js += ft('#dr-icon', { opacity: 0, scale: 0.2, rotation: -25 }, { opacity: 1, scale: 1, rotation: 0, duration: 0.7, ease: 'back.out(1.8)' }, 0.02)
  js += ft('#dr-ring', { opacity: 0.9, scale: 0.6 }, { opacity: 0, scale: 1.5, duration: 0.9, ease: 'power2.out' }, 0.1)
  js += `tl.fromTo('#dr-word .lt',{opacity:0,y:-80,scale:1.4},{opacity:1,y:0,scale:1,duration:0.32,ease:'back.out(2.2)',stagger:0.055},${r3(tLongarm - 0.12)});\n`
  // tagline: the brand moves up, each word lands as it is spoken
  js += to('#dr-brand', { y: -120, scale: 0.78, duration: 0.6, ease: 'power3.inOut' }, l1.a - 0.5)
  js += to('#dr-tag', { y: -170, duration: 0.6, ease: 'power3.inOut' }, l1.a - 0.5)
  wt.forEach((t, i) => (js += ft(`#dr-w${i}`, { opacity: 0, y: 40, scale: 1.25 }, { opacity: 1, y: 0, scale: 1, duration: 0.28, ease: 'back.out(2)' }, t - 0.04)))
  js += ft('#dr-dotp', { opacity: 0 }, { opacity: 1, duration: 0.1 }, wt[6] + 0.2)
  js += to('#dr-under', { strokeDashoffset: 0, duration: 0.6, ease: 'power2.out' }, wt[6] + 0.1)
  // music-only bars: the robot reaches out, jobs pop on the beat
  js += to('#dr-brand,#dr-tag', { opacity: 0, y: '-=60', filter: 'blur(12px)', duration: 0.35, ease: 'power2.in' }, tHero - 0.3)
  js += ft('#dr-hero', { opacity: 0, y: 140, scale: 0.9 }, { opacity: 1, y: 0, scale: 1, duration: 0.8, ease: 'power3.out' }, tHero - 0.1)
  js += to('#dr-hero', { y: -14, duration: 1.6, repeat: 3, yoyo: true, ease: 'sine.inOut' }, tHero + 0.7)
  const sfx = [['whoosh_long', -0.25], ['pop', tLongarm]]
  for (let i = 0; i < 4; i++) {
    const t = onBeat(s, tHero + 0.6 + i * BEAT * 2)
    if (t < musicEnd - 0.4) {
      js += pop(`#dr-c${i}`, t, 0.45)
      sfx.push(['pop', t])
    }
  }
  return { bg: 'paper', html, js, sfx }
}

scenes.idea = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tGreat = at(s, 'great')
  const tTerrible = at(s, 'terrible')
  const m = [at(s, 'thirty'), at(s, 'old laptop'), at(s, 'cloud vm')]
  const tNothing = at(s, 'doing nothing')
  const tPool = at(s, 'single pool')
  const machines = [
    ['tower-32c', '32 cores · 128 GB', '🖥'],
    ['old-thinkpad', '8 cores · 16 GB', '💻'],
    ['cloud-vm', '16 vCPU · 64 GB', '☁'],
  ]
  const html = `
  <div class="abs" style="left:150px;top:150px">
    <div class="disp h" id="id-l1" style="font-size:96px">Great at <span style="color:${C.green}">starting</span> work.</div>
    <div class="disp h" id="id-l2" style="font-size:96px;margin-top:20px">Terrible at <span style="color:${C.red}">waiting</span>.</div>
  </div>
  <div class="abs h" id="id-hour" style="left:1460px;top:150px;font-size:180px">⏳</div>
  ${machines
    .map(
      ([n, spec, ic], i) => `<div class="card abs h" id="id-m${i}" style="left:${170 + i * 540}px;top:470px;width:480px;height:330px;padding:36px">
      <div style="font-size:90px;line-height:1">${ic}</div>
      <div class="mono" style="font-size:34px;font-weight:700;margin-top:24px">${n}</div>
      <div style="font-size:26px;color:${C.ink2};margin-top:8px">${spec}</div>
      <div class="mono" id="id-s${i}" style="position:absolute;right:30px;top:36px;font-size:24px;color:${C.ink3};padding:8px 14px;border-radius:10px;background:${C.paper}">idle · 0%</div>
      <div class="abs h" id="id-on${i}" style="right:30px;top:36px;font:700 24px/1 'JetBrains Mono',monospace;color:#129952;padding:8px 14px;border-radius:10px;background:#e9fbf1">● worker</div>
    </div>`,
    )
    .join('')}
  <div class="abs h" id="id-pool" style="left:130px;top:430px;width:1660px;height:420px;border-radius:44px;border:5px dashed ${C.blue};background:rgba(60,70,242,.05)"></div>
  <div class="abs h chip" id="id-pooltag" style="left:760px;top:880px;font-size:30px;border-color:${C.blue};color:${C.blue}">one pool · 56 cores · 208 GB</div>`
  let js = blurIn('#id-l1', tGreat - 0.1)
  js += blurIn('#id-l2', tTerrible - 0.1)
  js += ft('#id-hour', { opacity: 0, rotation: -30, scale: 0.5 }, { opacity: 1, rotation: 0, scale: 1, duration: 0.5, ease: 'back.out(2)' }, tTerrible + 0.2)
  js += to('#id-hour', { rotation: 180, duration: 0.6, ease: 'power2.inOut' }, tTerrible + 1.0)
  js += to('#id-l1,#id-l2,#id-hour', { opacity: 0.25, y: -40, duration: 0.5, ease: 'power2.inOut' }, l1.a - 0.3)
  m.forEach((t, i) => (js += ft(`#id-m${i}`, { opacity: 0, y: 120, rotation: (i - 1) * 6 }, { opacity: 1, y: 0, rotation: (i - 1) * 2, duration: 0.55, ease: 'back.out(1.7)' }, t - 0.05)))
  js += [0, 1, 2].map((i) => to(`#id-m${i}`, { rotation: (i - 1) * -3, y: 12, duration: 0.25, yoyo: true, repeat: 1, ease: 'sine.inOut' }, tNothing + i * 0.08)).join('')
  // one pool: the cards straighten and close ranks, the dashed pool draws round them
  js += [0, 1, 2].map((i) => to(`#id-m${i}`, { rotation: 0, x: (1 - i) * 30, duration: 0.6, ease: 'power3.inOut' }, tPool - 0.2)).join('')
  js += ft('#id-pool', { opacity: 0, scale: 1.08 }, { opacity: 1, scale: 1, duration: 0.5, ease: 'power3.out' }, tPool)
  js += [0, 1, 2].map((i) => set(`#id-s${i}`, { opacity: 0 }, tPool + 0.3 + i * 0.12) + pop(`#id-on${i}`, tPool + 0.3 + i * 0.12, 0.35)).join('')
  js += pop('#id-pooltag', tPool + 0.8)
  js += to('#id-l1,#id-l2,#id-hour', { opacity: 0, duration: 0.3 }, tPool)
  return { bg: 'paper', html, js, sfx: [['whoosh_short', m[0] - 0.1], ['pop', m[1]], ['pop', m[2]], ['ding', tPool + 0.8]] }
}

scenes.how = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tEvery = at(s, 'every machine')
  const tCode = at(s, 'single code')
  const tWorker = at(s, 'worker')
  const tAnything = at(s, 'anything')
  const workers = [
    ['tower-32c', 260],
    ['devbox-01', 450],
    ['old-thinkpad', 640],
    ['cloud-vm', 830],
  ]
  const html = `
  <div class="abs" style="left:120px;top:90px"><span class="kicker h" id="hw-k">How it works</span></div>
  <div class="card abs h" id="hw-msi" style="left:150px;top:200px;width:760px;padding:0;overflow:hidden">
    <div style="display:flex;align-items:center;gap:14px;padding:18px 24px;background:${C.paper};font:700 26px/1 'Onest'"><img src="assets/icon.png" style="width:34px;height:34px;border-radius:8px"> Longarm Setup</div>
    <div style="padding:30px 34px;display:grid;gap:20px;font-size:28px">
      <div style="display:flex;align-items:center;gap:16px;font-weight:700"><i style="width:26px;height:26px;border-radius:50%;border:8px solid ${C.blue}"></i> Default (recommended)</div>
      <div style="display:flex;gap:12px"><span class="chip" style="background:#e9fbf1;color:#129952;border:0">✓ Send jobs to workers</span><span class="chip h" id="hw-role2" style="background:#e9fbf1;color:#129952;border:0">✓ Run jobs here</span></div>
      <div class="bar-track"><div class="bar-fill" id="hw-prog" style="background:${C.blue}"></div></div>
      <div style="display:flex;justify-content:space-between;align-items:center;color:${C.ink3};font-size:24px"><span id="hw-st">${steps('hw-stv', ['Ready to install', 'Installing…', 'Done - signing in'], [0, tEvery, tEvery + 1.4]).html}</span><span class="chip" style="background:${C.blue};color:#fff;border:0">Install</span></div>
    </div>
  </div>
  <div class="abs" style="left:150px;top:720px;display:flex;gap:14px">${['Windows', 'macOS', 'Linux', 'Dev Box'].map((o, i) => `<span class="chip h" id="hw-os${i}">${o}</span>`).join('')}</div>
  <div class="card abs h" id="hw-code" style="left:1010px;top:200px;width:760px;padding:36px">
    <div class="mono" style="font-size:24px;color:${C.ink3}">tray · sign-in code</div>
    <div class="mono" style="font-size:84px;font-weight:800;letter-spacing:.12em;margin:14px 0 26px">BCDF-GHJK</div>
    <div class="mono" style="font-size:24px;color:${C.ink3}">browser · ${HUB_LINK}</div>
    <div style="display:flex;gap:16px;align-items:center;margin-top:14px">
      <div class="mono" style="flex:1;padding:16px 20px;border-radius:14px;border:3px solid ${C.blue};font-size:36px;letter-spacing:.1em" id="hw-in">${chars('BCDF-GHJK')}</div>
      <div class="chip" id="hw-approve" style="background:${C.blue};color:#fff;border:0;font-size:28px">Approve</div>
    </div>
    <div class="h" id="hw-ok" style="margin-top:22px;font:700 30px/1.2 'Onest';color:#129952">✓ Approved · device and worker</div>
  </div>
  <svg class="abs h" id="hw-net" viewBox="0 0 ${W} ${H}" style="left:0;top:0;width:${W}px;height:${H}px">
    ${workers.map(([, y], i) => `<path id="hw-p${i}" d="M 640 560 C 900 560, 1000 ${y + 45}, 1330 ${y + 45}" fill="none" stroke="#fff" stroke-width="26" stroke-linecap="round" stroke-dasharray="900" stroke-dashoffset="900" style="filter:drop-shadow(0 8px 10px rgba(60,70,242,.35))"/>`).join('')}
  </svg>
  <img class="abs img h" id="hw-bot" src="assets/icon.png" style="left:380px;top:420px;width:280px;height:280px">
  ${workers.map(([n, y], i) => `<div class="card abs h" id="hw-w${i}" style="left:1330px;top:${y}px;width:420px;height:90px;padding:0 26px;display:flex;align-items:center;gap:16px;font:700 30px/1 'JetBrains Mono'"><span class="dot"></span>${n}<span class="tile ${['t-green', 't-purple', 't-orange', 't-sky'][i]} abs h" id="hw-tl${i}" style="width:64px;height:64px;font-size:28px;border-radius:18px;right:14px;top:13px">${['>_', '✓', '⚙', '☁'][i]}</span></div>`).join('')}`
  let js = pop('#hw-k', l0.a)
  js += ft('#hw-msi', { opacity: 0, y: 80, rotation: -2 }, { opacity: 1, y: 0, rotation: 0, duration: 0.6, ease: 'back.out(1.5)' }, l0.a - 0.1)
  js += steps('hw-stv', ['Ready to install', 'Installing…', 'Done - signing in'], [0, tEvery, tEvery + 1.4]).js
  js += pop('#hw-role2', tEvery - 0.3, 0.35)
  js += ft('#hw-prog', { scaleX: 0 }, { scaleX: 1, duration: 1.4, ease: 'power1.inOut' }, tEvery)
  ;[0, 1, 2, 3].forEach((i) => (js += pop(`#hw-os${i}`, tEvery + 0.15 + i * 0.14, 0.4)))
  js += ft('#hw-code', { opacity: 0, y: 80, rotation: 2 }, { opacity: 1, y: 0, rotation: 0, duration: 0.6, ease: 'back.out(1.5)' }, l1.a - 0.2)
  js += `tl.fromTo('#hw-in .ch',{opacity:0},{opacity:1,duration:0.01,stagger:0.07,ease:'none'},${r3(tCode - 0.2)});\n`
  js += ft('#hw-approve', { scale: 1 }, { scale: 0.9, duration: 0.08, yoyo: true, repeat: 1 }, tCode + 0.6)
  js += set('#hw-approve', { background: C.green }, tCode + 0.7)
  js += pop('#hw-ok', tCode + 0.75, 0.4)
  // every machine a worker: the setup cards leave, the robot's arms reach out
  js += to('#hw-msi,#hw-code,[id^=hw-os],#hw-k', { opacity: 0, y: -40, filter: 'blur(10px)', duration: 0.35, ease: 'power2.in' }, l2.a - 0.35)
  js += pop('#hw-bot', l2.a - 0.05, 0.6)
  js += set('#hw-net', { opacity: 1 }, l2.a)
  workers.forEach((_, i) => {
    js += pop(`#hw-w${i}`, tWorker - 0.25 + i * 0.12, 0.4)
    js += to(`#hw-p${i}`, { strokeDashoffset: 0, duration: 0.55, ease: 'power3.out' }, tWorker - 0.3 + i * 0.12)
    js += pop(`#hw-tl${i}`, tAnything + i * 0.16, 0.4)
  })
  return { bg: 'paper', html, js, sfx: [['ding', tCode + 0.75], ['whoosh_short', l2.a - 0.3], ['pop', tAnything], ['pop', tAnything + 0.32]] }
}

scenes.run = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tType = at(s, 'longarm run')
  const tPicks = at(s, 'picks')
  const tWorktree = at(s, 'worktree')
  const tStreams = at(s, 'streams')
  const tQuiet = at(s, 'quiet')
  const cmd = 'longarm run --repo . -- pnpm test'
  const rows = [
    ['devbox-01', 0.42, '9 GB free · 71% CPU'],
    ['tower-32c', 0.82, '31 GB free · 4% CPU'],
    ['old-thinkpad', 0.2, '3 GB free · 12% CPU'],
    ['cloud-vm', 0.55, '14 GB free · 38% CPU'],
  ]
  const outLines = [
    ['dim', 'placed on tower-32c · worktree of 3f9c2e1 + your changes'],
    ['ok', ' ✓ packages/core   412 tests   6.1s'],
    ['ok', ' ✓ packages/web    268 tests   9.4s'],
    ['ok', ' ✓ packages/cli     97 tests   3.2s'],
    ['win', 'exit 0 · 18.7s on tower-32c'],
  ]
  const html = `
  <div class="grid"></div>
  <div class="term abs" id="rn-term" style="left:110px;top:120px;width:1020px;height:720px"><div class="bar"><i></i><i></i><i></i><span>~/app</span></div>
    <pre><span class="cmd">$ </span><span class="cmd" id="rn-cmd">${chars(cmd)}</span>${outLines.map(([c, t], i) => `<span class="${c} h" id="rn-o${i}" style="display:block">${esc(t)}</span>`).join('')}</pre>
  </div>
  <div class="card abs h" id="rn-sched" style="left:1190px;top:120px;width:620px;padding:30px 34px">
    <div class="mono" style="font-size:22px;color:#8a8bb0;letter-spacing:.12em">SCHEDULER · FREE MEMORY</div>
    ${rows.map(([n, f, d], i) => `<div id="rn-r${i}" style="margin-top:24px;padding:14px 16px;border-radius:16px;border:3px solid transparent">
      <div style="display:flex;justify-content:space-between;font:700 26px/1 'JetBrains Mono';color:#fff">${n}<span style="font-weight:500;font-size:20px;color:#8a8bb0">${d}</span></div>
      <div class="bar-track" style="margin-top:12px;height:14px"><div class="bar-fill" id="rn-b${i}" style="background:${C.green};transform:scaleX(${f})"></div></div></div>`).join('')}
  </div>
  <div class="chip abs h mono" id="rn-wt" style="left:1190px;top:770px;font-size:24px;background:${C.night2};color:#7cf3b0;border-color:${C.nline}">⎇ fresh worktree · uncommitted changes included</div>
  <img class="abs img h fade-edges" id="rn-cool" src="assets/cool.webp" style="left:1220px;top:330px;width:620px;height:620px">
  <div class="chip abs h" id="rn-cpu" style="left:1330px;top:930px;font-size:30px">your laptop: 6% CPU · fans off</div>`
  let js = ft('#rn-term', { opacity: 0, y: 60 }, { opacity: 1, y: 0, duration: 0.5, ease: 'power3.out' }, Math.max(0, l0.a - 0.4))
  js += `tl.fromTo('#rn-cmd .ch',{opacity:0},{opacity:1,duration:0.01,stagger:0.045,ease:'none'},${r3(tType - 0.05)});\n`
  js += ft('#rn-sched', { opacity: 0, x: 80 }, { opacity: 1, x: 0, duration: 0.5, ease: 'power3.out' }, l1.a - 0.2)
  js += ft('#rn-r1', { borderColor: 'rgba(31,196,106,0)', backgroundColor: 'rgba(31,196,106,0)' }, { borderColor: C.green, backgroundColor: 'rgba(31,196,106,.12)', duration: 0.3 }, tPicks + 0.3)
  js += ft('#rn-r1', { scale: 1 }, { scale: 1.04, duration: 0.15, yoyo: true, repeat: 1 }, tPicks + 0.3)
  js += blurIn('#rn-o0', tPicks + 0.5, 0.3)
  js += pop('#rn-wt', tWorktree - 0.1, 0.4)
  ;[1, 2, 3, 4].forEach((i) => (js += ft(`#rn-o${i}`, { opacity: 0, x: -20 }, { opacity: 1, x: 0, duration: 0.2 }, tStreams + 0.1 + (i - 1) * 0.32)))
  js += to('#rn-sched,#rn-wt', { opacity: 0, x: 60, duration: 0.3, ease: 'power2.in' }, l2.a - 0.35)
  js += ft('#rn-cool', { opacity: 0, scale: 0.5, rotation: 8 }, { opacity: 1, scale: 1, rotation: 0, duration: 0.6, ease: 'back.out(1.8)' }, l2.a - 0.1)
  js += pop('#rn-cpu', tQuiet, 0.4)
  return { bg: 'night', html, js, sfx: [['type', tType], ['pop', tPicks + 0.3], ['ding', tStreams + 1.4], ['pop', l2.a]] }
}

scenes.agents = (s) => {
  const ls = [0, 1, 2, 3, 4].map((i) => L(s, i))
  const names = ['Claude Code', 'Copilot CLI', 'Codex', 'Gemini']
  const tn = [at(s, 'claude code'), at(s, 'copilot'), at(s, 'codex'), at(s, 'gemini')]
  const tSay = at(s, 'run the tests')
  const tFive = at(s, 'five')
  const tSteer = at(s, 'steer')
  const tHand = at(s, 'hand off')
  const tUncommitted = at(s, 'uncommitted')
  const tAnother = at(s, 'another machine')
  const fan = ['tower-32c', 'devbox-01', 'devbox-02', 'cloud-vm', 'old-thinkpad']
  const html = `
  <div class="grid"></div>
  <div class="abs disp h" id="ag-title" style="left:0;right:0;top:400px;text-align:center;font-size:150px;color:#fff">Built for <span style="color:#b3abff">agents</span>.</div>
  <div class="abs h" id="ag-cmd" style="left:0;right:0;top:170px;text-align:center"><span class="chip mono" style="font-size:40px;background:#0b0c26;color:#7cf3b0;border-color:${C.nline};padding:22px 34px">$ longarm skill install</span></div>
  <div class="abs" style="left:150px;top:380px;width:1620px;display:flex;gap:30px">
    ${names.map((n, i) => `<div class="card h" id="ag-n${i}" style="flex:1;padding:34px;text-align:center"><div style="font-size:64px">${['✳', '◐', '◎', '✦'][i]}</div><div style="font:700 36px/1.2 'Bricolage';color:#fff;margin-top:16px">${n}</div><div class="h" id="ag-ck${i}" style="font:700 26px/1 'JetBrains Mono';color:#5cf09a;margin-top:16px">✓ skill installed</div></div>`).join('')}
  </div>
  <div class="abs h" id="ag-chat" style="left:150px;top:150px;width:900px">
    <div style="display:flex;justify-content:flex-end"><div style="background:${C.blue};color:#fff;font-size:40px;padding:26px 34px;border-radius:30px 30px 8px 30px" id="ag-say">${chars('Run the tests on a worker.')}</div></div>
    <div class="h mono" id="ag-tool" style="margin-top:26px;border:2px dashed #3a3b78;border-radius:20px;padding:20px 24px;font-size:25px;color:#7cf3b0"><span style="background:${C.nline};color:#d8d9ff;padding:3px 10px;border-radius:8px;font-size:20px">Bash</span> longarm run --repo . -- pnpm test</div>
  </div>
  <div class="abs" id="ag-fan" style="left:1110px;top:150px;width:680px">
    ${fan.map((w, i) => `<div class="card h" id="ag-f${i}" style="padding:20px 24px;margin-bottom:16px"><div style="display:flex;justify-content:space-between;font:700 26px/1 'JetBrains Mono';color:#fff">${w}<span style="color:#a9aad6;font-weight:500;font-size:22px">sub-agent ${i + 1}</span></div><div class="bar-track" style="height:12px;margin-top:14px"><div class="bar-fill" id="ag-fb${i}" style="background:linear-gradient(90deg,${C.purple},${C.sky},${C.green})"></div></div></div>`).join('')}
  </div>
  <div class="term abs h" id="ag-steer" style="left:150px;top:180px;width:820px"><div class="bar"><i></i><i></i><i></i><span>longarm attach devbox-01 --job 01KQ</span></div>
    <pre><span class="prompt">●</span> <span class="cmd">Fixing packages/web…</span>\n<span class="dim">  edited src/router.ts (+12 −3)</span>\n<span class="cmd">&gt; </span><span class="cmd" id="ag-st">${chars('also add a test for it')}</span></pre></div>
  <div class="card abs h" id="ag-conv" style="left:1100px;top:250px;width:560px;padding:30px">
    <div style="font:700 30px/1.2 'Bricolage';color:#fff">✳ conversation · 214 turns</div>
    <div class="mono" style="margin-top:18px;font-size:23px;line-height:1.7"><span class="h" id="ag-f0x" style="display:block;color:#ffd166">M src/router.ts</span><span class="h" id="ag-f1x" style="display:block;color:#ffd166">M src/app.tsx</span><span class="h" id="ag-f2x" style="display:block;color:#7cf3b0">? test/router.test.ts</span></div>
  </div>
  <div class="abs h chip mono" id="ag-from" style="left:1100px;top:780px;font-size:26px;background:${C.night2};color:#fff;border-color:${C.nline}">laptop</div>
  <div class="abs h chip mono" id="ag-to" style="left:1480px;top:780px;font-size:26px;background:#e9fbf1;color:#129952;border:0">→ devbox-01</div>`
  let js = slam('#ag-title', ls[0].a)
  js += to('#ag-title', { opacity: 0, scale: 0.9, filter: 'blur(12px)', duration: 0.3, ease: 'power2.in' }, ls[1].a - 0.3)
  js += pop('#ag-cmd', ls[1].a - 0.1)
  tn.forEach((t, i) => {
    js += ft(`#ag-n${i}`, { opacity: 0, y: 120, rotation: (i - 1.5) * 5 }, { opacity: 1, y: 0, rotation: 0, duration: 0.5, ease: 'back.out(1.8)' }, t - 0.08)
    js += pop(`#ag-ck${i}`, t + 0.35, 0.3)
  })
  js += to('#ag-cmd,[id^=ag-n]', { opacity: 0, y: -50, filter: 'blur(10px)', duration: 0.35, ease: 'power2.in' }, ls[2].a - 0.35)
  js += set('#ag-chat', { opacity: 1 }, tSay - 0.5)
  js += pop('#ag-say', tSay - 0.5, 0.35)
  js += `tl.fromTo('#ag-say .ch',{opacity:0},{opacity:1,duration:0.01,stagger:0.035,ease:'none'},${r3(tSay - 0.4)});\n`
  js += blurIn('#ag-tool', ls[2].b - 0.2, 0.3)
  fan.forEach((_, i) => {
    const t = tFive + i * 0.16
    js += ft(`#ag-f${i}`, { opacity: 0, x: 120 }, { opacity: 1, x: 0, duration: 0.4, ease: 'back.out(1.6)' }, t)
    js += ft(`#ag-fb${i}`, { scaleX: 0.02 }, { scaleX: 1, duration: 2.2 + i * 0.4, ease: 'power1.inOut' }, t + 0.3)
  })
  js += to('#ag-chat,#ag-fan', { opacity: 0, y: -40, filter: 'blur(10px)', duration: 0.35, ease: 'power2.in' }, ls[4].a - 0.35)
  js += ft('#ag-steer', { opacity: 0, x: -80 }, { opacity: 1, x: 0, duration: 0.5, ease: 'power3.out' }, tSteer - 0.2)
  js += `tl.fromTo('#ag-st .ch',{opacity:0},{opacity:1,duration:0.01,stagger:0.05,ease:'none'},${r3(tSteer + 0.4)});\n`
  js += ft('#ag-conv', { opacity: 0, x: 80 }, { opacity: 1, x: 0, duration: 0.5, ease: 'power3.out' }, tHand - 0.2)
  js += pop('#ag-from', tHand, 0.35)
  ;[0, 1, 2].forEach((i) => (js += blurIn(`#ag-f${i}x`, tUncommitted + i * 0.15, 0.25)))
  js += to('#ag-conv', { x: 120, y: -30, rotation: 3, duration: 0.7, ease: 'power3.inOut' }, tAnother - 0.2)
  js += pop('#ag-to', tAnother, 0.4)
  return {
    bg: 'night',
    html,
    js,
    sfx: [['impact_soft', ls[0].a], ...tn.map((t) => ['pop', t]), ['type', tSay - 0.4], ...fan.map((_, i) => ['pop', tFive + i * 0.16]), ['whoosh_short', tAnother - 0.2]],
  }
}

scenes.survive = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tLid = at(s, 'close the lid')
  const tNet = at(s, 'switch networks')
  const tDaemon = at(s, 'restart the daemon')
  const tKeeps = at(s, 'keeps running')
  const tAttach = at(s, 're-attach')
  const ev = [
    ['💤', 'Lid closed', tLid],
    ['📶', 'Wi-Fi → hotel network', tNet],
    ['↻', 'longarmd restarted', tDaemon],
  ]
  const html = `
  <div class="abs disp h" id="sv-title" style="left:0;right:0;top:330px;text-align:center;font-size:160px">Jobs run <span style="color:${C.blue}">detached</span>.</div>
  <img class="abs img h fade-edges" id="sv-img" src="assets/sleep.webp" style="left:40px;top:180px;width:1000px;height:667px">
  <div class="abs" style="left:1080px;top:200px;width:720px">
    ${ev.map(([ic, t], i) => `<div class="card h" id="sv-e${i}" style="display:flex;align-items:center;gap:22px;padding:24px 30px;margin-bottom:20px;font:600 36px/1 'Onest'"><span style="font-size:44px;width:56px;text-align:center">${ic}</span>${t}<span style="margin-left:auto;font:700 22px/1 'JetBrains Mono';color:#129952">job ✓</span></div>`).join('')}
  </div>
  <div class="abs h" id="sv-job" style="left:1080px;top:640px;width:720px;padding:26px 30px;border-radius:24px;background:${C.night};color:#d8d9ff">
    <div style="display:flex;justify-content:space-between;font:500 24px/1 'JetBrains Mono'">job 01KQ7Z3M · cargo build<b style="color:#5cf09a">running</b></div>
    <div class="bar-track" style="background:${C.nline};margin-top:18px;height:14px"><div class="bar-fill" id="sv-jb" style="background:${C.green}"></div></div>
  </div>
  <div class="term abs h" id="sv-term" style="left:200px;top:160px;width:1520px"><div class="bar"><i></i><i></i><i></i><span>~ · longarm attach devbox-01 --job 01KQ7Z3M</span></div>
    <pre><span class="dim" id="sv-rp">${Array.from({ length: 7 }, (_, i) => `<span class="h" id="sv-r${i}" style="display:block">   Compiling crate-${['tokio', 'serde', 'hyper', 'axum', 'tonic', 'prost', 'rustls'][i]} v1.${i}.0</span>`).join('')}</span><span class="win h" id="sv-live" style="display:block">── replayed 1,204 lines · live ──</span><span class="ok h" id="sv-last" style="display:block">    Finished release [optimized] target(s) in 4m 12s</span></pre></div>`
  let js = slam('#sv-title', l0.a - 0.05)
  js += ft('#sv-title', { scale: 1 }, { scale: 1.04, duration: 0.12, yoyo: true, repeat: 1, immediateRender: false }, l0.a + 0.32)
  js += to('#sv-title', { opacity: 0, y: -60, filter: 'blur(12px)', duration: 0.3, ease: 'power2.in' }, l1.a - 0.3)
  js += ft('#sv-img', { opacity: 0, x: -80 }, { opacity: 1, x: 0, duration: 0.6, ease: 'power3.out' }, l1.a - 0.25)
  js += blurIn('#sv-job', l1.a - 0.1)
  js += ft('#sv-jb', { scaleX: 0.08 }, { scaleX: 0.92, duration: r3(tKeeps + 0.8 - l1.a), ease: 'none' }, l1.a)
  ev.forEach(([, , t], i) => {
    js += ft(`#sv-e${i}`, { opacity: 0, x: 90, rotation: 2 }, { opacity: 1, x: 0, rotation: 0, duration: 0.45, ease: 'back.out(1.8)' }, t - 0.05)
    js += ft('#sv-job', { boxShadow: '0 0 0 0 rgba(255,138,31,.9)' }, { boxShadow: '0 0 0 16px rgba(255,138,31,0)', duration: 0.5, immediateRender: false }, t)
  })
  js += to('#sv-img,[id^=sv-e],#sv-job', { opacity: 0, y: -40, filter: 'blur(10px)', duration: 0.35, ease: 'power2.in' }, l2.a + 0.6)
  js += ft('#sv-term', { opacity: 0, y: 70 }, { opacity: 1, y: 0, duration: 0.5, ease: 'power3.out' }, l2.a + 0.75)
  js += `tl.fromTo('[id^=sv-r]',{opacity:0},{opacity:1,duration:0.01,stagger:0.07},${r3(tAttach + 0.2)});\n`
  js += pop('#sv-live', tAttach + 0.8, 0.35)
  js += blurIn('#sv-last', tAttach + 1.4, 0.3)
  return { bg: 'paper', html, js, sfx: [['impact', l0.a], ['click', tLid], ['click', tNet], ['click', tDaemon], ['ding', tAttach + 1.4]] }
}

scenes.reach = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const tNat = at(s, 'nat')
  const tTcp = at(s, 'tcp')
  const tTs = at(s, 'tailscale')
  const tQuic = at(s, 'quic')
  const tPunch = at(s, 'hole punching')
  const tRelay = at(s, 'relay')
  const tTls = at(s, 'mutual tls')
  const tHub = at(s, 'hub only')
  const tEnc = at(s, 'encrypted')
  const lane = (i, name, color) => `<div class="abs h" id="re-l${i}" style="left:420px;top:${360 + i * 130}px;width:1080px;height:84px;border-radius:999px;background:${C.night2};border:2px solid ${C.nline}">
      <span class="mono abs" style="left:34px;top:26px;font-size:28px;color:#a9aad6">${name}</span>
      <i class="abs" id="re-r${i}" style="left:6px;top:8px;width:66px;height:66px;border-radius:50%;background:${color};box-shadow:0 0 30px ${color}"></i></div>`
  const html = `
  <div class="grid"></div>
  <div class="abs card" id="re-you" style="left:110px;top:440px;width:250px;padding:26px;text-align:center;font:700 30px/1.3 'JetBrains Mono';color:#fff">💻<br>your laptop</div>
  <div class="abs card" id="re-them" style="left:1560px;top:440px;width:250px;padding:26px;text-align:center;font:700 30px/1.3 'JetBrains Mono';color:#fff">🖥<br>worker</div>
  <div class="abs h" id="re-nat" style="left:1500px;top:250px;width:40px;height:620px;border-radius:12px;background:repeating-linear-gradient(45deg,#ff6b6b 0 14px,#ffb3b3 14px 28px)"></div>
  <div class="abs h mono" id="re-natl" style="left:1430px;top:200px;font-size:26px;font-weight:800;color:${C.red}">NAT · home router</div>
  ${lane(0, 'TCP', '#8a8bb0')}${lane(1, 'Tailscale', C.sky)}${lane(2, 'longnet · QUIC + hole punching', C.green)}
  <div class="abs h chip mono" id="re-win" style="left:1100px;top:640px;font-size:26px;background:${C.green};color:#fff;border:0">mTLS ✓ first</div>
  <div class="abs h" id="re-relay" style="left:420px;top:780px;width:1080px;text-align:center;font:600 30px/1 'Onest';color:#a9aad6;border-top:4px dashed #3a3b78;padding-top:22px">relay (UDP or TLS over TCP) when nothing else gets through</div>
  <div class="abs h card" id="re-hub" style="left:810px;top:110px;width:300px;padding:22px;text-align:center;font:700 30px/1.3 'JetBrains Mono';color:#fff">hub<br><span style="font-size:22px;color:#a9aad6;font-weight:500">coordinates only</span></div>
  <svg class="abs h" id="re-ctl" viewBox="0 0 ${W} ${H}" style="left:0;top:0;width:${W}px;height:${H}px"><path d="M 900 230 L 300 440 M 1020 230 L 1660 440" stroke="#6b6ca8" stroke-width="5" stroke-dasharray="10 14" fill="none"/></svg>
  <div class="abs h" id="re-lock" style="left:880px;top:520px;font-size:110px">🔒</div>
  <div class="abs h chip" id="re-mtls" style="left:770px;top:660px;font-size:32px;font-weight:700">mutual TLS · end to end</div>`
  let js = ft('#re-you,#re-them', { opacity: 0, scale: 0.8 }, { opacity: 1, scale: 1, duration: 0.5, ease: 'back.out(1.8)' }, Math.max(0, l0.a - 0.2))
  js += ft('#re-nat', { opacity: 0, scaleY: 0 }, { opacity: 1, scaleY: 1, duration: 0.45, ease: 'back.out(1.6)' }, tNat - 0.2)
  js += blurIn('#re-natl', tNat)
  ;[tTcp, tTs, tQuic].forEach((t, i) => (js += ft(`#re-l${i}`, { opacity: 0, x: -60 }, { opacity: 1, x: 0, duration: 0.4, ease: 'power3.out' }, t - 0.15)))
  // the race: TCP stops at the NAT wall, Tailscale arrives, longnet wins at "hole punching"
  const go = tPunch - 1.1
  js += to('#re-r0', { x: 994, duration: 1.0, ease: 'power2.in' }, go)
  js += to('#re-r0', { x: 974, duration: 0.2, ease: 'power2.out' }, go + 1.0)
  js += to('#re-r1', { x: 1002, duration: 1.7, ease: 'power2.inOut' }, go)
  js += to('#re-r2', { x: 1002, duration: 1.1, ease: 'expo.inOut' }, go)
  js += pop('#re-win', tPunch + 0.05, 0.4)
  js += blurIn('#re-relay', tRelay - 0.1)
  js += to('[id^=re-l],#re-win,#re-relay,#re-natl', { opacity: 0, filter: 'blur(10px)', duration: 0.35, ease: 'power2.in' }, l2.a - 0.4)
  js += pop('#re-lock', tTls - 0.1, 0.5)
  js += pop('#re-mtls', tTls + 0.3, 0.4)
  js += pop('#re-hub', tHub - 0.1, 0.45)
  js += ft('#re-ctl', { opacity: 0 }, { opacity: 1, duration: 0.4 }, tHub + 0.2)
  js += ft('#re-lock', { rotation: 0 }, { rotation: -12, duration: 0.12, yoyo: true, repeat: 3, immediateRender: false }, tEnc)
  return { bg: 'night', html, js, sfx: [['impact_soft', tNat], ['whoosh_short', go], ['ding', tPunch + 0.05], ['click', tTls]] }
}

scenes.toolbox = (s) => {
  const ls = [0, 1, 2, 3].map((i) => L(s, i))
  const tVs = at(s, 'vs code')
  const tTray = at(s, 'tray')
  const tPanel = at(s, 'web panel')
  const html = `
  <div class="abs" id="tb-1" style="inset:0">
    <div class="abs disp h" id="tb-1t" style="left:120px;top:110px;font-size:84px;color:#fff">Attach, <span style="color:#b3abff">tmux style</span></div>
    ${[0, 1, 2].map((i) => `<div class="term abs h" id="tb-p${i}" style="left:${[120, 1000, 1000][i]}px;top:${[260, 260, 600][i]}px;width:${[840, 800, 800][i]}px;height:${[660, 300, 320][i]}px"><div class="bar"><i></i><i></i><i></i><span>${['devbox-01 · dev', 'devbox-01 · logs', 'tower-32c · build'][i]}</span></div><pre>${['$ pnpm dev\n<span class="ok">  ➜ Local: http://localhost:3000</span>\n<span class="dim">  ready in 412 ms</span>', '<span class="dim">GET /api/jobs 200 4ms\nGET /api/me 200 1ms</span>', '<span class="dim">   Compiling longarm v0.2.0</span>\n<span class="ok">    Finished release</span>'][i]}</pre></div>`).join('')}
  </div>
  <div class="abs h" id="tb-2" style="inset:0">
    <div class="abs disp" style="left:120px;top:110px;font-size:84px;color:${C.ink}">Forward a port</div>
    <div class="chip abs mono" style="left:120px;top:260px;font-size:34px">$ longarm forward devbox-01 3000</div>
    <div class="card abs" id="tb-br" style="left:620px;top:380px;width:1180px;height:600px;overflow:hidden;padding:0">
      <div style="padding:18px 24px;background:${C.paper};display:flex;gap:10px;align-items:center"><i class="dot" style="background:#ff5f57"></i><i class="dot" style="background:#febc2e"></i><i class="dot" style="background:#28c840"></i><span class="mono" style="margin-left:20px;padding:8px 20px;border-radius:999px;background:#fff;font-size:24px">localhost:3000</span></div>
      <div style="padding:60px;font:800 70px/1 'Bricolage';color:${C.ink}">Your app,<br><span style="color:${C.blue}">served from devbox-01</span></div>
    </div>
  </div>
  <div class="abs h" id="tb-3" style="inset:0">
    <div class="abs disp" style="left:120px;top:110px;font-size:84px;color:#fff">Sync · SSH · <span style="color:#b3abff">VS Code Remote</span></div>
    <div class="chip abs mono" style="left:120px;top:260px;font-size:32px;background:${C.night2};color:#7cf3b0;border-color:${C.nline}">$ longarm sync .</div>
    <div class="abs" style="left:120px;top:380px;width:560px">${['src/router.ts', 'src/app.tsx', 'test/router.test.ts', '.env.example'].map((f, i) => `<div class="mono h" id="tb-sf${i}" style="font-size:28px;color:#d8d9ff;padding:14px 0;border-bottom:1px solid ${C.nline}">⇡ ${f}</div>`).join('')}</div>
    <div class="term abs" style="left:760px;top:330px;width:1040px;height:600px"><div class="bar"><i></i><i></i><i></i><span>SSH: devbox-01 · Visual Studio Code</span></div><pre><span class="dim">~/work/app</span> <span class="cmd">$ ls</span>\nsrc  test  package.json  README.md\n\n<span class="ok">no keys to manage · same mTLS channel</span></pre></div>
    <div class="abs h" id="tb-key" style="left:1600px;top:200px;font-size:100px">🔑</div><div class="abs h" id="tb-x" style="left:1590px;top:250px;width:140px;height:12px;background:${C.red};border-radius:8px;transform:rotate(-30deg)"></div>
  </div>
  <div class="abs h" id="tb-4" style="inset:0">
    <div class="abs disp" style="left:120px;top:90px;font-size:84px;color:${C.ink}">The whole fleet, at a glance</div>
    <img class="abs img" id="tb-tray" src="assets/tray-workers.png" style="left:150px;top:220px;width:460px;height:780px;border-radius:22px;box-shadow:0 40px 80px -30px rgba(12,13,44,.6)">
    <img class="abs img" id="tb-panel" src="assets/panel-nodes.png" style="left:690px;top:260px;width:1150px;height:719px;border-radius:18px;box-shadow:0 40px 80px -30px rgba(12,13,44,.6)">
  </div>`
  // background swaps with each feature (night, paper, night, paper)
  let js = set('#tb-stage', { background: C.night }, 0)
  js += slam('#tb-1t', ls[0].a - 0.05)
  ;[0, 1, 2].forEach((i) => (js += ft(`#tb-p${i}`, { opacity: 0, scale: 0.85, y: 60 }, { opacity: 1, scale: 1, y: 0, duration: 0.4, ease: 'back.out(1.7)' }, ls[0].a + 0.15 + i * 0.22)))
  const cut = (from, toId, t, bg) => to(from, { opacity: 0, x: -260, filter: 'blur(16px)', duration: 0.22, ease: 'power3.in' }, t - 0.22) + set('#tb-stage', { background: bg }, t) + ft(toId, { opacity: 0, x: 260, filter: 'blur(16px)' }, { opacity: 1, x: 0, filter: 'blur(0px)', duration: 0.3, ease: 'power3.out' }, t)
  js += cut('#tb-1', '#tb-2', ls[1].a - 0.05, C.paper)
  js += ft('#tb-br', { scale: 0.8, opacity: 0, rotation: -3 }, { scale: 1, opacity: 1, rotation: 0, duration: 0.5, ease: 'back.out(1.6)' }, ls[1].a + 0.4)
  js += cut('#tb-2', '#tb-3', ls[2].a - 0.05, C.night)
  ;[0, 1, 2, 3].forEach((i) => (js += ft(`#tb-sf${i}`, { opacity: 0, x: -40 }, { opacity: 1, x: 0, duration: 0.25 }, ls[2].a + 0.3 + i * 0.15)))
  js += pop('#tb-key', tVs + 0.3, 0.35)
  js += ft('#tb-x', { opacity: 0, scaleX: 0 }, { opacity: 1, scaleX: 1, duration: 0.25, ease: 'power3.out' }, tVs + 0.9)
  js += cut('#tb-3', '#tb-4', ls[3].a - 0.05, C.paper)
  js += ft('#tb-tray', { opacity: 0, y: 120, rotation: -4 }, { opacity: 1, y: 0, rotation: 0, duration: 0.55, ease: 'back.out(1.5)' }, tTray - 0.1)
  js += ft('#tb-panel', { opacity: 0, x: 200, rotationY: -18, transformPerspective: 1600 }, { opacity: 1, x: 0, rotationY: 0, duration: 0.7, ease: 'power3.out' }, tPanel - 0.15)
  return {
    bg: 'night',
    html,
    js,
    sfx: [['impact_soft', ls[0].a], ['whoosh_short', ls[1].a - 0.25], ['whoosh_short', ls[2].a - 0.25], ['whoosh_short', ls[3].a - 0.25], ['pop', tTray], ['pop', tPanel]],
  }
}

scenes.cta = (s) => {
  const [l0, l1, l2] = [0, 1, 2].map((i) => L(s, i))
  const rects = JSON.parse(readFileSync(join(root, 'assets/site-rects.json'), 'utf8'))
  const tThree = at(s, 'three minutes')
  const tHub = at(s, 'open your hub')
  const tInst = at(s, 'installer')
  const tLine = at(s, 'one line')
  const tApprove = at(s, 'approve')
  const tThat = at(s, "that's it")
  // the site still is 2880 px wide (2x); shown 1440 css px -> scale k into a 1600 px frame
  const k = 1600 / 1440
  const cam = (r, zoom) => ({ x: r3(-r.x * k * zoom + 160), y: r3(-(r.y - 40) * k * zoom + 160), scale: zoom })
  const html = `
  <div class="abs" id="ct-clock" style="left:0;right:0;top:220px;text-align:center">
    <div class="disp" style="font-size:300px;color:${C.blue}">${steps('ct-min', ['0:00', '1:00', '2:00', '3:00'], [0, tThree - 0.3, tThree - 0.1, tThree + 0.1]).html}</div>
    <div class="disp h" id="ct-mt" style="font-size:80px;margin-top:10px">minutes to your first job</div>
  </div>
  <div class="abs h" id="ct-site" style="left:160px;top:100px;width:1600px;height:880px;border-radius:28px;overflow:hidden;box-shadow:0 50px 90px -40px rgba(19,19,59,.55);border:2px solid ${C.line};background:${C.paper}">
    <div class="abs" style="left:0;right:0;top:0;height:72px;z-index:2;display:flex;align-items:center;gap:12px;padding:0 26px;background:#fff;border-bottom:2px solid ${C.line}">
      <i class="dot" style="background:#ff5f57"></i><i class="dot" style="background:#febc2e"></i><i class="dot" style="background:#28c840"></i>
      <span class="mono" style="margin-left:24px;padding:10px 26px;border-radius:999px;background:${C.paper};font-size:28px;color:${C.ink}">🔒 <span id="ct-url">${chars(HUB_LINK)}</span></span>
    </div>
    <div class="abs" style="left:0;right:0;top:72px;bottom:0;overflow:hidden">
    <div id="ct-cam" style="position:absolute;left:0;top:0;transform-origin:0 0;width:${r3(1440 * k)}px">
      <img class="img" src="assets/site-full.png" style="width:${r3(1440 * k)}px;height:${r3(rects.page.h * k)}px">
      <div class="abs h" id="ct-hl1" style="left:${r3(rects.msi.x * k - 10)}px;top:${r3(rects.msi.y * k - 10)}px;width:${r3(rects.msi.w * k + 20)}px;height:${r3(rects.msi.h * k + 20)}px;border:5px solid ${C.orange};border-radius:999px"></div>
      <div class="abs h" id="ct-hl2" style="left:${r3(rects.ps1.x * k - 10)}px;top:${r3(rects.ps1.y * k - 10)}px;width:${r3(rects.ps1.w * k + 20)}px;height:${r3(rects.ps1.h * k + 20)}px;border:5px solid ${C.orange};border-radius:18px"></div>
    </div>
    </div>
  </div>
  <div class="card abs h" id="ct-code" style="left:560px;top:300px;width:800px;padding:44px;text-align:center">
    <div class="mono" style="font-size:26px;color:${C.ink3}">${HUB_LINK}</div>
    <div class="mono" style="font-size:96px;font-weight:800;letter-spacing:.12em;margin:18px 0">BCDF-GHJK</div>
    <div class="chip" id="ct-ap" style="background:${C.blue};color:#fff;border:0;font-size:34px;padding:18px 40px">Approve</div>
  </div>
  <div class="abs disp h" id="ct-that" style="left:0;right:0;top:760px;text-align:center;font-size:110px;color:${C.green}">That's it.</div>`
  const top = cam({ x: 0, y: 40 }, 1)
  // push in on the install card: the MSI button to the end of the one-liner, centred in the 808 px view
  const z = 1.2
  const mid = (rects.msi.y + rects.ps1.y + rects.ps1.h) / 2
  const inst = { x: r3(-(rects.install.x - 30) * k * z + 160), y: r3(404 - mid * k * z), scale: z }
  let js = steps('ct-min', ['0:00', '1:00', '2:00', '3:00'], [0, tThree - 0.3, tThree - 0.1, tThree + 0.1]).js
  js += ft('#ct-clock', { opacity: 0, scale: 0.8 }, { opacity: 1, scale: 1, duration: 0.5, ease: 'back.out(1.7)' }, Math.max(0, l0.a - 0.3))
  js += blurIn('#ct-mt', tThree + 0.3)
  js += to('#ct-clock', { opacity: 0, y: -80, filter: 'blur(12px)', duration: 0.35, ease: 'power2.in' }, tHub - 0.35)
  js += ft('#ct-site', { opacity: 0, y: 100, rotationX: 12, transformPerspective: 2000 }, { opacity: 1, y: 0, rotationX: 0, duration: 0.7, ease: 'power3.out' }, tHub - 0.15)
  js += `tl.fromTo('#ct-url .ch',{opacity:0},{opacity:1,duration:0.01,stagger:0.045,ease:'none'},${r3(tHub)});\n`
  js += set('#ct-cam', top, 0)
  js += to('#ct-cam', { ...inst, duration: 1.1, ease: 'power3.inOut' }, tHub + 0.7)
  js += pop('#ct-hl1', tInst, 0.35)
  js += pop('#ct-hl2', tLine, 0.35)
  js += to('#ct-site', { opacity: 0, scale: 0.92, filter: 'blur(12px)', duration: 0.35, ease: 'power2.in' }, l2.a - 0.35)
  js += pop('#ct-code', l2.a - 0.05, 0.5)
  js += ft('#ct-ap', { scale: 1 }, { scale: 0.9, duration: 0.08, yoyo: true, repeat: 1 }, tApprove + 0.1)
  js += set('#ct-ap', { background: C.green }, tApprove + 0.2)
  js += slam('#ct-that', tThat - 0.05)
  return { bg: 'paper', html, js, sfx: [['ding', tThree + 0.1], ['whoosh_short', tHub - 0.15], ['pop', tInst], ['pop', tLine], ['ding', tApprove + 0.2], ['impact_soft', tThat]] }
}

scenes.outro = (s) => {
  const l0 = L(s, 0)
  const tLong = at(s, 'longarm')
  const tGive = at(s, 'give')
  const end = dur(s)
  const tiles = [
    // around the lockup (icon 810-1110 x 150-450, word 470-660, tagline 690-760, url 830-905), never on it
    ['t-green', '>_', -630, -240],
    ['t-purple', '≡', 630, -250],
    ['t-orange', '⚙', -760, 140],
    ['t-sky', '☁', 770, 100],
    ['t-green', '✓', -400, -120],
    ['t-blue', '⇄', 740, 480],
  ]
  const html = `
  <div class="abs" style="left:960px;top:420px">
    ${tiles.map(([c, g], i) => `<div class="tile abs h ${c}" id="ou-t${i}" style="left:-48px;top:-48px">${g}</div>`).join('')}
  </div>
  <img class="abs img h" id="ou-icon" src="assets/icon.png" style="left:810px;top:150px;width:300px;height:300px">
  <div class="abs disp h" id="ou-word" style="left:0;right:0;top:470px;text-align:center;font-size:190px;color:${C.ink}">Longarm</div>
  <div class="abs disp h" id="ou-tag" style="left:0;right:0;top:690px;text-align:center;font-size:64px;color:${C.ink2}">Give your coding agents a <span style="color:${C.blue}">long arm</span>.</div>
  <div class="abs h" id="ou-url" style="left:0;right:0;top:830px;text-align:center"><span class="chip mono" style="font-size:44px;padding:20px 40px;background:${C.blue};border-color:${C.blue};color:#fff">${HUB_LINK}</span>
    <div class="mono" style="margin-top:22px;font-size:24px;color:${C.ink3}">github.com/lubobill1990/longarm</div></div>`
  let js = ft('#ou-icon', { opacity: 0, scale: 0.3, rotation: -20 }, { opacity: 1, scale: 1, rotation: 0, duration: 0.7, ease: 'back.out(1.8)' }, Math.max(0, tLong - 0.35))
  js += slam('#ou-word', tLong)
  tiles.forEach(([, , x, y], i) => {
    const landed = tLong + 0.1 + i * 0.06 + 0.7
    const home = end - 2.4 + i * 0.04
    js += ft(`#ou-t${i}`, { opacity: 0, x: 0, y: 0, scale: 0.3 }, { opacity: 1, x, y, scale: 1, rotation: (i % 2 ? 1 : -1) * 10, duration: 0.7, ease: 'back.out(1.6)' }, tLong + 0.1 + i * 0.06)
    // bob gently until the tiles fly home (an odd repeat count ends where it started)
    const bobs = Math.floor((home - landed) / 1.4)
    if (bobs >= 2) js += to(`#ou-t${i}`, { y: y - 16, duration: 1.4, repeat: (bobs % 2 ? bobs - 1 : bobs) - 1, yoyo: true, ease: 'sine.inOut' }, landed)
  })
  js += blurIn('#ou-tag', tGive - 0.05, 0.4)
  js += pop('#ou-url', l0.b + 0.3, 0.5)
  // last bar: tiles fly home into the icon, the lockup holds
  js += tiles.map((_, i) => to(`#ou-t${i}`, { x: 0, y: -120, scale: 0.2, opacity: 0, duration: 0.5, ease: 'power3.in' }, end - 2.4 + i * 0.04)).join('')
  js += ft('#ou-icon', { scale: 1 }, { scale: 1.12, duration: 0.18, yoyo: true, repeat: 1, ease: 'power2.out', immediateRender: false }, end - 1.85)
  return { bg: 'paper', html, js, sfx: [['impact', tLong], ['whoosh_long', end - 2.4]] }
}

// ---- captions -------------------------------------------------------------------

function captions() {
  const lines = sections.flatMap((s) => s.lines)
  const html = lines.map((l, i) => `<div class="cap" id="cap${i}">${esc(l.text)}</div>`).join('')
  let js = ''
  lines.forEach((l, i) => {
    js += ft(`#cap${i}`, { opacity: 0, y: 14 }, { opacity: 1, y: 0, duration: 0.18, ease: 'power2.out' }, l.start - 0.05)
    const next = lines[i + 1]
    const off = next && next.start - l.end < 0.6 ? next.start - 0.07 : l.end + 0.25
    js += to(`#cap${i}`, { opacity: 0, duration: 0.12 }, off)
  })
  const css = `.cap{position:absolute;left:50%;bottom:44px;transform:translateX(-50%);max-width:1500px;width:max-content;text-align:center;font:600 38px/1.3 'Onest',sans-serif;color:#fff;background:rgba(12,13,44,.78);padding:12px 26px;border-radius:16px;opacity:0}`
  return { html, js, css }
}

// ---- sound effects --------------------------------------------------------------

function sfxClips(list) {
  if (!SFX) return ''
  const files = Object.fromEntries(SFX.sfx.map((x) => [x.name, x]))
  const NAMES = { whoosh_short: 'whoosh-short', whoosh_long: 'whoosh-long', impact_soft: 'impact', type: 'key-tap', click: 'click-soft' }
  // the music already hits on the drop, survive, toolbox and outro cues: impacts there stay under it
  const GAIN = { impact: 0.6, impact_soft: 0.3, type: 0.3, pop: 0.45, ding: 0.4, click: 0.5 }
  // typing is a run of key taps, a little irregular
  const expanded = list.flatMap(([name, t, g]) => (name === 'type' ? [0, 0.09, 0.2, 0.27, 0.38, 0.47, 0.6, 0.68].map((d) => [name, t + d, g]) : [[name, t, g]]))
  return expanded
    .map(([name, t, gain], i) => {
      const f = files[NAMES[name] ?? name]
      if (!f) return fail(`no sound effect ${name}`)
      const start = r3(Math.max(0, t - (f.peak ?? 0)))
      const vol = gain ?? GAIN[name] ?? 0.5
      return `<audio class="clip" id="sfx${i}" data-start="${start}" data-duration="${r3(f.duration)}" data-volume="${vol}" src="${f.file}"></audio>`
    })
    .join('\n      ')
}

// ---- write the project -------------------------------------------------------------

// only what this script writes: hyperframes keeps caches in film/ (and may hold it open)
for (const d of ['compositions', 'fonts', 'vendor', 'assets', 'audio']) rmSync(join(film, d), { recursive: true, force: true })
mkdirSync(join(film, 'compositions'), { recursive: true })
mkdirSync(join(film, 'fonts'), { recursive: true })
mkdirSync(join(film, 'vendor'), { recursive: true })
cpSync(join(root, 'assets'), join(film, 'assets'), { recursive: true })
cpSync(join(root, '..', '..', 'packaging', 'icons', 'longarm.png'), join(film, 'assets', 'icon.png'))
for (const f of ['tray-workers.png', 'panel-nodes.png']) cpSync(join(root, '..', '..', 'docs', 'ui-screenshots', f), join(film, 'assets', f))
cpSync(join(root, 'audio'), join(film, 'audio'), { recursive: true })
const fs = join(web, 'node_modules', '@fontsource-variable')
cpSync(join(fs, 'bricolage-grotesque/files/bricolage-grotesque-latin-opsz-normal.woff2'), join(film, 'fonts/bricolage.woff2'))
cpSync(join(fs, 'onest/files/onest-latin-wght-normal.woff2'), join(film, 'fonts/onest.woff2'))
cpSync(join(fs, 'jetbrains-mono/files/jetbrains-mono-latin-wght-normal.woff2'), join(film, 'fonts/jbmono.woff2'))
cpSync(join(root, 'node_modules/gsap/dist/gsap.min.js'), join(film, 'vendor/gsap.min.js'))

const allSfx = []
const mounts = sections.map((s, i) => {
  const make = scenes[s.id] ?? fail(`no scene for ${s.id}`)
  const sc = make(s)
  for (const [name, t, g] of sc.sfx ?? []) allSfx.push([name, r3(s.a + t), g])
  const id = s.id
  const file = `<template id="${id}-template">
  <div data-composition-id="${id}" data-width="${W}" data-height="${H}" data-duration="${dur(s)}">
    <div class="stage ${sc.bg}" id="${PFX[id]}-stage">${sc.html}
    </div>
    ${sc.css ? `<style>${sc.css}</style>` : ''}
    <script>
      (function () {
        const tl = gsap.timeline({ paused: true });
${sc.js}
        window.__timelines = window.__timelines || {};
        window.__timelines["${id}"] = tl;
      })();
    </script>
  </div>
</template>
`
  writeFileSync(join(film, 'compositions', `${id}.html`), file)
  return `<div id="sc-${id}" class="scene" data-composition-id="${id}" data-composition-src="compositions/${id}.html" data-start="${r3(s.a)}" data-duration="${dur(s)}" data-track-index="1" data-width="${W}" data-height="${H}"></div>`
})

const cap = captions()
const index = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=${W}, height=${H}" />
    <title>Longarm - give your coding agents a long arm</title>
    <script src="vendor/gsap.min.js"></script>
    <style>${BASE_CSS}
${cap.css}</style>
  </head>
  <body>
    <div id="root" data-composition-id="main" data-start="0" data-duration="${r3(T.total)}" data-width="${W}" data-height="${H}">
      ${mounts.join('\n      ')}
      <div id="captions" class="scene" data-track-kind="captions" data-composition-id="captions" data-start="0" data-duration="${r3(T.total)}" data-track-index="2" data-width="${W}" data-height="${H}">
        ${cap.html}
        <script>
          (function () {
            const tl = gsap.timeline({ paused: true });
${cap.js}
            window.__timelines = window.__timelines || {};
            window.__timelines["captions"] = tl;
          })();
        </script>
      </div>
      <audio class="clip" id="mix" data-start="0" data-duration="${r3(T.total)}" data-volume="1" src="audio/mix.wav"></audio>
      ${sfxClips(allSfx)}
    </div>
    <script>
      window.__timelines = window.__timelines || {};
      window.__timelines["main"] = gsap.timeline({ paused: true });
    </script>
  </body>
</html>
`
writeFileSync(join(film, 'index.html'), index)
writeFileSync(join(film, 'sfx-sheet.json'), JSON.stringify(allSfx, null, 1))
writeFileSync(
  join(film, 'package.json'),
  JSON.stringify({ name: 'longarm-promo', private: true, type: 'module', scripts: { check: 'npx --yes hyperframes@0.8.200 check', render: 'npx --yes hyperframes@0.8.200 render' } }, null, 2),
)
console.log(`film/: ${sections.length} scenes, ${r3(T.total)} s, ${allSfx.length} sfx`)
