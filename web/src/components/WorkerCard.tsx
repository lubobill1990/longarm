import { useState } from 'react'
import type { LegStatus, MetricsSnapshot, PoolWorker, ProcGroup } from '../api/types'
import {
  AMBER,
  CPU_C,
  fmtAgo,
  fmtBytes,
  fmtRTT,
  fmtUptime,
  gb,
  GREEN,
  isRelayed,
  MEM_C,
  memPct,
  num,
  RED,
  RTT_C,
  since,
} from '../lib/format'
import { ADMIN_LABEL, fmtLastSeen, isAdminNode, offlineSeconds, type PoolMachine } from '../lib/machines'
import { Bar, Sparkline } from './charts'

const AGENTS: [string, string, string][] = [
  ['claude', 'Claude Code', 'var(--claude)'],
  ['copilot', 'Copilot CLI', 'var(--copilot)'],
]

export interface CardActions {
  attach?: (w: string) => void
  ssh?: (w: string) => void
  panel?: (w: string) => void
  /** longarm admin drain / undrain: admins only (leave it unset for others) */
  drain?: (w: string, on: boolean) => void
  /** remove (revoke) workers on the hub; the caller confirms */
  remove?: (names: string[]) => void
  /** whether this user may remove the worker (its owner, or an admin) */
  canRemove?: (w: PoolWorker) => boolean
}

const adminTip = (name: string) =>
  `Admin commands run as SYSTEM (a local administrator) on this machine. Only the owner can use them; ` +
  `never shared, never picked automatically: name the admin worker explicitly (${name}).`

/** A shield: the admin-commands capability. */
export function ShieldIcon({ size = 11 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" style={{ verticalAlign: '-1px' }}>
      <path d="M8 1 2.5 3v4.2c0 3.4 2.3 6.4 5.5 7.8 3.2-1.4 5.5-4.4 5.5-7.8V3L8 1z" fill="currentColor" />
    </svg>
  )
}

/** "admin commands: on": the machine has an admin worker. */
export function AdminBadge({ admin, compact }: { admin: PoolWorker; compact?: boolean }) {
  return (
    <span className="pill admin" data-testid="admin-badge" title={adminTip(admin.name)}>
      <ShieldIcon /> {compact ? 'admin' : 'admin commands: on'}
    </span>
  )
}

function stateOf(w?: PoolWorker): string {
  return w ? (w.state ?? 'connecting') : 'offline'
}

function statusText(w: PoolWorker, now: number): string {
  const st = stateOf(w)
  const s = since(w.since, now)
  if (st === 'online') return `online ${fmtAgo(s)}`
  if (st === 'offline') return `offline ${fmtAgo(s)}, reconnecting`
  return 'connecting…'
}

function capText(label: string, w: PoolWorker, now: number): string {
  const st = stateOf(w)
  if (st === 'offline') return `${label} offline ${fmtLastSeen(offlineSeconds(w, now))}`
  return `${label} ${st === 'online' ? 'online' : 'connecting'}`
}

/** A machine of one pool worker (no companion known). */
export function machineOf(w: PoolWorker): PoolMachine {
  const admin = isAdminNode(w.node)
  return {
    key: w.name,
    session: admin ? undefined : w,
    admin: admin ? w : undefined,
    primary: w,
    state: stateOf(w),
    offlineFor: 0,
    longOffline: false,
    labels: Object.entries(w.node?.labels ?? {}).filter(([k]) => k !== 'os' && k !== 'arch' && k !== ADMIN_LABEL),
  }
}

/**
 * One machine in a dark card layout: its session worker (and, if it has
 * one, its admin worker as the "admin commands" capability), status line,
 * labels, then this client's link (active leg, path, NAT, RTT + 5-min RTT
 * sparkline), CPU and memory bars with the 5-min sparkline, Claude /
 * Copilot sessions, info line and the top processes by CPU and memory.
 * attach / ssh go to the session worker; admin actions name the admin
 * worker explicitly (the ⋯ menu). Pass a machine (m) or a lone worker (w).
 */
export function WorkerCard({ w, m, actions, now = Date.now() }: { w?: PoolWorker; m?: PoolMachine; actions?: CardActions; now?: number }) {
  const [menu, setMenu] = useState(false)
  const mm = m ?? machineOf(w!)
  const session = mm.session
  const admin = mm.admin
  const p = mm.primary
  const state = mm.state
  const dot = state === 'online' ? GREEN : state === 'offline' ? RED : AMBER
  const metrics = p.state === 'online' || p.source === 'hub' ? p.metrics : undefined
  const draining = session?.node?.draining
  const canRemove = (x?: PoolWorker) => !!x && !!actions?.remove && (actions.canRemove?.(x) ?? false)
  const menuItems: [string, () => void][] = []
  if (admin && actions?.attach) menuItems.push([`run as admin (attach ${admin.name})`, () => actions.attach!(admin.name)])
  if (admin && actions?.ssh) menuItems.push([`ssh as admin (${admin.name})`, () => actions.ssh!(admin.name)])
  for (const x of [session, admin]) if (x && canRemove(x)) menuItems.push([`remove ${x.name}…`, () => actions!.remove!([x.name])])
  return (
    <div className="wcard" data-testid={`worker-${mm.key}`} data-state={state}>
      <div className="head">
        <span className="dot" style={{ background: dot }} />
        <span className="name">{mm.key}</span>
        <span className="sub">{statusText(session ?? admin ?? p, now)}</span>
        {draining && (
          <span className="pill warn" title={session?.node?.drain_reason}>
            draining
          </span>
        )}
        {p.source && p.source !== 'none' && (
          <span className="pill" title="where these metrics come from">
            {p.source}
          </span>
        )}
        <span className="actions">
          {session && actions?.attach && <button onClick={() => actions.attach!(session.name)}>attach</button>}
          {session && actions?.ssh && <button onClick={() => actions.ssh!(session.name)}>ssh ↗</button>}
          {actions?.panel && <button onClick={() => actions.panel!(mm.key)}>hub ↗</button>}
          {session && actions?.drain && (
            <button title="longarm admin drain / undrain (admin certificate)" onClick={() => actions.drain!(session.name, !session.node?.admin_drain)}>
              {session.node?.admin_drain ? 'resume' : 'drain'}
            </button>
          )}
          {menuItems.length > 0 && (
            <span className="menuwrap">
              <button aria-label="more actions" aria-expanded={menu} onClick={() => setMenu((v) => !v)}>
                ⋯
              </button>
              {menu && (
                <span className="menu" role="menu">
                  {menuItems.map(([label, fn]) => (
                    <button
                      key={label}
                      role="menuitem"
                      onClick={() => {
                        setMenu(false)
                        fn()
                      }}
                    >
                      {label}
                    </button>
                  ))}
                </span>
              )}
            </span>
          )}
        </span>
      </div>
      {(admin || mm.labels.length > 0) && (
        <div className="caps" data-testid="caps">
          {admin && <AdminBadge admin={admin} />}
          {admin && (
            <span className="small muted" data-testid="cap-state">
              {session ? `${capText('worker', session, now)} · ${capText('admin', admin, now)}` : `admin only · ${capText('admin', admin, now)}`}
            </span>
          )}
          {mm.labels.map(([k, v]) => (
            <span key={k} className="pill chip" data-testid="label-chip">
              {k}={v}
            </span>
          ))}
        </div>
      )}
      <LinkRows w={p} now={now} />
      {!metrics ? (
        <div className={`msg ${state === 'offline' ? 'red' : 'muted'}`}>
          {(p.error || (state === 'connecting' ? 'waiting for the first sample…' : 'unreachable')).slice(0, 120)}
        </div>
      ) : (
        <Metrics m={metrics} w={p} />
      )}
    </div>
  )
}

/** A long-offline machine: one compact line, "last seen", the error behind "details". */
export function OfflineRow({ m, actions }: { m: PoolMachine; actions?: CardActions }) {
  const [open, setOpen] = useState(false)
  const members = [m.session, m.admin].filter((x): x is PoolWorker => !!x)
  const removable = members.filter((x) => !!actions?.remove && (actions.canRemove?.(x) ?? false)).map((x) => x.name)
  const errors = members.filter((x) => x.error).map((x) => `${x.name}: ${x.error}`)
  return (
    <div className="orow" data-testid={`offline-${m.key}`}>
      <div className="row">
        <span className="dot" style={{ width: 8, height: 8, background: 'var(--faint)' }} />
        <span className="oname">{m.key}</span>
        {m.admin && <AdminBadge admin={m.admin} compact />}
        <span className="muted small">last seen {fmtLastSeen(m.offlineFor)} ago</span>
        <span className="spacer" />
        {errors.length > 0 && (
          <button className="link small" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {open ? 'hide details' : 'details'}
          </button>
        )}
        {removable.length > 0 && (
          <button className="link small danger" onClick={() => actions!.remove!(removable)}>
            remove
          </button>
        )}
      </div>
      {open && <div className="small muted mono odetail">{errors.join('\n')}</div>}
    </div>
  )
}

function LinkRows({ w, now }: { w: PoolWorker; now: number }) {
  const legs = w.link?.legs ?? []
  if (!legs.length) return null
  const active = legs.find((l) => l.active) ?? legs[0]
  const hist = (active.history ?? []).map((s) => (s.rtt_ms !== undefined && s.rtt_ms >= 0 ? s.rtt_ms : null))
  const fails = (active.history ?? [])
    .map((s, i, a) => ({ i: a.length - 1 - i, bad: (s.rtt_ms ?? 0) < 0 }))
    .filter((x) => x.bad)
    .map((x) => ({ at: x.i, color: RED }))
  const relayed = active.up && isRelayed(active.kind)
  const relayFor = relayed && active.relay_since ? since(active.relay_since, now) : 0
  return (
    <div data-testid="link">
      <div className="linkrow">
        <span className="muted small" style={{ width: 36, fontWeight: 700 }}>
          LINK
        </span>
        <span className="path" style={{ color: !active.up ? RED : relayed ? AMBER : GREEN }} data-testid="link-path">
          {active.label || active.transport}
        </span>
        {active.nat && <span className="pill">nat {active.nat}</span>}
        {relayed && relayFor > 0 && <span className="small amber">relayed {fmtAgo(relayFor)}</span>}
        <span className="spacer" />
        <span className="mono" data-testid="link-rtt">
          {active.up ? fmtRTT(active.rtt_ms) : 'down'}
        </span>
      </div>
      <Legs legs={legs} />
      <Sparkline series={[{ values: hist, color: RTT_C, label: 'RTT' }]} max="auto" height={30} legend right="5 min" marks={fails} />
    </div>
  )
}

function Legs({ legs }: { legs: LegStatus[] }) {
  if (legs.length < 2) return null
  return (
    <div className="legs">
      {legs.map((l, i) => (
        <span key={i} className={`leg ${l.active ? 'active' : ''} ${l.up ? '' : 'down'}`} title={l.error || l.detail || l.endpoint}>
          {l.label || l.transport} {l.up ? fmtRTT(l.rtt_ms) : ''}
        </span>
      ))}
    </div>
  )
}

function Metrics({ m, w }: { m: MetricsSnapshot; w: PoolWorker }) {
  const cpu = m.cpu_percent ?? 0
  const mp = memPct(m)
  const hist = w.history ?? []
  const diskFree = gb(m.disk_free_bytes)
  return (
    <div style={{ marginTop: 6 }}>
      <Bar label="CPU" pct={cpu} value={`${cpu.toFixed(0)}%`} />
      <Bar label="MEM" pct={mp} value={`${gb(m.mem_used_bytes).toFixed(1)} / ${gb(m.mem_total_bytes).toFixed(0)} GB`} />
      <div style={{ marginTop: 4 }}>
        <Sparkline
          series={[
            { values: hist.map((p) => p.mem ?? 0), color: MEM_C, label: 'MEM' },
            { values: hist.map((p) => p.cpu ?? 0), color: CPU_C, label: 'CPU' },
          ]}
          legend
          right="5 min"
        />
      </div>
      <div className="agents">
        {AGENTS.map(([key, label, color]) => {
          const a = m.agents?.[key]
          const n = a?.sessions ?? 0
          return (
            <div key={key} className="agent" style={{ ['--c' as string]: n ? color : undefined }} data-testid={`agent-${key}`}>
              <span style={{ color: n ? 'var(--fg)' : 'var(--muted)' }}>{label}</span>
              <span className="n" style={{ color: n ? color : 'var(--faint)' }}>
                {n}
              </span>
              <span className="m">{n ? `${gb(a?.mem_bytes).toFixed(1)}G` : '—'}</span>
            </div>
          )
        })}
      </div>
      <div className="info">
        {m.threads ?? '?'} threads · {m.nproc ?? '?'} procs · longarm jobs {m.jobs_running ?? 0}
        {w.my_jobs ? ` (mine ${w.my_jobs})` : ''} · up {fmtUptime(num(m.uptime_seconds))}
        {num(m.disk_total_bytes) > 0 && ` · disk ${diskFree.toFixed(0)} GB free`}
      </div>
      <TopProcs m={m} />
    </div>
  )
}

function TopProcs({ m }: { m: MetricsSnapshot }) {
  const cols: [ProcGroup[], string, (p: ProcGroup) => string][] = [
    [(m.top_cpu ?? []).slice(0, 3), 'TOP CPU', (p) => `${(p.cpu_percent ?? 0).toFixed(0)}%`],
    [(m.top_mem ?? []).slice(0, 3), 'TOP MEMORY', (p) => fmtBytes(p.mem_bytes)],
  ]
  return (
    <div className="tops">
      {cols.map(([items, title, val]) => (
        <div key={title}>
          <div className="title">{title}</div>
          {items.map((p, i) => (
            <div className="p" key={i}>
              <span>
                {p.name.slice(0, 16)}
                {(p.count ?? 1) > 1 ? ` ×${p.count}` : ''}
              </span>
              <span className="v">{val(p)}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  )
}
