import { useEffect, useRef, useState } from 'react'
import { longarmApi, trayApi, type TrayInfo, type WorkerUpdateJob } from '../api/local'
import type { PoolView, PoolWorker } from '../api/types'
import { ErrorBox, useAction } from '../components/common'
import { OfflineRow, WorkerCard, type CardActions } from '../components/WorkerCard'
import { fmtAgo, since } from '../lib/format'
import { poolMachines } from '../lib/machines'
import { IconServers, IconUser } from '../components/icons'
import { EmptyState, Skeleton } from './ui'

/** Where to read how to add a machine (the README's install steps). */
export const INSTALL_URL = 'https://github.com/lubobill1990/longarm/blob/main/docs/usage.md#install'

/** Whether this client's certificate is an admin's (longarm admin ...). */
export function isAdminClient(pool?: PoolView): boolean {
  return (pool?.identity ?? '').startsWith('admin/')
}

/** The pool's source line ("live from the hub · hub connected"): in the fixed header. */
export function SourceBar({ pool, now }: { pool: PoolView; now: number }) {
  const hub = pool.hub
  const src = pool.source ?? 'none'
  const srcText =
    src === 'hub' ? 'live from the hub' : src === 'direct' ? 'live from the workers (direct)' : src === 'mixed' ? 'hub + direct' : 'no live data'
  return (
    <div className="srcbar" data-testid="source">
      <span className="dot" style={{ width: 8, height: 8, background: src === 'none' ? 'var(--red)' : src === 'hub' ? 'var(--green)' : 'var(--amber)' }} />
      <span data-testid="source-text">{srcText}</span>
      <span className="sep" aria-hidden="true">·</span>
      <span data-testid="hub-state" title={hub?.address && !hub.connected ? 'Workers are reached directly (cached addresses); placement, sign-in and new links wait for the hub' : undefined}>
        {hub?.address
          ? hub.connected
            ? 'hub connected'
            : `hub unavailable${hub.since ? ' ' + fmtAgo(since(hub.since, now)) : ''} · direct connections still work`
          : 'no hub'}
      </span>
      <span className="spacer" />
      <span className="srcbar-chip" title="transport policy (Settings)">
        transport {pool.transport}
      </span>
    </div>
  )
}

/** One-second clock for "online 5m" texts. */
export function useNow(): number {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])
  return now
}

/**
 * The pool panel: one card per machine (a session worker and its admin
 * worker together), online first, live every 2 s; machines offline for
 * more than an hour in a collapsed "Offline (n)" section, one line each.
 */
/**
 * Updates of machines the tray started (longarm worker update through the
 * daemon), polled every 2 s while they run.
 */
export function useWorkerUpdates(): [Record<string, WorkerUpdateJob>, (name: string, version: string) => Promise<void>] {
  const [jobs, setJobs] = useState<Record<string, WorkerUpdateJob>>({})
  const timers = useRef<Record<string, ReturnType<typeof setInterval>>>({})
  useEffect(() => () => Object.values(timers.current).forEach(clearInterval), [])
  const start = async (name: string, version: string) => {
    const j = await longarmApi.updateWorker(name, version)
    setJobs((m) => ({ ...m, [name]: j }))
    clearInterval(timers.current[name])
    timers.current[name] = setInterval(async () => {
      try {
        const s = await longarmApi.workerUpdate(name)
        setJobs((m) => ({ ...m, [name]: s }))
        if (s.state !== 'running') clearInterval(timers.current[name])
      } catch {
        /* the daemon restarts too when this machine updates: keep polling */
      }
    }, 2000)
  }
  return [jobs, start]
}

export function PoolTab({ pool, error, info, onNavigate }: { pool?: PoolView; error?: string; info?: TrayInfo; onNavigate?: (tab: string) => void }) {
  const { run } = useAction()
  const now = useNow()
  const [updates, startUpdate] = useWorkerUpdates()
  if (!pool)
    return error ? (
      <ErrorBox error={error} />
    ) : (
      <div className="cards" data-testid="pool-loading">
        <Skeleton card lines={4} label="connecting to longarm daemon…" />
        <Skeleton card lines={4} label="" />
      </div>
    )
  const { active, offline } = poolMachines(pool.workers ?? [], now)
  const hubUp = !!pool.hub?.address
  const admin = isAdminClient(pool)
  const me = pool.user || info?.account?.user || ''
  const actions: CardActions = {
    attach: (name) => run(() => trayApi.attach(name), `opening a terminal: longarm attach ${name}`),
    ssh: (name) => run(() => trayApi.ssh(name), `opening longarm ssh ${name}`),
    panel: info?.hub_url ? (name) => run(() => trayApi.openPanel(`/nodes#${name}`)) : undefined,
    // drain needs an admin certificate (the hub's Admin API)
    drain: hubUp && admin ? (name, on) => run(() => longarmApi.drainWorker(name, on), `${name} ${on ? 'drained' : 'resumed'}`) : undefined,
    remove: hubUp
      ? (names) => {
          if (!confirm(`Remove ${names.join(' and ')} from the hub? This revokes ${names.length > 1 ? 'their' : 'its'} certificates; the machine must sign in again to come back.`)) return
          run(
            async () => {
              for (const n of names) await longarmApi.removeWorker(n)
            },
            `removed ${names.join(', ')}`,
          )
        }
      : undefined,
    canRemove: (w: PoolWorker) => admin || (!!me && !!w.node?.owner && w.node.owner === me),
    // the owner only (an admin certificate is no exception): longarm worker update
    update: hubUp ? (name, version) => run(() => startUpdate(name, version), `updating ${name} to ${version}`) : undefined,
    canUpdate: (w: PoolWorker) => !!me && !!w.node?.owner && w.node.owner === me,
    updates,
  }
  const empty = (pool.workers ?? []).length === 0
  return (
    <div>
      <ErrorBox error={error} />
      {empty && <EmptyPool needSignIn={!!info?.account?.needed} onNavigate={onNavigate} />}
      {active.length > 0 && (
        <div className="cards">
          {active.map((m) => (
            <WorkerCard key={m.key} m={m} now={now} actions={actions} />
          ))}
        </div>
      )}
      {offline.length > 0 && (
        <details className="offline-section" data-testid="offline-section">
          <summary>
            <span className="summary-title">Offline ({offline.length})</span> <span className="muted small">not seen for over an hour</span>
          </summary>
          {offline.map((m) => (
            <OfflineRow key={m.key} m={m} actions={actions} />
          ))}
        </details>
      )}
    </div>
  )
}

function EmptyPool({ needSignIn, onNavigate }: { needSignIn: boolean; onNavigate?: (tab: string) => void }) {
  if (needSignIn)
    return (
      <EmptyState
        testId="empty-pool"
        icon={<IconUser size={20} />}
        title="Not signed in yet"
        action={
          onNavigate && (
            <button className="primary" onClick={() => onNavigate('account')}>
              Account tab
            </button>
          )
        }
      >
        Sign in on the Account tab to see your workers.
      </EmptyState>
    )
  return (
    <EmptyState
      testId="empty-pool"
      icon={<IconServers size={20} />}
      title="No workers yet"
      action={<button onClick={() => trayApi.open(INSTALL_URL)}>How to install</button>}
    >
      Install Longarm on a Dev Box (or any machine) and sign in there.
    </EmptyState>
  )
}
