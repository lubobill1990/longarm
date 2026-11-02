import { useEffect, useRef, useState, type ReactNode } from 'react'
import { hubApi, type AuthProvider } from '../api/hub'
import type { LatestRelease, Me } from '../api/types'
import { feedbackURL } from '../lib/feedback'
import { msiFile } from '../lib/release'
import { Link } from '../lib/router'
import { installLines } from '../panel/GetPage'
import cool from './img/cool.webp'
import hero from './img/hero.webp'
import melting from './img/melting.webp'
import sleep from './img/sleep.webp'
import wave from './img/wave.webp'
import mascot from './img/mascot.webp'
import './landing.css'

// The hub's front page: what Longarm is, how to install it on this hub,
// and (top right) the visitor's sign-in state with the way into their hub.
// Public: it only reads /api/me (401 = signed out), /auth/info and
// /releases/latest.json.

const GITHUB = 'https://github.com/lubobill1990/longarm'
const DOCS = `${GITHUB}/blob/main/docs/usage.md`

const reducedMotion = () => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches

/** Adds "in" to the element once it scrolls into view (at once without IntersectionObserver). */
function useInView<T extends Element>(threshold = 0.25) {
  const ref = useRef<T>(null)
  const [inView, setInView] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el || typeof IntersectionObserver === 'undefined') {
      setInView(true)
      return
    }
    const io = new IntersectionObserver(
      ([e]) => {
        if (e.isIntersecting) {
          setInView(true)
          io.disconnect()
        }
      },
      { threshold },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [threshold])
  return [ref, inView] as const
}

function Reveal({ children, className = '', delay = 0, as: Tag = 'div' }: { children: ReactNode; className?: string; delay?: number; as?: 'div' | 'section' | 'li' }) {
  const [ref, inView] = useInView<HTMLDivElement>(0.15)
  return (
    <Tag ref={ref as never} className={`rv ${inView ? 'in' : ''} ${className}`} style={{ transitionDelay: `${delay}ms` }}>
      {children}
    </Tag>
  )
}

/** A counter that advances every `ms` while `on`. */
function useTicker(ms: number, on = true) {
  const [n, setN] = useState(0)
  useEffect(() => {
    if (!on || reducedMotion()) return
    const t = setInterval(() => setN((x) => x + 1), ms)
    return () => clearInterval(t)
  }, [ms, on])
  return n
}

function CopyLine({ text, testId }: { text: string; testId?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="lp-copy" data-testid={testId}>
      <code>{text}</code>
      <button
        type="button"
        onClick={() => {
          navigator.clipboard?.writeText(text).then(
            () => {
              setCopied(true)
              setTimeout(() => setCopied(false), 1400)
            },
            () => {},
          )
        }}
      >
        {copied ? 'Copied ✓' : 'Copy'}
      </button>
    </div>
  )
}

// ---- top bar -------------------------------------------------------------

function AuthCorner({ me, hubHome }: { me: Me | null | undefined; hubHome: string }) {
  if (me === undefined) return <span className="lp-auth-skel" aria-hidden="true" />
  if (me === null) {
    return (
      <div className="lp-auth" data-testid="landing-signed-out">
        <Link to="/login" className="lp-btn ghost sm">
          Sign in
        </Link>
        <a href="#start" className="lp-btn primary sm lp-wide-only">
          Get started
        </a>
      </div>
    )
  }
  return (
    <div className="lp-auth" data-testid="landing-signed-in">
      <span className="lp-who" title={`Signed in to ${me.hub}`}>
        <span className="lp-avatar">{(me.user || '?').slice(0, 1).toUpperCase()}</span>
        <span className="lp-who-name">{me.user}</span>
        <span className="lp-dot" aria-label="signed in" />
      </span>
      <Link to={hubHome} className="lp-btn primary sm">
        Open my hub →
      </Link>
    </div>
  )
}

function TopBar({ me, hubHome }: { me: Me | null | undefined; hubHome: string }) {
  const [scrolled, setScrolled] = useState(false)
  useEffect(() => {
    const on = () => setScrolled(window.scrollY > 8)
    on()
    window.addEventListener('scroll', on, { passive: true })
    return () => window.removeEventListener('scroll', on)
  }, [])
  return (
    <header className={`lp-top ${scrolled ? 'scrolled' : ''}`}>
      <a href="#top" className="lp-logo">
        <img src={mascot} alt="" width={34} height={34} />
        <span>Longarm</span>
      </a>
      <nav className="lp-nav">
        <a href="#how">How it works</a>
        <a href="#agents">For agents</a>
        <a href="#features">Features</a>
        <a href="#start">Install</a>
      </nav>
      <AuthCorner me={me} hubHome={hubHome} />
    </header>
  )
}

// ---- hero ----------------------------------------------------------------

const DISPATCH = [
  { cmd: 'pnpm test', worker: 'devbox-01', note: '31 GB free', tint: 'green' },
  { cmd: 'cargo build --release', worker: 'tower-32c', note: '32 cores', tint: 'orange' },
  { cmd: 'claude -p "fix the flaky test"', worker: 'cloud-vm', note: 'own worktree', tint: 'purple' },
  { cmd: 'go test ./...', worker: 'old-thinkpad', note: 'idle', tint: 'sky' },
  { cmd: 'npx playwright test', worker: 'devbox-02', note: '18 GB free', tint: 'green' },
]

function Hero({ me, hubHome }: { me: Me | null | undefined; hubHome: string }) {
  const tick = useTicker(2200)
  const shown = [0, 1, 2].map((i) => DISPATCH[(tick + i) % DISPATCH.length])
  return (
    <section className="lp-hero" id="top">
      <div className="lp-hero-copy">
        <p className="lp-eyebrow load" style={{ animationDelay: '60ms' }}>
          <span className="lp-pulse" /> for Claude Code · Copilot CLI · Codex · Gemini CLI
        </p>
        <h1 className="load" style={{ animationDelay: '140ms' }}>
          Give your coding agents a{' '}
          <span className="lp-longarm">
            <span className="lp-stretch">long</span> arm
            <svg viewBox="0 0 300 40" preserveAspectRatio="none" aria-hidden="true">
              <path d="M4 26 C 60 6, 120 38, 180 20 S 270 8, 296 22" />
            </svg>
          </span>
          .
        </h1>
        <p className="lp-lede load" style={{ animationDelay: '240ms' }}>
          Longarm turns the spare machines around you - a workstation under the desk, an old laptop, a cloud VM - into <b>one pool of workers</b>. Your agents run
          builds, test suites and whole sessions there. Your laptop stays cool and quiet.
        </p>
        <div className="lp-cta load" style={{ animationDelay: '340ms' }}>
          <a href="#start" className="lp-btn primary lg">
            Install in 3 minutes
          </a>
          {me ? (
            <Link to={hubHome} className="lp-btn ghost lg">
              Open my hub
            </Link>
          ) : (
            <a href="#how" className="lp-btn ghost lg">
              See how it works
            </a>
          )}
        </div>
        <ul className="lp-hero-facts load" style={{ animationDelay: '440ms' }}>
          <li>Windows · macOS · Linux</li>
          <li>Through any NAT</li>
          <li>mTLS end to end</li>
        </ul>
      </div>
      <div className="lp-hero-art load" style={{ animationDelay: '200ms' }}>
        <img src={hero} alt="The Longarm robot on a laptop, reaching out with long arms to a workstation, a server, an old laptop and the cloud" />
        <ul className="lp-dispatch" aria-label="jobs being dispatched">
          {shown.map((j, i) => (
            <li key={`${tick}-${i}`} className={`lp-job t-${j.tint}`} style={{ animationDelay: `${i * 90}ms` }}>
              <span className="lp-job-cmd">
                <span className="faint">$</span> longarm run -- {j.cmd}
              </span>
              <span className="lp-job-to">
                → <b>{j.worker}</b> <span className="faint">· {j.note}</span>
              </span>
              <span className="lp-job-bar">
                <i style={{ animationDuration: `${1.6 + i * 0.5}s` }} />
              </span>
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}

// ---- before / after ------------------------------------------------------

function Meter({ label, value, unit, hot }: { label: string; value: number; unit: string; hot?: boolean }) {
  return (
    <div className={`lp-meter ${hot ? 'hot' : ''}`}>
      <span className="lp-meter-l">{label}</span>
      <span className="lp-meter-track">
        <i style={{ width: `${Math.min(100, value)}%` }} />
      </span>
      <span className="lp-meter-v">
        {value}
        {unit}
      </span>
    </div>
  )
}

function BeforeAfter() {
  const [ref, inView] = useInView<HTMLDivElement>(0.35)
  const [manual, setManual] = useState<boolean>()
  const auto = useTicker(3800, inView && manual === undefined)
  const after = manual ?? auto % 2 === 1
  return (
    <section className="lp-sec lp-ba" ref={ref}>
      <Reveal className="lp-sec-head">
        <p className="lp-kicker">The problem</p>
        <h2>Agents are great at starting work - and terrible at waiting for it.</h2>
        <p className="lp-sub">
          One <code>pnpm test</code> pins your CPU. A second agent session doubles it. Three parallel sub-agents and the fans scream. Meanwhile there is a 32-core box at
          home doing nothing.
        </p>
      </Reveal>
      <Reveal className={`lp-ba-card ${after ? 'after' : 'before'}`}>
        <div className="lp-switch" role="radiogroup" aria-label="compare">
          <button role="radio" aria-checked={!after} className={!after ? 'on' : ''} onClick={() => setManual(false)}>
            Without Longarm
          </button>
          <button role="radio" aria-checked={after} className={after ? 'on' : ''} onClick={() => setManual(true)}>
            With Longarm
          </button>
          <span className="lp-switch-knob" />
        </div>
        <div className="lp-ba-body">
          <div className="lp-ba-art">
            <img src={melting} alt="An overheating laptop" className={after ? 'gone' : ''} />
            <img src={cool} alt="A relaxed, cool laptop" className={after ? '' : 'gone'} />
          </div>
          <div className="lp-ba-stats">
            <h3>Your laptop</h3>
            <Meter label="CPU" value={after ? 7 : 100} unit="%" hot={!after} />
            <Meter label="Memory" value={after ? 38 : 97} unit="%" hot={!after} />
            <Meter label="Fan" value={after ? 12 : 100} unit="%" hot={!after} />
            <h3>Your other machines</h3>
            <div className="lp-workers">
              {['tower-32c', 'devbox-01', 'cloud-vm', 'old-thinkpad'].map((w, i) => (
                <span key={w} className={`lp-worker ${after ? 'busy' : ''}`} style={{ transitionDelay: `${i * 80}ms` }}>
                  <i />
                  {w}
                  <em>{after ? ['cargo build', 'pnpm test', 'claude -p', 'go test'][i] : 'idle'}</em>
                </span>
              ))}
            </div>
          </div>
        </div>
      </Reveal>
    </section>
  )
}

// ---- how it works ----------------------------------------------------------

const TERM: { t: string; cls?: string; pause?: number }[] = [
  { t: '$ longarm run --repo . -- pnpm test', cls: 'cmd', pause: 500 },
  { t: 'placed on tower-32c (31.2 GB free, 4% CPU) · worktree of 3f9c2e1 + your changes', cls: 'dim' },
  { t: 'job 01KQ7Z3M started', cls: 'dim', pause: 400 },
  { t: ' ✓ packages/core   412 tests   6.1s', cls: 'ok' },
  { t: ' ✓ packages/web    268 tests   9.4s', cls: 'ok' },
  { t: ' ✓ packages/cli     97 tests   3.2s', cls: 'ok', pause: 300 },
  { t: 'Test Files  64 passed · Tests 777 passed', cls: 'ok' },
  { t: 'exit 0 · 18.7s on tower-32c · your laptop: 6% CPU', cls: 'win' },
]

function Terminal({ lines = TERM, title = 'longarm' }: { lines?: typeof TERM; title?: string }) {
  const [ref, inView] = useInView<HTMLDivElement>(0.4)
  const [n, setN] = useState(0) // lines shown
  const [chars, setChars] = useState(0) // of the first (typed) line
  useEffect(() => {
    if (!inView) return
    if (reducedMotion()) {
      setChars(lines[0].t.length)
      setN(lines.length)
      return
    }
    let alive = true
    const timers: ReturnType<typeof setTimeout>[] = []
    const later = (fn: () => void, ms: number) => timers.push(setTimeout(() => alive && fn(), ms))
    const run = () => {
      setN(0)
      setChars(0)
      let t = 300
      for (let c = 1; c <= lines[0].t.length; c++) later(() => setChars(c), (t += 38))
      t += lines[0].pause ?? 300
      for (let i = 1; i <= lines.length; i++) {
        later(() => setN(i), t)
        t += 260 + (lines[i - 1].pause ?? 0)
      }
      later(run, t + 3800)
    }
    run()
    return () => {
      alive = false
      timers.forEach(clearTimeout)
    }
  }, [inView, lines])
  return (
    <div className="lp-term" ref={ref}>
      <div className="lp-term-bar">
        <i />
        <i />
        <i />
        <span>{title}</span>
      </div>
      <pre>
        <span className="cmd">
          {lines[0].t.slice(0, chars)}
          {n === 0 && <span className="lp-caret" />}
        </span>
        {lines.slice(1, n).map((l, i) => (
          <span key={i} className={`ln ${l.cls ?? ''}`}>
            {'\n'}
            {l.t}
          </span>
        ))}
      </pre>
    </div>
  )
}

function Installer() {
  const step = useTicker(1100)
  const s = step % 7 // 0-1 choose, 2-4 installing, 5-6 done
  return (
    <div className={`lp-msi s${Math.min(s, 5)}`} aria-hidden="true">
      <div className="lp-msi-bar">
        <img src={mascot} alt="" width={18} height={18} /> Longarm Setup
      </div>
      <div className="lp-msi-body">
        <label className="on">
          <i /> Default (recommended)
        </label>
        <label>
          <i /> Custom
        </label>
        <div className="lp-msi-roles">
          <span className="on">✓ Send jobs to workers</span>
          <span className={s >= 1 ? 'on' : ''}>✓ Run jobs on this computer</span>
        </div>
        <div className="lp-msi-prog">
          <i />
        </div>
        <div className="lp-msi-foot">
          <span className="faint small">{s < 2 ? 'Ready to install' : s < 5 ? 'Installing…' : 'Done - the tray is signing you in'}</span>
          <span className="lp-msi-btn">{s < 2 ? 'Install' : s < 5 ? '…' : 'Finish'}</span>
        </div>
      </div>
      <div className="lp-os">
        <span>Windows</span>
        <span>macOS</span>
        <span>Linux</span>
        <span>Dev Box</span>
      </div>
    </div>
  )
}

function SignInCode() {
  const step = useTicker(1500)
  const s = step % 4 // 0 code, 1 typing, 2 approved, 3 approved
  return (
    <div className="lp-code-card">
      <div className="lp-code-row">
        <span className="faint small">tray · sign-in code</span>
        <span className="lp-code">BCDF-GHJK</span>
      </div>
      <div className={`lp-code-browser s${s}`}>
        <span className="faint small">browser · {location.host}/link</span>
        <span className="lp-code-input">{s === 0 ? '' : 'BCDF-GHJK'}</span>
        <span className="lp-code-ok">{s >= 2 ? '✓ Approved - this laptop is your device and your worker' : 'Approve'}</span>
      </div>
    </div>
  )
}

function HowItWorks() {
  return (
    <section className="lp-sec lp-how" id="how">
      <Reveal className="lp-sec-head">
        <p className="lp-kicker">How it works</p>
        <h2>Three steps. Then every machine is a worker.</h2>
      </Reveal>
      <ol className="lp-steps">
        <Reveal as="li" className="lp-step" delay={0}>
          <span className="lp-step-n">1</span>
          <h3>Install on every machine</h3>
          <p>An MSI on Windows, one line on macOS and Linux. Workers run as a service - or as you, while you are signed in.</p>
          <Installer />
        </Reveal>
        <Reveal as="li" className="lp-step" delay={120}>
          <span className="lp-step-n">2</span>
          <h3>Sign in once, with one code</h3>
          <p>No hub address, no keys, no fingerprints. Approve the code in any browser - your phone will do.</p>
          <SignInCode />
        </Reveal>
        <Reveal as="li" className="lp-step wide" delay={240}>
          <span className="lp-step-n">3</span>
          <h3>Send it anything</h3>
          <p>
            The scheduler picks the machine with free memory and CPU to spare, gives the job a fresh worktree of your checkout - uncommitted changes included - and
            streams the output back.
          </p>
          <Terminal />
        </Reveal>
      </ol>
    </section>
  )
}

// ---- for agents ------------------------------------------------------------

const CHAT: { who: 'you' | 'agent' | 'tool'; text: string }[] = [
  { who: 'you', text: 'Run the full test suite on a worker, and fan out a sub-agent per failing package.' },
  { who: 'tool', text: 'longarm run --repo . --type test -- pnpm test   → tower-32c' },
  { who: 'agent', text: '3 packages fail. Starting 3 sub-agents, each on its own worker and worktree.' },
  { who: 'tool', text: 'longarm run --steerable --repo . -- claude "fix packages/web"   ×3' },
  { who: 'agent', text: 'All green. Branches pushed: fix/web, fix/cli, fix/core.' },
]

const FANOUT = [
  { w: 'tower-32c', task: 'fix packages/web', d: 5.2 },
  { w: 'devbox-01', task: 'fix packages/cli', d: 6.8 },
  { w: 'cloud-vm', task: 'fix packages/core', d: 4.4 },
  { w: 'devbox-02', task: 'e2e: chromium', d: 7.6 },
  { w: 'old-thinkpad', task: 'lint + typecheck', d: 3.6 },
]

function AgentChat() {
  const [ref, inView] = useInView<HTMLDivElement>(0.35)
  const n = useTicker(1300, inView)
  const shown = reducedMotion() ? CHAT.length : Math.min(CHAT.length, (n % (CHAT.length + 3)) + 1)
  return (
    <div className="lp-chat" ref={ref}>
      <div className="lp-chat-head">
        <span className="lp-chat-logo">✳</span> Claude Code <span className="faint">· longarm skill</span>
      </div>
      {CHAT.slice(0, shown).map((m, i) => (
        // lp-from-*: the panel's styles.css already has an .agent
        <div key={i} className={`lp-msg lp-from-${m.who}`}>
          {m.who === 'tool' ? (
            <>
              <span className="lp-tool-tag">Bash</span> <code>{m.text}</code>
            </>
          ) : (
            m.text
          )}
        </div>
      ))}
      {shown < CHAT.length && <div className="lp-typing" aria-hidden="true"><i /><i /><i /></div>}
    </div>
  )
}

function Agents() {
  return (
    <section className="lp-dark" id="agents">
      <div className="lp-dark-grid" aria-hidden="true" />
      <div className="lp-sec">
        <Reveal className="lp-sec-head">
          <p className="lp-kicker">Built for agents</p>
          <h2>Your agent already knows how to use it.</h2>
          <p className="lp-sub">
            One command installs the Longarm skill into Claude Code, Copilot CLI, Codex and Gemini CLI. From then on you just say <em>"run the tests on a worker"</em>.
          </p>
        </Reveal>
        <div className="lp-agents">
          <Reveal>
            <AgentChat />
          </Reveal>
          <Reveal delay={150} className="lp-fanout">
            <h3>Fan out</h3>
            <p className="faint">Parallel sub-agents, each on its own worker, in its own worktree - placed by a scheduler that knows who has memory to spare.</p>
            <ul>
              {FANOUT.map((f) => (
                <li key={f.w}>
                  <span className="lp-fan-w">{f.w}</span>
                  <span className="lp-fan-t">{f.task}</span>
                  <span className="lp-fan-bar">
                    <i style={{ animationDuration: `${f.d}s` }} />
                  </span>
                </li>
              ))}
            </ul>
          </Reveal>
        </div>
        <div className="lp-agent-cards">
          {[
            ['🧠', 'The Longarm skill', 'Teaches the agent to dispatch work, steer sessions and hand them off.', 'longarm skill install'],
            ['🔌', 'MCP server', 'fleet_status, fleet_exec, fleet_job_submit, fleet_sync, fleet_forward … Secrets passed by name, never through the model.', 'claude mcp add longarm -- longarm mcp'],
            ['🕹️', 'Steer it live', 'A remote agent runs on a real terminal: watch it, type into it, detach - it is still a job.', 'longarm run --steerable -- claude "fix it"'],
            ['🚚', 'Hand off a session', 'Move a whole conversation - uncommitted code and all - to another machine and keep going.', 'longarm handoff --to devbox-01 --latest'],
          ].map(([icon, title, text, cmd], i) => (
            <Reveal key={title} className="lp-acard" delay={i * 90}>
              <span className="lp-acard-i">{icon}</span>
              <h3>{title}</h3>
              <p>{text}</p>
              <code>{cmd}</code>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

// ---- survive + reach -------------------------------------------------------

function Survive() {
  const [ref, inView] = useInView<HTMLDivElement>(0.3)
  const n = useTicker(1400, inView)
  const step = reducedMotion() ? 4 : n % 6
  const events = ['Laptop lid closed', 'Wi-Fi → hotel network', 'longarmd restarted for an update', 'longarm attach · replayed 1,204 lines, live again']
  return (
    <section className="lp-sec lp-survive" ref={ref}>
      <Reveal className="lp-survive-art">
        <img src={sleep} alt="A closed laptop sleeping while the Longarm robot keeps working on a server" />
      </Reveal>
      <Reveal className="lp-survive-copy" delay={100}>
        <p className="lp-kicker">Survives everything</p>
        <h2>Close the lid. The job keeps running.</h2>
        <p className="lp-sub">
          Every job and session lives in its own small process on the worker. Change networks, restart the daemon, let it update itself - then re-attach right where you left
          off.
        </p>
        <ul className="lp-timeline">
          {events.map((e, i) => (
            <li key={e} className={step > i ? 'on' : ''}>
              <span className="lp-tl-dot" />
              {e}
            </li>
          ))}
        </ul>
        <div className="lp-job-steady">
          <span>job 01KQ7Z3M · cargo build --release</span>
          <span className="lp-steady-bar">
            <i />
          </span>
          <b>running</b>
        </div>
      </Reveal>
    </section>
  )
}

function Reach() {
  return (
    <section className="lp-sec lp-reach">
      <Reveal className="lp-sec-head center">
        <p className="lp-kicker">Reaches anything</p>
        <h2>Home router? Carrier-grade NAT? It still connects.</h2>
        <p className="lp-sub">
          Every connection races three transports at once; the first mutual-TLS handshake wins. The hub only coordinates - when it is down, workers keep working, and
          relayed traffic stays end-to-end encrypted.
        </p>
      </Reveal>
      <Reveal className="lp-race" delay={100}>
        <div className="lp-race-end you">your laptop</div>
        <div className="lp-lanes">
          <div className="lp-lane tcp">
            <span className="lp-lane-n">TCP</span>
            <span className="lp-wall" title="NAT">
              NAT
            </span>
            <i className="lp-runner" />
          </div>
          <div className="lp-lane ts">
            <span className="lp-lane-n">Tailscale</span>
            <i className="lp-runner" />
          </div>
          <div className="lp-lane ln">
            <span className="lp-lane-n">longnet · QUIC + hole punching</span>
            <i className="lp-runner" />
            <span className="lp-win">mTLS ✓ first</span>
          </div>
        </div>
        <div className="lp-race-end them">worker behind NAT</div>
      </Reveal>
      <p className="lp-race-note faint">… and a relay (UDP or TLS over TCP) when nothing else gets through.</p>
    </section>
  )
}

// ---- features --------------------------------------------------------------

const FEATURES: { icon: string; title: string; text: string; cmd?: string; big?: boolean; tint: string }[] = [
  { icon: '⌨', title: 'Remote terminals, tmux style', text: 'Named sessions, several clients at once, detach with Ctrl-] d, re-attach with replay.', cmd: 'longarm attach devbox -s dev', big: true, tint: 'blue' },
  { icon: '⇄', title: 'Port forwarding', text: "Put a worker's dev server on your localhost.", cmd: 'longarm forward devbox 3000', tint: 'green' },
  { icon: '⟲', title: 'Code sync', text: 'Mirror your checkout - commits, changes, untracked files - into a worktree on the worker.', cmd: 'longarm sync .', tint: 'orange' },
  { icon: '🔗', title: 'Port links', text: "Let jobs on one worker reach another worker's port: frontend here, backend there.", cmd: 'longarm link create api:8080 --on web', tint: 'purple' },
  { icon: '🔑', title: 'Built-in SSH', text: 'ssh, scp and VS Code Remote-SSH over the same mTLS channel - no keys to manage.', tint: 'sky' },
  { icon: '🖥', title: 'Remote desktop', text: 'Open a Dev Box in Windows App, other Windows workers in Remote Desktop.', cmd: 'longarm desktop devbox', tint: 'blue' },
  { icon: '📦', title: 'Packages for everything', text: 'MSI, deb and rpm, a macOS pkg and Homebrew cask, a Docker image for the hub.', tint: 'orange' },
  { icon: '⚖', title: 'Memory-aware scheduler', text: 'Picks the least-loaded worker with enough free memory, prefers ones that already have your repo, queues when nobody fits.', big: true, tint: 'green' },
  { icon: '📊', title: 'Tray and web panel', text: 'Live CPU and memory per worker, your link to each, your jobs and agent sessions.', tint: 'sky' },
  { icon: '✍', title: 'Signed self-updates', text: 'Workers update canary-first, health-check the new version and roll back by themselves.', tint: 'purple' },
]

function Features() {
  return (
    <section className="lp-sec lp-features" id="features">
      <Reveal className="lp-sec-head">
        <p className="lp-kicker">The toolbox</p>
        <h2>Everything you'd do on the machine - from wherever you are.</h2>
      </Reveal>
      <div className="lp-bento">
        {FEATURES.map((f, i) => (
          <Reveal key={f.title} className={`lp-feat t-${f.tint} ${f.big ? 'big' : ''}`} delay={(i % 3) * 80}>
            <span className="lp-feat-i">{f.icon}</span>
            <h3>{f.title}</h3>
            <p>{f.text}</p>
            {f.cmd && <code>{f.cmd}</code>}
          </Reveal>
        ))}
      </div>
    </section>
  )
}

// ---- get started -------------------------------------------------------------

type OS = 'windows' | 'unix' | 'installed'

function GetStarted({ me, hubHome, providers, feedback }: { me: Me | null | undefined; hubHome: string; providers?: AuthProvider[]; feedback: string }) {
  const [rel, setRel] = useState<LatestRelease>()
  const [os, setOS] = useState<OS>(() => (/Windows/i.test(navigator.userAgent) ? 'windows' : 'unix'))
  useEffect(() => {
    hubApi.latestRelease().then(setRel, () => {})
  }, [])
  const base = me?.base_url || location.origin
  const lines = installLines(base)
  const msi = msiFile(rel, 'amd64')
  const msiArm = msiFile(rel, 'arm64')
  const names = (providers ?? []).map((p) => p.label)
  const signIn = names.length ? `your ${names.join(', ').replace(/, ([^,]*)$/, ' or $1')} account` : 'your account'
  return (
    <section className="lp-sec lp-start" id="start">
      <Reveal className="lp-sec-head center">
        <p className="lp-kicker">Get started</p>
        <h2>Up and running in about three minutes.</h2>
        <p className="lp-sub">
          This hub - <b>{location.host}</b> - serves everything you need. Do this on each machine you want to use: your laptop and every machine that should run jobs.
        </p>
      </Reveal>
      <div className="lp-start-grid">
        <Reveal className="lp-install">
          <div className="lp-install-n">1</div>
          <h3>Install</h3>
          <div className="lp-tabs" role="tablist">
            {(
              [
                ['windows', 'Windows'],
                ['unix', 'macOS · Linux'],
                ['installed', 'Installed'],
              ] as [OS, string][]
            ).map(([k, label]) => (
              <button key={k} role="tab" aria-selected={os === k} className={os === k ? 'on' : ''} onClick={() => setOS(k)}>
                {label}
              </button>
            ))}
          </div>
          {os === 'windows' && (
            <div className="lp-tab-body">
              <div className="row wrap">
                {msi ? (
                  <a className="lp-btn primary" href={msi} data-testid="landing-msi">
                    ⬇ Download the MSI (x64)
                  </a>
                ) : (
                  <span className="faint">No MSI on this hub yet - use the one-liner.</span>
                )}
                {msiArm && (
                  <a className="lp-btn ghost" href={msiArm}>
                    ARM64
                  </a>
                )}
                {rel?.version && <span className="faint small">version {rel.version}</span>}
              </div>
              <p className="small">
                Run it, click Next, keep <b>Default</b> and pick what this computer does: send jobs to workers, run jobs, or both. The tray then starts the sign-in by
                itself.
              </p>
              <p className="small faint">Or one line in PowerShell:</p>
              <CopyLine text={lines.windows} testId="landing-ps1" />
            </div>
          )}
          {os === 'unix' && (
            <div className="lp-tab-body">
              <p className="small">One line - the hub serves the script with its address and CA pinned:</p>
              <CopyLine text={lines.unix} testId="landing-sh" />
              <p className="small faint">
                Linux packages (apt / dnf) and Homebrew: <a href={`${GITHUB}#quick-start`}>the README</a>.
              </p>
            </div>
          )}
          {os === 'installed' && (
            <div className="lp-tab-body">
              <p className="small">Sign this machine in to this hub - its domain is enough:</p>
              <CopyLine text={`longarmd login ${location.host} --role both`} />
            </div>
          )}
        </Reveal>
        <Reveal className="lp-install" delay={120}>
          <div className="lp-install-n">2</div>
          <h3>Approve one code</h3>
          <p className="small">
            The tray (or the script) shows a code. Open the link in any browser, sign in with {signIn}, enter the code and approve - once, for both the device and the
            worker.
          </p>
          <div className="lp-mini-code">
            <span>{location.host}/link</span>
            <b>BCDF-GHJK</b>
          </div>
        </Reveal>
        <Reveal className="lp-install" delay={240}>
          <div className="lp-install-n">3</div>
          <h3>Run something - and teach your agent</h3>
          <CopyLine text="longarm ls" />
          <CopyLine text="longarm run -- pnpm test" />
          <CopyLine text="longarm skill install" />
          <p className="small faint">Your machines and jobs show up in your hub and in the tray.</p>
        </Reveal>
      </div>
      <Reveal className="lp-final">
        <img src={wave} alt="" className="lp-final-mascot" />
        <div>
          <h2>Your machines are waiting.</h2>
          <p className="lp-sub">{me ? `Signed in as ${me.user}. Jump into your hub to see your machines and jobs.` : 'Sign in to see your machines, your jobs and the installers for this hub.'}</p>
          <div className="lp-cta">
            {me ? (
              <Link to={hubHome} className="lp-btn primary lg">
                Open my hub →
              </Link>
            ) : (
              <Link to="/login" className="lp-btn primary lg">
                Sign in to {location.host}
              </Link>
            )}
            <a className="lp-btn ghost lg" href={DOCS}>
              Read the docs
            </a>
          </div>
          <p className="lp-feedback">
            Questions, ideas, a bug?{' '}
            <a href={feedback} target="_blank" rel="noreferrer" data-testid="landing-feedback">
              Tell us →
            </a>
          </p>
        </div>
      </Reveal>
    </section>
  )
}

function Footer({ version, feedback }: { version?: string; feedback: string }) {
  return (
    <footer className="lp-foot">
      <span className="lp-logo">
        <img src={mascot} alt="" width={26} height={26} />
        <span>Longarm</span>
      </span>
      <span className="faint">Give your coding agents a long arm.{version ? ` · hub ${version}` : ''}</span>
      <span className="spacer" />
      <a href={feedback} target="_blank" rel="noreferrer">
        Feedback
      </a>
      <a href={GITHUB}>GitHub</a>
      <a href={DOCS}>Usage</a>
      <a href={`${GITHUB}/blob/main/docs/design.md`}>Design</a>
      <a href={`${GITHUB}/blob/main/docs/security.md`}>Security</a>
      <a href={`${GITHUB}#run-your-own-hub`}>Run your own hub</a>
      <a href="/status">Status</a>
      <a href="/licenses.txt">Licenses</a>
    </footer>
  )
}

/** The front page; `me`: undefined while loading, null when signed out. */
export default function LandingPage({ me, hubHome }: { me: Me | null | undefined; hubHome: string }) {
  // the hub's sign-in providers (the install steps) and feedback link, public
  const [auth, setAuth] = useState<{ providers?: AuthProvider[]; feedback_url?: string }>()
  useEffect(() => {
    hubApi.authInfo().then(
      (i) => setAuth({ ...i, providers: i.providers ?? (i.entra ? [{ id: '', label: 'Microsoft' }] : []) }),
      () => setAuth({}),
    )
  }, [])
  const feedback = feedbackURL(auth?.feedback_url ?? me?.feedback_url)
  useEffect(() => {
    document.title = 'Longarm - give your coding agents a long arm'
    document.documentElement.classList.add('lp-root')
    return () => document.documentElement.classList.remove('lp-root')
  }, [])
  return (
    <div className="lp" data-testid="landing">
      <div className="lp-bg" aria-hidden="true">
        <i className="b1" />
        <i className="b2" />
        <i className="b3" />
      </div>
      <TopBar me={me} hubHome={hubHome} />
      <main>
        <Hero me={me} hubHome={hubHome} />
        <BeforeAfter />
        <HowItWorks />
        <Agents />
        <Survive />
        <Reach />
        <Features />
        <GetStarted me={me} hubHome={hubHome} providers={auth?.providers} feedback={feedback} />
      </main>
      <Footer version={me?.version} feedback={feedback} />
    </div>
  )
}
