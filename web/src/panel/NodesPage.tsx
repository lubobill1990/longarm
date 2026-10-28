import { useEffect, useMemo, useState } from 'react'
import { hubApi } from '../api/hub'
import type { LinkReport, MetricsPoint, NodeInfo, Profile } from '../api/types'
import { MiniBar, Sparkline } from '../components/charts'
import { ErrorBox, StatePill, useAction, usePoll } from '../components/common'
import { VersionTag } from '../components/VersionTag'
import { AMBER, CPU_C, fmtAgo, fmtRTT, gb, GREEN, isRelayed, MEM_C, memPct, RED, since } from '../lib/format'
import { nodeMachines } from '../lib/machines'

const HIST = 150

/** Live node list: SSE node + metrics events on top of /api/nodes. */
export function useLiveNodes() {
  const [nodes, setNodes] = useState<Record<string, NodeInfo>>({})
  const [hist, setHist] = useState<Record<string, MetricsPoint[]>>({})
  const [error, setError] = useState<string>()
  const [tick, setTick] = useState(0)
  const reload = () => setTick((t) => t + 1)
  useEffect(() => {
    let alive = true
    hubApi
      .nodes(true)
      .then((ns) => alive && setNodes(Object.fromEntries(ns.map((n) => [n.name, n]))))
      .catch((e) => setError(String(e.message ?? e)))
    hubApi
      .history()
      .then((hs) => alive && setHist(Object.fromEntries(hs.map((h) => [h.node, h.points ?? []]))))
      .catch(() => {})
    const s = hubApi.events(['node', 'metrics'], (ev) => {
      const n = ev.node_info
      if (!n) return
      setNodes((old) => ({ ...old, [n.name]: n }))
      if (ev.kind === 'metrics' && n.metrics) {
        const m = n.metrics
        const p: MetricsPoint = { time: m.time, cpu: m.cpu_percent ?? 0, mem: memPct(m) }
        setHist((old) => ({ ...old, [n.name]: [...(old[n.name] ?? []), p].slice(-HIST) }))
      }
    })
    return () => {
      alive = false
      s.close()
    }
  }, [tick])
  return { nodes, hist, error, reload }
}

/** admin=false: a user-role login sees the workers read-only (no links, toolchains or actions). */
export function NodesPage({ admin = true }: { admin?: boolean }) {
  const { nodes, hist, error, reload } = useLiveNodes()
  const links = usePoll(() => (admin ? hubApi.links() : Promise.resolve([])), admin ? 10000 : 0, [admin])
  const profiles = usePoll(() => (admin ? hubApi.profiles() : Promise.resolve([])), admin ? 30000 : 0, [admin])
  // the newest complete release: outdated versions are marked
  const latest = usePoll(() => hubApi.latestRelease().catch(() => undefined), 600000)
  const list = useMemo(() => Object.values(nodes).sort((a, b) => a.name.localeCompare(b.name)), [nodes])
  const pending = list.filter((n) => n.state === 'pending')
  const workers = list.filter((n) => n.role === 'worker' && n.state !== 'pending')
  // one machine together: a session worker, then its admin worker; online machines first
  const machines = nodeMachines(workers)
  const others = list.filter((n) => n.role !== 'worker' && n.state !== 'pending')
  const online = workers.filter((n) => n.online && n.state === 'approved').length
  return (
    <div>
      <div className="page-title">
        <h1>Nodes</h1>
        <span className="muted">
          {online}/{workers.filter((n) => n.state === 'approved').length} workers online
        </span>
        <span className="spacer" />
        <button onClick={reload}>Refresh</button>
      </div>
      <ErrorBox error={error} />
      {admin && pending.length > 0 && (
        <div className="card" style={{ marginBottom: 12, borderColor: '#9e6a03' }} data-testid="pending">
          <h3 style={{ margin: '0 0 8px' }}>Waiting for approval</h3>
          <table className="grid">
            <tbody>
              {pending.map((n) => (
                <PendingRow key={n.name} n={n} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      <table className="grid" data-testid="nodes">
        <thead>
          <tr>
            <th>worker</th>
            <th>CPU (5 min)</th>
            <th>memory (5 min)</th>
            <th title="Claude Code / Copilot CLI sessions">CC / CP</th>
            <th>jobs</th>
            {admin && <th>links (client → path, RTT)</th>}
            <th>labels</th>
            <th>toolchain</th>
            {admin && <th />}
          </tr>
        </thead>
        <tbody>
          {machines.flatMap((g) => {
            const main = g.session ?? g.admin!
            const rows = [
              <NodeRow key={main.name} n={main} adminWorker={g.session ? g.admin : undefined} hist={hist[main.name] ?? []} links={links.data ?? []} profiles={profiles.data ?? []} admin={admin} latest={latest.data?.version} />,
            ]
            if (g.session && g.admin)
              rows.push(<NodeRow key={g.admin.name} n={g.admin} sub hist={hist[g.admin.name] ?? []} links={links.data ?? []} profiles={profiles.data ?? []} admin={admin} latest={latest.data?.version} />)
            return rows
          })}
          {workers.length === 0 && (
            <tr>
              <td colSpan={admin ? 9 : 7} className="notice">
                {admin ? 'No workers yet: create a join code under “Add machine”.' : 'No workers.'}
              </td>
            </tr>
          )}
        </tbody>
      </table>
      {others.length > 0 && (
        <>
          <h3 className="muted" style={{ marginTop: 24 }}>
            Clients and admins
          </h3>
          <table className="grid">
            <tbody>
              {others.map((n) => (
                <OtherRow key={n.name} n={n} admin={admin} latest={latest.data?.version} />
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}

function PendingRow({ n }: { n: NodeInfo }) {
  const { run, busy } = useAction()
  return (
    <tr data-testid={`pending-${n.name}`}>
      <td>
        <b>{n.name}</b> <span className="muted">{n.role}</span>
      </td>
      <td className="muted">
        {n.hostname} {n.os}/{n.arch} {n.version}
      </td>
      <td className="muted">joined {fmtAgo(since(n.joined_at))} ago</td>
      <td style={{ textAlign: 'right' }}>
        <button className="primary" disabled={busy} onClick={() => run(() => hubApi.approve(n.name), `${n.name} approved`)}>
          Approve
        </button>{' '}
        <button className="danger" disabled={busy} onClick={() => confirm(`Reject ${n.name}?`) && run(() => hubApi.revoke(n.name), `${n.name} rejected`)}>
          Reject
        </button>
      </td>
    </tr>
  )
}

function NodeRow({
  n,
  hist,
  links,
  profiles,
  admin,
  adminWorker,
  sub,
  latest,
}: {
  n: NodeInfo
  hist: MetricsPoint[]
  links: LinkReport[]
  profiles: Profile[]
  admin: boolean
  /** the machine's admin worker (shown as a capability of this row) */
  adminWorker?: NodeInfo
  /** this is the admin worker of the row above (same machine) */
  sub?: boolean
  latest?: string
}) {
  const { run, busy } = useAction()
  const m = n.online ? n.metrics : undefined
  const revoked = n.state === 'revoked'
  const pairs = links.flatMap((r) => (r.links ?? []).filter((l) => l.worker === n.name).map((l) => ({ client: r.client, l })))
  const editLabels = () => {
    const cur = Object.entries(n.labels ?? {})
      .filter(([k]) => k !== 'os' && k !== 'arch')
      .map(([k, v]) => `${k}=${v}`)
      .join(' ')
    const s = prompt('Labels (k=v, space separated; k= deletes):', cur)
    if (s === null) return
    const next: Record<string, string> = {}
    for (const k of Object.keys(n.labels ?? {})) if (k !== 'os' && k !== 'arch') next[k] = ''
    for (const kv of s.split(/\s+/).filter(Boolean)) {
      const i = kv.indexOf('=')
      if (i > 0) next[kv.slice(0, i)] = kv.slice(i + 1)
    }
    run(() => hubApi.setLabels(n.name, next), 'labels saved')
  }
  return (
    <tr data-testid={`node-${n.name}`} className={sub ? 'subrow' : undefined} style={revoked ? { opacity: 0.5 } : undefined}>
      <td className="nowrap">
        <div className="row" style={sub ? { paddingLeft: 14 } : undefined}>
          {sub && <span className="faint" title="the admin worker of the machine above">↳</span>}
          <span className="dot" style={{ background: revoked ? RED : n.online ? GREEN : RED }} />
          <b>{n.name}</b>
          {adminWorker && (
            <span
              className="pill info"
              data-testid={`admin-cap-${n.name}`}
              title={`this machine also runs an admin worker, ${adminWorker.name}: commands as SYSTEM, its owner only, named explicitly`}
            >
              admin commands: {adminWorker.state === 'revoked' ? 'revoked' : adminWorker.online ? 'online' : 'offline'}
            </span>
          )}
          {n.admin_worker && (
            <span className="pill warn" data-testid={`admin-${n.name}`} title={`admin worker: runs commands as SYSTEM on its machine; only ${n.owner ?? 'its owner'} can use it, never shared or placed automatically`}>
              admin
            </span>
          )}
          {n.draining && (
            <span className="pill warn" title={n.drain_reason}>
              draining
            </span>
          )}
          {revoked && <StatePill state="revoked" />}
        </div>
        <div className="muted small">
          {n.online ? 'online' : `offline ${fmtAgo(since(n.last_seen))}`} · {n.os}/{n.arch} ·{' '}
          <VersionTag version={n.version} latest={latest} ua={n.update_available} testId={`outdated-${n.name}`} />
          {n.isolation === 'per-user' && <span title="jobs run under per-user OS accounts"> · per-user</span>}
        </div>
      </td>
      <td style={{ width: 170 }}>
        <Sparkline series={[{ values: hist.map((p) => p.cpu ?? 0), color: CPU_C }]} slots={HIST} height={28} />
        <div className="small mono" data-testid="cpu">
          {m ? `${(m.cpu_percent ?? 0).toFixed(0)}%` : '—'}
        </div>
      </td>
      <td style={{ width: 170 }}>
        <Sparkline series={[{ values: hist.map((p) => p.mem ?? 0), color: MEM_C }]} slots={HIST} height={28} />
        <div className="small mono">
          {m ? (
            <>
              <MiniBar pct={memPct(m)} /> {gb(m.mem_used_bytes).toFixed(1)}/{gb(m.mem_total_bytes).toFixed(0)} GB
            </>
          ) : (
            '—'
          )}
        </div>
      </td>
      <td className="mono nowrap">
        <span style={{ color: 'var(--claude)' }}>{m?.agents?.claude?.sessions ?? 0}</span> /{' '}
        <span style={{ color: 'var(--copilot)' }}>{m?.agents?.copilot?.sessions ?? 0}</span>
      </td>
      <td className="mono">
        {n.jobs_active ?? 0}
        {n.max_jobs ? `/${n.max_jobs}` : ''}
        {(n.reserved_gb ?? 0) > 0 && <div className="muted small">{(n.reserved_gb ?? 0).toFixed(0)} GB res.</div>}
      </td>
      {admin && (
        <td className="small">
          {pairs.length === 0 && <span className="faint">no client reports</span>}
          {pairs.map(({ client, l }) => {
            const act = (l.legs ?? []).find((x) => x.active)
            const color = l.state !== 'online' ? RED : act && isRelayed(act.kind) ? AMBER : GREEN
            return (
              <div key={client} className="nowrap">
                {client}: <span style={{ color }}>{l.active || l.state}</span> <span className="mono">{fmtRTT(l.rtt_ms)}</span>
                {act?.nat && <span className="faint"> nat {act.nat}</span>}
              </div>
            )
          })}
        </td>
      )}
      <td className="small">
        {Object.entries(n.labels ?? {})
          .filter(([k]) => k !== 'os' && k !== 'arch')
          .map(([k, v]) => (
            <span key={k} className="pill" style={{ marginRight: 3 }}>
              {k}={v}
            </span>
          ))}
        {admin && !revoked && (
          <button className="link small" onClick={editLabels}>
            edit
          </button>
        )}
      </td>
      <td className="small">
        {admin ? (
          <select
            aria-label={`profile of ${n.name}`}
            value={n.profile ?? ''}
            disabled={revoked || busy}
            onChange={(e) => run(() => hubApi.assignProfile(n.name, e.target.value), 'profile assigned')}
          >
            <option value="">(none)</option>
            {profiles.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
        ) : (
          n.profile
        )}
        {n.toolchain && n.toolchain.state !== 'none' && (
          <div title={n.toolchain.error}>
            r{n.toolchain.revision} <StatePill state={n.toolchain.state} />
          </div>
        )}
      </td>
      {admin && (
        <td className="nowrap" style={{ textAlign: 'right' }}>
          {!revoked && (
            <>
              <button disabled={busy} onClick={() => run(() => hubApi.drain(n.name, !n.admin_drain), n.admin_drain ? 'resumed' : 'drained')}>
                {n.admin_drain ? 'Resume' : 'Drain'}
              </button>{' '}
              <button disabled={busy || !n.online} title="reconcile the toolchain profile now" onClick={() => run(() => hubApi.toolchainApply(n.name), 'toolchain apply requested')}>
                Update
              </button>{' '}
              <button className="danger" disabled={busy} onClick={() => confirm(`Revoke ${n.name}? Its certificates stop working everywhere.`) && run(() => hubApi.revoke(n.name), `${n.name} revoked`)}>
                Revoke
              </button>
            </>
          )}
        </td>
      )}
    </tr>
  )
}

function OtherRow({ n, admin, latest }: { n: NodeInfo; admin: boolean; latest?: string }) {
  const { run, busy } = useAction()
  return (
    <tr>
      <td>
        <b>{n.name}</b> <span className="muted">{n.role}</span>
        {n.user && n.user !== n.name && <span className="muted small"> · user {n.user}</span>}
        {n.version && (
          <span className="muted small">
            {' '}
            · <VersionTag version={n.version} latest={latest} testId={`outdated-${n.name}`} />
          </span>
        )}
      </td>
      <td>
        <StatePill state={n.state} />
      </td>
      <td className="muted small">last seen {n.last_seen ? `${fmtAgo(since(n.last_seen))} ago` : '—'}</td>
      <td style={{ textAlign: 'right' }}>
        {admin && n.state !== 'revoked' && (
          <button className="danger" disabled={busy} onClick={() => confirm(`Revoke ${n.name}?`) && run(() => hubApi.revoke(n.name), `${n.name} revoked`)}>
            Revoke
          </button>
        )}
      </td>
    </tr>
  )
}
