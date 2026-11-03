import { useEffect, useRef, useState } from 'react'
import { longarmApi, trayApi, type TrayInfo, type WorkerUpdateJob } from '../api/local'
import type { PoolView, PoolWorker } from '../api/types'
import { ask } from '../components/confirm'
import { ErrorBox, useAction } from '../components/common'
import { OfflineRow, WorkerCard, type CardActions } from '../components/WorkerCard'
import { docsURL, repoURL } from '../lib/feedback'
import { updateRunning } from '../lib/update'
import { fmtAgo, since } from '../lib/format'
import { poolMachines } from '../lib/machines'
import { IconPlus, IconServers, IconUser } from '../components/icons'
import { AddMachine } from './AddMachine'
import { transportLabel } from './ClientTab'
import { EmptyState, Skeleton } from './ui'

/** Opens how to add a machine (the docs' install steps) in the repository the hub points at. */
function openInstallDocs() {
  const open = (repo?: string) => trayApi.open(docsURL(repoURL(repo), 'usage.md', 'install')).catch(() => {})
  trayApi.update().then(
    (u) => open(u.repo_url),
    () => open(),
  )
}

/** Whether this client's certificate is an admin's (longarm admin ...). */
export function isAdminClient(pool?: PoolView): boolean {
  return (pool?.identity ?? '').startsWith('admin/')
}

/**
 * The pool's source line ("hub unavailable ... direct connections still
 * work"), in the fixed header - only when something is off: the hub
 * unreachable or no live data. All well, the title bar's count says
 * "hub ✓" and its tooltip where the data comes from.
 */
export function SourceBar({ pool, now }: { pool: PoolView; now: number }) {
  const hub = pool.hub
  const src = pool.source ?? 'none'
  const hubDown = !!hub?.address && !hub.connected
  if (!hubDown && (src !== 'none' || !(pool.workers ?? []).length)) return null
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
      <span className="srcbar-chip" title="Connection to workers (Settings)">
        Connection: {transportLabel(pool.transport)}
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
    const started = Date.now()
    const j = await longarmApi.updateWorker(name, version)
    setJobs((m) => ({ ...m, [name]: { ...j, started, to: version } }))
    clearInterval(timers.current[name])
    timers.current[name] = setInterval(async () => {
      try {
        const s = await longarmApi.workerUpdate(name)
        setJobs((m) => ({ ...m, [name]: { ...s, started, to: version } }))
        if (s.state !== 'running') clearInterval(timers.current[name])
      } catch {
        /* the daemon restarts too when this machine updates: keep polling */
      }
    }, 2000)
  }
  return [jobs, start]
}

/**
 * The confirmation of "Update": what happens to the machine (Longarm restarts
 * for a minute or two; its admin worker too) and to the jobs and sessions
 * on it (they keep running), or that an update runs already.
 */
export function updateConfirmText(name: string, version: string, w?: PoolWorker, adminWorker?: string): string {
  const u = w?.node?.update
  if (updateRunning(u)) return `${name} is already updating to ${u?.to}. Start another update anyway? (the worker refuses it while one runs)`
  const companion = adminWorker ? ` Its admin worker ${adminWorker} restarts too.` : ''
  return `Update ${name} to ${version}?\n\nLongarm on the machine restarts for a minute or two; running jobs and sessions keep running.${companion}`
}

export function PoolTab({ pool, error, info, onNavigate }: { pool?: PoolView; error?: string; info?: TrayInfo; onNavigate?: (tab: string) => void }) {
  const { run } = useAction()
  const now = useNow()
  const [updates, startUpdate] = useWorkerUpdates()
  const [adding, setAdding] = useState(false)
  if (adding) return <AddMachine onClose={() => setAdding(false)} />
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
    desktop: (name) => run(() => trayApi.desktop(name), `opening the desktop of ${name}`),
    // drain needs an admin certificate (the hub's Admin API)
    drain: hubUp && admin ? (name, on) => run(() => longarmApi.drainWorker(name, on), `${name} ${on ? 'drained' : 'resumed'}`) : undefined,
    remove: hubUp
      ? async (names) => {
          const q = `Remove ${names.join(' and ')} from the hub? This revokes ${names.length > 1 ? 'their' : 'its'} certificates; the machine must sign in again to come back.`
          if (!(await ask(q, { ok: 'Remove', danger: true }))) return
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
    update: hubUp
      ? async (name, version) => {
          const w = (pool.workers ?? []).find((x) => x.name === name)
          const m = [...active, ...offline].find((x) => x.session?.name === name)
          if (!(await ask(updateConfirmText(name, version, w, m?.admin?.name), { ok: 'Update' }))) return
          run(() => startUpdate(name, version), `updating ${name} to ${version}`)
        }
      : undefined,
    canUpdate: (w: PoolWorker) => !!me && !!w.node?.owner && w.node.owner === me,
    updates,
    sessions: (name, agent) => run(() => trayApi.openSessions({ worker: name, agent })),
  }
  const empty = (pool.workers ?? []).length === 0
  return (
    <div>
      <ErrorBox error={error} />
      {empty && <EmptyPool needSignIn={!!info?.account?.needed} onNavigate={onNavigate} onAdd={() => setAdding(true)} />}
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
            <span className="summary-title">Offline ({offline.length})</span> <span className="muted small">Not seen for over an hour</span>
          </summary>
          {offline.map((m) => (
            <OfflineRow key={m.key} m={m} actions={actions} />
          ))}
        </details>
      )}
      {!empty && (
        <button className="fab" aria-label="Add a machine" title="Add a machine" data-testid="add-machine-fab" onClick={() => setAdding(true)}>
          <IconPlus size={20} />
        </button>
      )}
      {!empty && <div className="fab-spacer" aria-hidden="true" />}
    </div>
  )
}

function EmptyPool({ needSignIn, onNavigate, onAdd }: { needSignIn: boolean; onNavigate?: (tab: string) => void; onAdd: () => void }) {
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
      action={
        <>
          <button className="primary" data-testid="empty-add-machine" onClick={onAdd}>
            Add a machine
          </button>{' '}
          <button data-testid="install-docs" onClick={openInstallDocs}>
            How to install
          </button>
        </>
      }
    >
      Install Longarm on a Dev Box (or any machine) and sign in there.
    </EmptyState>
  )
}
