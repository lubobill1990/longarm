import { useState } from 'react'
import type { LegStatus, MetricsSnapshot, PoolWorker, ProcGroup } from '../api/types'
import type { WorkerUpdateJob } from '../api/local'
import {
  AMBER,
  CPU_C,
  cssColor,
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
import { IconExternal, IconMore } from './icons'

const AGENTS: [string, string, string][] = [
  ['claude', 'Claude Code', 'var(--claude)'],
  ['copilot', 'Copilot CLI', 'var(--copilot)'],
]

export interface CardActions {
  attach?: (w: string) => void
  ssh?: (w: string) => void
  /** longarm desktop: Windows App for a Dev Box, Remote Desktop otherwise (Windows workers) */
  desktop?: (w: string) => void
  panel?: (w: string) => void
  /** longarm admin drain / undrain: admins only (leave it unset for others) */
  drain?: (w: string, on: boolean) => void
  /** remove (revoke) workers on the hub; the caller confirms */
  remove?: (names: string[]) => void
  /** whether this user may remove the worker (its owner, or an admin) */
  canRemove?: (w: PoolWorker) => boolean
  /** "Update to X": longarm worker update (the worker's owner only; the caller runs and tracks it) */
  update?: (w: string, version: string) => void
  /** whether this user may update the worker's machine (its owner) */
  canUpdate?: (w: PoolWorker) => boolean
  /** updates in progress or finished, by worker name */
  updates?: Record<string, WorkerUpdateJob>
}

/** "0.1.8" (a leading v dropped), "" when unknown. */
export function fmtVersion(v?: string): string {
  return (v ?? '').replace(/^v/, '')
}

/** The machine's versions: "v0.1.8", "v0.1.8 · admin v0.1.7" when they differ. */
export function machineVersion(session?: PoolWorker, admin?: PoolWorker): string {
  const sv = fmtVersion(session?.node?.version)
  const av = fmtVersion(admin?.node?.version)
  if (sv && av && sv !== av) return `v${sv} · admin v${av}`
  if (sv || av) return `v${sv || av}`
  return ''
}

/** The worker an update of the machine goes to: the session worker, else the admin worker. */
export function updateTarget(session?: PoolWorker, admin?: PoolWorker): PoolWorker | undefined {
  return session ?? admin
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
  const dot = cssColor(state === 'online' ? GREEN : state === 'offline' ? RED : AMBER)
  const metrics = p.state === 'online' || p.source === 'hub' ? p.metrics : undefined
  const draining = session?.node?.draining
  const canRemove = (x?: PoolWorker) => !!x && !!actions?.remove && (actions.canRemove?.(x) ?? false)
  const menuItems: [string, () => void][] = []
  if (admin && actions?.attach) menuItems.push([`run as admin (attach ${admin.name})`, () => actions.attach!(admin.name)])
  if (admin && actions?.ssh) menuItems.push([`ssh as admin (${admin.name})`, () => actions.ssh!(admin.name)])
  for (const x of [session, admin]) if (x && canRemove(x)) menuItems.push([`remove ${x.name}…`, () => actions!.remove!([x.name])])
  const version = machineVersion(session, admin)
  const target = updateTarget(session, admin)
  const ua = target?.node?.update_available
  const avail = ua?.version ?? ''
  const job = target ? actions?.updates?.[target.name] : undefined
  const running = job?.state === 'running'
  // "none": longarm worker update can't install it there (no Longarm Update
  // service, no self-update): no button, the version pill says why
  const remote = ua?.method !== 'none'
  const canUpdate =
    !!target && !!avail && remote && !!actions?.update && (actions.canUpdate?.(target) ?? false) && target.state === 'online'
  return (
    <div className="wcard" data-testid={`worker-${mm.key}`} data-state={state}>
      <div className="head">
        <span className={`dot is-${state}`} style={{ background: dot }} />
        <span className="name">{mm.key}</span>
        {version && (
          <span className={`pill ${avail ? 'warn' : 'chip'} ver`} data-testid="version" title={avail ? `Longarm ${avail} is available${remote ? '' : ` - it can't be updated from here: ${ua?.reason ?? ''}`}` : 'Longarm version'}
          >
            {version}
            {avail ? ' ↑' : ''}
          </span>
        )}
        <span className="sub">{statusText(session ?? admin ?? p, now)}</span>
        {draining && (
          <span className="pill warn" title={session?.node?.drain_reason}>
            draining
          </span>
        )}
        <span className="actions">
          {canUpdate && (
            <button
              className="act update"
              data-testid="update-btn"
              disabled={running}
              title={`longarm worker update ${target!.name} --version ${avail}: installs the signed release; running jobs and sessions keep running`}
              onClick={() => actions!.update!(target!.name, avail)}
            >
              {running ? 'Updating…' : `Update to ${avail}`}
            </button>
          )}
          {session && actions?.attach && (
            <button className="act" title={`open a terminal: longarm attach ${session.name}`} onClick={() => actions.attach!(session.name)}>
              attach
            </button>
          )}
          {session && actions?.ssh && (
            <button className="act" title={`longarm ssh ${session.name} (a new window)`} onClick={() => actions.ssh!(session.name)}>
              ssh <IconExternal aria-hidden="true" />
            </button>
          )}
          {session && actions?.desktop && (session.node?.os ?? 'windows') === 'windows' && (
            <button
              className="act"
              title={`longarm desktop ${session.name}: a Dev Box opens in Windows App, other workers in Remote Desktop`}
              onClick={() => actions.desktop!(session.name)}
            >
              desktop <IconExternal aria-hidden="true" />
            </button>
          )}
          {actions?.panel && (
            <button className="act" title="open this machine in the hub panel" onClick={() => actions.panel!(mm.key)}>
              hub <IconExternal aria-hidden="true" />
            </button>
          )}
          {session && actions?.drain && (
            <button className="act" title="longarm admin drain / undrain (admin certificate)" onClick={() => actions.drain!(session.name, !session.node?.admin_drain)}>
              {session.node?.admin_drain ? 'resume' : 'drain'}
            </button>
          )}
          {menuItems.length > 0 && (
            <span className="menuwrap">
              <button
                className="act icon"
                aria-label="more actions"
                aria-haspopup="menu"
                aria-expanded={menu}
                onClick={() => setMenu((v) => !v)}
                onKeyDown={(e) => e.key === 'Escape' && setMenu(false)}
              >
                <IconMore />
              </button>
              {menu && (
                <span className="menu" role="menu" onKeyDown={(e) => e.key === 'Escape' && setMenu(false)}>
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
      {job && job.state !== 'idle' && <UpdateLine job={job} />}
      <LinkRows w={p} now={now} />
      {!metrics ? (
        <div className={`msg ${state === 'offline' ? 'red' : 'muted'}`}>
          {(p.error || (state === 'connecting' ? 'waiting for the first sample…' : 'unreachable')).slice(0, 120)}
        </div>
      ) : (
        <Metrics m={metrics} w={p} now={now} />
      )}
    </div>
  )
}

/** An update of the machine: its progress, then the outcome. */
export function UpdateLine({ job }: { job: WorkerUpdateJob }) {
  if (job.state === 'running') {
    const last = job.progress?.[job.progress.length - 1] ?? 'starting…'
    return (
      <div className="msg muted" data-testid="update-line">
        updating: {last}
      </div>
    )
  }
  if (job.state === 'failed')
    return (
      <div className="msg red" data-testid="update-line">
        update failed: {job.outcome?.error || job.error}
      </div>
    )
  return (
    <div className="msg ok" data-testid="update-line">
      {job.outcome?.summary}
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
        <span className="dot is-gone" style={{ width: 8, height: 8, background: 'var(--faint)' }} />
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

/** Loopback / unspecified endpoints: only usable on the worker's own machine. */
export function isLocalOnly(endpoint?: string): boolean {
  const m = /^[a-z]+:\/\/(\[[^\]]*\]|[^:/?]*)/.exec(endpoint ?? '')
  const h = (m?.[1] ?? '').replace(/^\[|\]$/g, '')
  return h === 'localhost' || h.startsWith('127.') || h === '::1' || h === '0.0.0.0' || h === '::'
}

function LinkRows({ w, now }: { w: PoolWorker; now: number }) {
  const legs = w.link?.legs ?? []
  if (!legs.length) {
    // The worker advertises only endpoints of its own machine (loopback):
    // nothing this client can dial - say so instead of a bare "down".
    const eps = w.node?.endpoints ?? []
    if (!eps.length || !eps.every(isLocalOnly)) return null
    return (
      <div data-testid="link">
        <div className="linkrow">
          <span className="muted small linklabel">
            LINK
          </span>
          <span className="path is-down" style={{ color: cssColor(RED) }} data-testid="link-path" title={eps.join(' ')}>
            tcp (loopback only): unreachable from here
          </span>
        </div>
        <div className="small muted">the worker listens on its own machine only; a current longarmd also listens on the hub's relay</div>
      </div>
    )
  }
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
        <span className="muted small linklabel">
          LINK
        </span>
        <span className={`path ${!active.up ? 'is-down' : relayed ? 'is-relayed' : 'is-up'}`} style={{ color: cssColor(!active.up ? RED : relayed ? AMBER : GREEN) }} data-testid="link-path">
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

/** Metrics older than this are stale: 2 × the 5 s the hub's and the direct samples arrive within. */
export const METRICS_STALE_S = 10

/** Where the metrics come from and how old they are; stale ones are grayed out. */
export function metricsSource(w: PoolWorker, m: MetricsSnapshot, now: number): { text: string; stale: boolean } {
  const age = m.time ? since(m.time, now) : undefined
  const fresh = w.source === 'hub' || w.source === 'direct'
  const via = w.source === 'hub' ? 'metrics via hub' : w.source === 'direct' ? 'metrics direct' : 'last known metrics'
  return {
    text: age === undefined ? via : `${via} · updated ${fmtAgo(age)} ago`,
    stale: !fresh || (age !== undefined && age > METRICS_STALE_S),
  }
}

function Metrics({ m, w, now }: { m: MetricsSnapshot; w: PoolWorker; now: number }) {
  const cpu = m.cpu_percent ?? 0
  const mp = memPct(m)
  const hist = w.history ?? []
  const diskFree = gb(m.disk_free_bytes)
  const src = metricsSource(w, m, now)
  return (
    <div style={{ marginTop: 6, opacity: src.stale ? 0.45 : undefined }} data-stale={src.stale ? 'true' : undefined}>
      <div className="small muted" data-testid="metrics-source" title="where these metrics come from">
        {src.text}
      </div>
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
