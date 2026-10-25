import { useEffect, useState } from 'react'
import { longarmApi, trayApi, type TrayInfo } from '../api/local'
import type { PoolView } from '../api/types'
import { ErrorBox, useAction } from '../components/common'
import { WorkerCard } from '../components/WorkerCard'
import { fmtAgo, since } from '../lib/format'

/** The pool panel: one card per worker, live every 2 s. */
export function PoolTab({ pool, error, info }: { pool?: PoolView; error?: string; info?: TrayInfo }) {
  const { run } = useAction()
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])
  if (!pool) return <div className="notice">{error ? <ErrorBox error={error} /> : 'connecting to longarm daemon…'}</div>
  const hub = pool.hub
  const src = pool.source ?? 'none'
  const srcText =
    src === 'hub' ? 'live from the hub' : src === 'direct' ? 'live from the workers (direct)' : src === 'mixed' ? 'hub + direct' : 'no live data'
  return (
    <div>
      <div className="srcbar" data-testid="source">
        <span className="dot" style={{ width: 8, height: 8, background: src === 'none' ? 'var(--red)' : src === 'hub' ? 'var(--green)' : 'var(--amber)' }} />
        <span data-testid="source-text">{srcText}</span>
        <span>·</span>
        <span>
          {hub?.address
            ? hub.connected
              ? 'hub connected'
              : `hub down ${hub.since ? fmtAgo(since(hub.since, now)) : ''}`
            : 'no hub'}
        </span>
        <span className="spacer" />
        <span title="transport policy (Settings)">transport {pool.transport}</span>
      </div>
      <ErrorBox error={error} />
      {(pool.workers ?? []).length === 0 && <div className="notice">No workers known yet (join a hub or add workers to workers.toml).</div>}
      {(pool.workers ?? []).map((w) => (
        <WorkerCard
          key={w.name}
          w={w}
          now={now}
          actions={{
            attach: (name) => run(() => trayApi.attach(name), `opening a terminal: longarm attach ${name}`),
            ssh: (name) => run(() => trayApi.ssh(name), `opening ssh longarm-${name}`),
            panel: info?.hub_url ? (name) => run(() => trayApi.openPanel(`/nodes#${name}`)) : undefined,
            drain: pool.hub?.address ? (name, on) => run(() => longarmApi.drainWorker(name, on), `${name} ${on ? 'drained' : 'resumed'}`) : undefined,
          }}
        />
      ))}
    </div>
  )
}
