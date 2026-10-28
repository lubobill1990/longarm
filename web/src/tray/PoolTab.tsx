import { useEffect, useState } from 'react'
import { longarmApi, trayApi, type TrayInfo } from '../api/local'
import type { PoolView, PoolWorker } from '../api/types'
import { ErrorBox, useAction } from '../components/common'
import { OfflineRow, WorkerCard, type CardActions } from '../components/WorkerCard'
import { fmtAgo, since } from '../lib/format'
import { poolMachines } from '../lib/machines'

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
      <span>·</span>
      <span>{hub?.address ? (hub.connected ? 'hub connected' : `hub down ${hub.since ? fmtAgo(since(hub.since, now)) : ''}`) : 'no hub'}</span>
      <span className="spacer" />
      <span title="transport policy (Settings)">transport {pool.transport}</span>
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
export function PoolTab({ pool, error, info, onNavigate }: { pool?: PoolView; error?: string; info?: TrayInfo; onNavigate?: (tab: string) => void }) {
  const { run } = useAction()
  const now = useNow()
  if (!pool) return <div className="notice">{error ? <ErrorBox error={error} /> : 'connecting to longarm daemon…'}</div>
  const { active, offline } = poolMachines(pool.workers ?? [], now)
  const hubUp = !!pool.hub?.address
  const admin = isAdminClient(pool)
  const me = pool.user || info?.account?.user || ''
  const actions: CardActions = {
    attach: (name) => run(() => trayApi.attach(name), `opening a terminal: longarm attach ${name}`),
    ssh: (name) => run(() => trayApi.ssh(name), `opening ssh longarm-${name}`),
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
  }
  const empty = (pool.workers ?? []).length === 0
  return (
    <div>
      <ErrorBox error={error} />
      {empty && <EmptyPool needSignIn={!!info?.account?.needed} onNavigate={onNavigate} />}
      {active.map((m) => (
        <WorkerCard key={m.key} m={m} now={now} actions={actions} />
      ))}
      {offline.length > 0 && (
        <details className="offline-section" data-testid="offline-section">
          <summary>
            Offline ({offline.length}) <span className="muted small">not seen for over an hour</span>
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
      <div className="notice" data-testid="empty-pool">
        Not signed in yet: sign in on the Account tab to see your workers.{' '}
        {onNavigate && (
          <button className="link" onClick={() => onNavigate('account')}>
            Account tab
          </button>
        )}
      </div>
    )
  return (
    <div className="notice" data-testid="empty-pool">
      No workers yet: install Longarm on a Dev Box (or any machine) and sign in there.{' '}
      <button className="link" onClick={() => trayApi.open(INSTALL_URL)}>
        How to install
      </button>
    </div>
  )
}
