import { useEffect, useRef, useState } from 'react'
import type { LongarmSession } from '../api/sessions'
import type { MetricsPoint, NodeInfo, PoolWorker } from '../api/types'
import { ErrorBox, usePoll } from '../components/common'
import { IconPlus, IconTerminal } from '../components/icons'
import { OfflineRow, WorkerCard, type CardActions } from '../components/WorkerCard'
import { poolMachines } from '../lib/machines'
import { TerminalView } from '../sessions/TerminalView'
import { Skeleton } from '../tray/ui'
import { hasNativeDesktop, mobileApi, nativeURL, type MobileStatus, type SignInState } from './api'
import { KeyBar } from './KeyBar'
import '../tray/tokens.css'
import '../tray/tray.css'
import '../sessions/sessions.css'
import './mobile.css'

// The phone app's shell (/m; docs/mobile.md): sign in, your machines (the
// tray's cards: metrics, state, versions), a machine's sessions with a new
// terminal, a terminal with a key bar. The core (internal/mobile) serves it
// in the app's WebView; the desktop viewer is native (longarm-native://).
//
// Routes are the hash: #/ (machines), #/w/<worker>, #/w/<worker>/s/<session>
// - the phone's back gesture / button goes back through them.

type Route = { kind: 'machines' } | { kind: 'machine'; worker: string } | { kind: 'terminal'; worker: string; session: string }

function parseRoute(hash: string): Route {
  const p = hash.replace(/^#\/?/, '').split('/').map(decodeURIComponent)
  if (p[0] === 'w' && p[1]) return p[2] === 's' && p[3] ? { kind: 'terminal', worker: p[1], session: p[3] } : { kind: 'machine', worker: p[1] }
  return { kind: 'machines' }
}

export const go = (...parts: string[]) => {
  location.hash = '#/' + parts.map(encodeURIComponent).join('/')
}

function useRoute(): Route {
  const [r, setR] = useState(() => parseRoute(location.hash))
  useEffect(() => {
    const on = () => setR(parseRoute(location.hash))
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return r
}

export function MobileApp() {
  const route = useRoute()
  const st = usePoll(mobileApi.status, 0)
  if (!st.data) return <div className="m-app">{st.error ? <ErrorBox error={st.error} /> : <Skeleton lines={3} label="starting…" />}</div>
  if (!st.data.signedIn) return <SignIn status={st.data} onDone={st.reload} />
  return (
    <div className="m-app" data-route={route.kind}>
      {route.kind === 'machines' && <Machines status={st.data} onSignedOut={st.reload} />}
      {route.kind === 'machine' && <Machine worker={route.worker} />}
      {route.kind === 'terminal' && <Terminal worker={route.worker} session={route.session} />}
    </div>
  )
}

function Header({ title, sub, back, right }: { title: string; sub?: string; back?: boolean; right?: React.ReactNode }) {
  return (
    <header className="m-head">
      {back && (
        <button className="m-back" aria-label="back" onClick={() => history.back()}>
          ‹
        </button>
      )}
      <div className="m-title">
        <div className="m-name">{title}</div>
        {sub && <div className="m-sub muted">{sub}</div>}
      </div>
      {right}
    </header>
  )
}

// ---------------------------------------------------------------- sign-in

function SignIn({ status, onDone }: { status: MobileStatus; onDone: () => void }) {
  const [hub, setHub] = useState(status.hub ?? '')
  const [st, setSt] = useState<SignInState>()
  const [error, setError] = useState(status.error)
  const running = !!st && !st.done
  useEffect(() => {
    if (!running) return
    const t = setInterval(async () => {
      try {
        const s = await mobileApi.signInState()
        setSt(s)
        if (s.done) {
          if (s.error) setError(s.error)
          else onDone()
        }
      } catch (e) {
        setError(String(e))
      }
    }, 1000)
    return () => clearInterval(t)
  }, [running, onDone])
  const start = async () => {
    setError(undefined)
    try {
      setSt(await mobileApi.signIn(hub.trim(), 'phone'))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }
  return (
    <div className="m-app">
      <Header title="Longarm" sub="Sign this phone in" />
      <div className="m-body">
        <ErrorBox error={error} />
        <label className="m-field">
          <span>Hub</span>
          <input value={hub} onChange={(e) => setHub(e.target.value)} placeholder="longarm.example.com" autoCapitalize="off" autoCorrect="off" spellCheck={false} disabled={running} />
        </label>
        <button className="m-btn primary" disabled={running || !hub.trim()} onClick={start}>
          {running ? 'Waiting for approval…' : 'Sign in'}
        </button>
        {st?.code && !st.done && (
          <div className="m-card m-code">
            <p>
              Open this on any device, sign in with your account and enter the code:
            </p>
            <p>
              <a href={st.url} target="_blank" rel="noreferrer" className="mono">
                {st.url}
              </a>
            </p>
            <p className="m-codeval mono">{st.code}</p>
            <button className="m-btn" onClick={() => navigator.clipboard?.writeText(st.url ?? '')}>
              Copy link
            </button>
          </div>
        )}
        {st?.message && !st.done && <p className="muted small">{st.message}</p>}
        <p className="muted small">The phone gets a certificate of its own; your machines then show here. Only machines you own are listed.</p>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- machines

function asWorker(n: NodeInfo, history?: MetricsPoint[]): PoolWorker {
  return { name: n.name ?? '', state: n.online ? 'online' : 'offline', since: n.last_seen, source: 'hub', metrics: n.metrics, node: n, history }
}

const HISTORY = 60 // samples kept per machine (5 minutes at one a 5 s poll)

/** history: the cards' charts from the samples this page has seen (the hub's record has the latest one only). */
function useHistory(nodes: NodeInfo[] | undefined): Map<string, MetricsPoint[]> {
  const hist = useRef(new Map<string, MetricsPoint[]>())
  const last = useRef(new Map<string, string>())
  for (const n of nodes ?? []) {
    const m = n.metrics
    const name = n.name ?? ''
    if (!m?.time || last.current.get(name) === m.time) continue
    last.current.set(name, m.time)
    const memTotal = Number(m.mem_total_bytes ?? 0)
    const diskTotal = Number(m.disk_total_bytes ?? 0)
    const p: MetricsPoint = {
      time: m.time,
      cpu: m.cpu_percent ?? 0,
      mem: memTotal ? (Number(m.mem_used_bytes ?? 0) / memTotal) * 100 : 0,
      disk: diskTotal ? (1 - Number(m.disk_free_bytes ?? 0) / diskTotal) * 100 : undefined,
      net_rx: Number(m.net_rx_bytes_per_sec ?? 0),
      net_tx: Number(m.net_tx_bytes_per_sec ?? 0),
    }
    hist.current.set(name, [...(hist.current.get(name) ?? []), p].slice(-HISTORY))
  }
  return hist.current
}

function Machines({ status, onSignedOut }: { status: MobileStatus; onSignedOut: () => void }) {
  const ms = usePoll(mobileApi.machines, 5000)
  const hist = useHistory(ms.data)
  const now = Date.now()
  const desktop = hasNativeDesktop()
  const actions: CardActions = {
    attach: (w) => go('w', w),
    sessions: (w) => go('w', w),
    desktop: desktop ? (w) => (location.href = nativeURL('desktop', w)) : undefined,
  }
  const workers = (ms.data ?? []).map((n) => asWorker(n, hist.get(n.name ?? '')))
  const { active, offline } = poolMachines(workers, now)
  return (
    <>
      <Header
        title="Machines"
        sub={`${status.user} · ${status.hub}`}
        right={
          <button
            className="m-link small"
            onClick={async () => {
              if (!confirm('Sign this phone out? It forgets its certificate; sign in again to come back.')) return
              await mobileApi.signOut()
              onSignedOut()
            }}
          >
            Sign out
          </button>
        }
      />
      <div className="m-body">
        <ErrorBox error={ms.error} />
        {!ms.data && !ms.error && <Skeleton card lines={4} label="asking the hub…" />}
        {ms.data && workers.length === 0 && <p className="muted">No machines of yours yet: add one from a computer (longarm worker add, or the tray).</p>}
        <div className="cards">
          {active.map((m) => (
            <WorkerCard key={m.key} m={m} now={now} actions={actions} />
          ))}
        </div>
        {offline.length > 0 && (
          <details className="offline-section">
            <summary>
              <span className="summary-title">Offline ({offline.length})</span>
            </summary>
            {offline.map((m) => (
              <OfflineRow key={m.key} m={m} actions={actions} />
            ))}
          </details>
        )}
        <p className="muted small m-foot">Longarm {status.version}</p>
      </div>
    </>
  )
}

// ---------------------------------------------------------------- one machine

function Machine({ worker }: { worker: string }) {
  const ss = usePoll(() => mobileApi.sessions(worker), 5000, [worker])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const desktop = hasNativeDesktop()
  const start = async (agent?: 'claude' | 'copilot') => {
    setBusy(true)
    setError(undefined)
    try {
      // a size close to the phone's: the terminal resizes it when it attaches
      const s = await mobileApi.start(worker, { agent, cols: 60, rows: 30 })
      go('w', worker, 's', s.name)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const sessions = (ss.data ?? []).slice().sort((a, b) => (b.created_at ?? '').localeCompare(a.created_at ?? ''))
  return (
    <>
      <Header title={worker} sub="Sessions" back />
      <div className="m-body">
        <ErrorBox error={error ?? ss.error} />
        <div className="m-actions">
          <button className="m-btn primary" disabled={busy} onClick={() => start()}>
            <IconPlus size={16} /> New terminal
          </button>
          {desktop && (
            <button className="m-btn" onClick={() => (location.href = nativeURL('desktop', worker))}>
              Desktop
            </button>
          )}
        </div>
        <div className="m-actions">
          <button className="m-btn small" disabled={busy} onClick={() => start('claude')}>
            New Claude Code
          </button>
          <button className="m-btn small" disabled={busy} onClick={() => start('copilot')}>
            New Copilot CLI
          </button>
        </div>
        {!ss.data && !ss.error && <Skeleton lines={3} label={`connecting to ${worker}…`} />}
        {ss.data && sessions.length === 0 && <p className="muted">No sessions on {worker}. A new terminal keeps running when you leave it: come back to it here (or with longarm attach on a computer).</p>}
        {sessions.map((s) => (
          <SessionRow key={s.name} worker={worker} s={s} onStopped={ss.reload} />
        ))}
      </div>
    </>
  )
}

function SessionRow({ worker, s, onStopped }: { worker: string; s: LongarmSession; onStopped: () => void }) {
  const ended = !!s.ended_at || s.state === 'JOB_STATE_SUCCEEDED' || s.state === 'JOB_STATE_FAILED'
  const cmd = (s.argv ?? []).join(' ') || 'shell'
  return (
    <div className="m-card m-session" onClick={() => go('w', worker, 's', s.name)}>
      <IconTerminal size={16} />
      <div className="m-sinfo">
        <div className="m-name">{s.name}</div>
        <div className="muted small mono">
          {cmd}
          {ended ? ` · ended${s.has_exit_code ? ` (${s.exit_code})` : ''}` : s.attached ? ` · ${s.attached} attached` : ''}
        </div>
      </div>
      {!ended && (
        <button
          className="m-link small"
          onClick={async (e) => {
            e.stopPropagation()
            if (!confirm(`Stop ${s.name}? Its program gets Ctrl-C, then is ended.`)) return
            await mobileApi.stop(worker, s.name)
            onStopped()
          }}
        >
          Stop
        </button>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- terminal

function Terminal({ worker, session }: { worker: string; session: string }) {
  const send = useRef<(b: Uint8Array<ArrayBuffer>) => void>(undefined)
  // the visible height: the soft keyboard takes the rest (visualViewport)
  const [h, setH] = useState<number>(() => window.visualViewport?.height ?? window.innerHeight)
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    const on = () => setH(vv.height)
    vv.addEventListener('resize', on)
    return () => vv.removeEventListener('resize', on)
  }, [])
  return (
    <div className="m-termpage" style={{ height: h }}>
      <Header title={session} sub={worker} back />
      <div className="m-term">
        <TerminalView worker={worker} session={session} visible touch send={send} />
      </div>
      <KeyBar send={(s) => send.current?.(new TextEncoder().encode(s))} />
    </div>
  )
}
