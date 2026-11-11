import { useEffect, useState } from 'react'
import './tokens.css'
import './tray.css'
import './updateprogress.css'

/**
 * The update progress window (longarm-tray --update-progress; Windows):
 * an update of this machine that the tray started stops the tray itself,
 * so this window - a copy of the tray that the installer leaves running -
 * shows how it goes. Its Go side (internal/tray/updateprogress.go) follows
 * the Longarm Update service; it closes itself once the new version is in.
 */

export interface ProgressStatus {
  to: string
  from?: string
  /** waiting | checking | downloading | verifying | installing | restarting | done | failed */
  phase: string
  message?: string
  bytes_done?: number
  bytes_total?: number
  error?: string
  reboot?: boolean
  close_in?: number
}

const STEPS = ['Download', 'Verify', 'Install and restart'] as const

/** The step a phase is at (STEPS.length: all done). */
export function stepOf(phase: string): number {
  switch (phase) {
    case 'verifying':
      return 1
    case 'installing':
    case 'restarting':
      return 2
    case 'done':
      return STEPS.length
    default:
      return 0
  }
}

const mb = (b?: number) => Math.round((b ?? 0) / 2 ** 20)
const v = (s?: string) => (s ? `v${s.replace(/^v/, '')}` : '')

/** What happens now, one line. */
export function detailOf(p: ProgressStatus): string {
  switch (p.phase) {
    case 'waiting':
      return 'Starting the update…'
    case 'checking':
      return 'Asking the hub for the release…'
    case 'downloading':
      return (p.bytes_total ?? 0) > 0 ? `Downloading ${mb(p.bytes_done)} of ${Math.max(1, mb(p.bytes_total))} MB` : 'Downloading…'
    case 'verifying':
      return 'Checking the signature…'
    case 'installing':
      return 'Installing…'
    case 'restarting':
      return 'Installing - Longarm stops and starts again. This takes a minute or two.'
    case 'done':
      return p.reboot ? `${v(p.to)} is installed. Restart Windows to finish.` : `${v(p.to)} is installed and running.`
    case 'failed':
      return p.error || 'The update failed.'
    default:
      return ''
  }
}

async function close() {
  try {
    await fetch('/trayapi/progress/close', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })
  } catch {
    /* the window goes anyway */
  }
}

export function UpdateProgressApp() {
  const q = new URLSearchParams(location.search)
  const [p, setP] = useState<ProgressStatus>({ to: q.get('to') ?? '', phase: 'waiting' })
  useEffect(() => {
    let stop = false
    const poll = async () => {
      try {
        const r = await fetch('/trayapi/progress')
        if (r.ok && !stop) setP(await r.json())
      } catch {
        /* the next poll */
      }
      if (!stop) setTimeout(poll, 1000)
    }
    poll()
    return () => {
      stop = true
    }
  }, [])
  return <UpdateProgressView p={p} onClose={close} />
}

export function UpdateProgressView({ p, onClose }: { p: ProgressStatus; onClose: () => void }) {
  const step = stepOf(p.phase)
  const done = p.phase === 'done'
  const failed = p.phase === 'failed'
  const total = p.bytes_total ?? 0
  const pct = done ? 100 : p.phase === 'downloading' && total > 0 ? Math.min(100, (100 * (p.bytes_done ?? 0)) / total) : -1
  const title = done ? 'Longarm is up to date' : failed ? 'The update did not finish' : 'Updating Longarm'
  return (
    <main className="upw" data-testid="update-progress" data-phase={p.phase}>
      <header>
        <h1>{title}</h1>
        <span className="upw-ver" data-testid="update-versions">
          {p.from && !sameVersion(p.from, p.to) ? `${v(p.from)} → ${v(p.to)}` : v(p.to)}
        </span>
      </header>
      <ol className="upw-steps" aria-label="steps">
        {STEPS.map((s, i) => {
          const state = failed && i === step ? 'failed' : i < step ? 'done' : i === step ? 'now' : 'next'
          return (
            <li key={s} data-state={state} aria-current={state === 'now' ? 'step' : undefined}>
              <span className="upw-mark" aria-hidden="true">
                {state === 'done' ? '✓' : state === 'failed' ? '!' : ''}
              </span>
              {s}
            </li>
          )
        })}
      </ol>
      <div
        className={`upw-bar${pct < 0 && !failed ? ' busy' : ''}`}
        role="progressbar"
        aria-label="update progress"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct >= 0 ? Math.round(pct) : undefined}
      >
        <span style={pct >= 0 ? { width: `${pct}%` } : undefined} />
      </div>
      <p className={`upw-detail${failed ? ' bad' : ''}`} data-testid="update-detail" role="status">
        {detailOf(p)}
      </p>
      <footer>
        <span className="upw-note">
          {done
            ? `This window closes in ${p.close_in ?? 0} s.`
            : failed
              ? 'Settings > Updates in the tray has the details.'
              : 'Jobs and sessions keep running; the tray comes back by itself.'}
        </span>
        <button className={done || failed ? 'primary' : ''} onClick={onClose}>
          {done || failed ? 'Close' : 'Hide'}
        </button>
      </footer>
    </main>
  )
}

function sameVersion(a: string, b: string) {
  return a.replace(/^v/, '') === b.replace(/^v/, '')
}
