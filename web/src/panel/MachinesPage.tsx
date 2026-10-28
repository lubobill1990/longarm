import { useMemo } from 'react'
import { hubApi } from '../api/hub'
import type { Me, NodeInfo } from '../api/types'
import { ErrorBox, StatePill, useAction, usePoll } from '../components/common'
import { VersionTag } from '../components/VersionTag'
import { fmtAgo, GREEN, RED, since } from '../lib/format'
import { ADMIN_LABEL, nodeMachines, type Group } from '../lib/machines'
import { Link } from '../lib/router'

/** The labels a user may edit (os/arch come from the worker). */
const ownLabels = (n: NodeInfo) => Object.entries(n.labels ?? {}).filter(([k]) => k !== 'os' && k !== 'arch' && k !== ADMIN_LABEL && !k.startsWith('longarm.io/'))

/** A machine: its session worker and admin worker (lib/machines, as the tray and Nodes), and its devices. */
interface Machine extends Group<NodeInfo> {
  devices: NodeInfo[]
}

/** The login's own workers grouped per machine; a device named like a worker's machine or host joins it. */
export function myMachines(nodes: NodeInfo[], user: string): Machine[] {
  const live = nodes.filter((n) => n.state !== 'revoked')
  const workers = live.filter((n) => n.role === 'worker' && n.owner === user)
  const devices = live.filter((n) => n.role !== 'worker' && (n.user === user || (!n.user && n.name === user)))
  const out: Machine[] = nodeMachines(workers).map((g) => ({ ...g, devices: [] }))
  for (const d of devices) {
    const k = d.name.toLowerCase()
    const m = out.find((g) => [g.session, g.admin].some((w) => !!w && (w.name.toLowerCase() === k || (w.hostname ?? '').split('.')[0].toLowerCase() === k)))
    if (m) m.devices.push(d)
    else out.push({ key: d.name, devices: [d] })
  }
  return out
}

/**
 * "My machines": the login's own workers and devices, one row per machine
 * (worker + admin worker + device), with sharing, labels and removal.
 */
export function MachinesPage({ me }: { me: Me }) {
  const user = me.tenant || me.user
  const nodes = usePoll(() => hubApi.nodes(true), 15000)
  const latest = usePoll(() => hubApi.latestRelease().catch(() => undefined), 600000)
  const machines = useMemo(() => myMachines(nodes.data ?? [], user), [nodes.data, user])
  return (
    <div>
      <div className="page-title">
        <h1>My machines</h1>
        <span className="spacer" />
        <button onClick={nodes.reload}>Refresh</button>
      </div>
      <ErrorBox error={nodes.error} />
      {nodes.data && machines.length === 0 && (
        <div className="notice" data-testid="machines-empty">
          No machines yet. <Link to="/get">Get Longarm</Link> and sign in on each machine you want to use.
        </div>
      )}
      {machines.map((m) => (
        <MachineCard key={m.key} m={m} latest={latest.data?.version} onChange={nodes.reload} />
      ))}
    </div>
  )
}

function MachineCard({ m, latest, onChange }: { m: Machine; latest?: string; onChange: () => void }) {
  const main = m.session ?? m.admin ?? m.devices[0]
  if (!main) return null
  const title = main.hostname || main.name
  return (
    <div className="card" style={{ marginBottom: 12 }} data-testid={`machine-${m.key}`}>
      <h3 style={{ margin: '0 0 8px' }}>{title}</h3>
      <table className="grid">
        <tbody>
          {m.session && <WorkerRow n={m.session} latest={latest} onChange={onChange} />}
          {m.admin && <WorkerRow n={m.admin} latest={latest} onChange={onChange} />}
          {m.devices.map((d) => (
            <tr key={d.name} data-testid={`device-${d.name}`}>
              <td>
                <b>{d.name}</b> <span className="muted">device ({d.role})</span>
              </td>
              <td className="muted small">last seen {d.last_seen ? `${fmtAgo(since(d.last_seen))} ago` : '—'}</td>
              <td className="small">{d.version && <VersionTag version={d.version} latest={latest} />}</td>
              <td />
              <td />
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function WorkerRow({ n, latest, onChange }: { n: NodeInfo; latest?: string; onChange: () => void }) {
  const { run, busy } = useAction()
  const shares = n.shares ?? {}
  const shared = [...(shares.users ?? []), ...(shares.teams ?? []).map((t) => `team ${t}`)]
  const done = <T,>(p: Promise<T>) => p.then((r) => (onChange(), r))
  const editLabels = () => {
    const cur = ownLabels(n)
      .map(([k, v]) => `${k}=${v}`)
      .join(' ')
    const s = prompt('Labels (k=v, space separated; delete one by leaving it out):', cur)
    if (s === null) return
    const next: Record<string, string> = {}
    for (const [k] of ownLabels(n)) next[k] = ''
    for (const kv of s.split(/\s+/).filter(Boolean)) {
      const i = kv.indexOf('=')
      if (i > 0) next[kv.slice(0, i)] = kv.slice(i + 1)
    }
    if (Object.keys(next).length === 0) return
    run(() => done(hubApi.setWorkerLabels(n.name, next)), 'labels saved')
  }
  const share = () => {
    const s = prompt(`Share ${n.name} with (user names; "team:NAME" for a team), space separated:`, '')
    if (!s) return
    const words = s.split(/[\s,]+/).filter(Boolean)
    const add_teams = words.filter((w) => w.startsWith('team:')).map((w) => w.slice(5))
    const add_users = words.filter((w) => !w.startsWith('team:'))
    run(() => done(hubApi.shareWorker(n.name, { add_users, add_teams })), `${n.name} shared`)
  }
  const unshare = (who: string) => {
    const req = who.startsWith('team ') ? { remove_teams: [who.slice(5)] } : { remove_users: [who] }
    run(() => done(hubApi.shareWorker(n.name, req)), `${who} removed`)
  }
  return (
    <tr data-testid={`worker-${n.name}`}>
      <td className="nowrap">
        <div className="row">
          <span className="dot" style={{ background: n.online ? GREEN : RED }} />
          <b>{n.name}</b>
          {n.admin_worker ? (
            <span className="pill warn" title="admin worker: runs commands as SYSTEM on this machine; only you can use it, never shared">
              admin
            </span>
          ) : (
            <span className="muted small">worker</span>
          )}
          {n.draining && (
            <span className="pill warn" title={n.drain_reason}>
              draining
            </span>
          )}
        </div>
        <div className="muted small">
          {n.online ? 'online' : `offline ${fmtAgo(since(n.last_seen))}`} · {n.os}/{n.arch}
        </div>
      </td>
      <td className="small">
        <VersionTag version={n.version} latest={latest} ua={n.update_available} testId={`outdated-${n.name}`} />
        {n.update_available?.reason && <div className="muted small">{n.update_available.reason}</div>}
      </td>
      <td className="small">
        {ownLabels(n).map(([k, v]) => (
          <span key={k} className="pill" style={{ marginRight: 3 }}>
            {k}={v}
          </span>
        ))}
        <button className="link small" disabled={busy} onClick={editLabels}>
          labels
        </button>
      </td>
      <td className="small" data-testid={`shares-${n.name}`}>
        {n.admin_worker ? (
          <span className="muted">only you</span>
        ) : (
          <>
            {shares.everyone && <StatePill state="everyone" />}
            {shared.length === 0 && !shares.everyone && <span className="muted">only you </span>}
            {shared.map((w) => (
              <span key={w} className="pill" style={{ marginRight: 3 }}>
                {w}{' '}
                <button className="link small" aria-label={`unshare ${w}`} disabled={busy} onClick={() => unshare(w)}>
                  ✕
                </button>
              </span>
            ))}
            <button className="link small" disabled={busy} onClick={share}>
              share
            </button>
          </>
        )}
      </td>
      <td style={{ textAlign: 'right' }}>
        <button
          className="danger"
          disabled={busy}
          onClick={() =>
            confirm(`Remove ${n.name}? Its certificates are revoked: it stops taking jobs until it signs in again.`) &&
            run(() => done(hubApi.removeWorker(n.name)), `${n.name} removed`)
          }
        >
          Remove
        </button>
      </td>
    </tr>
  )
}
