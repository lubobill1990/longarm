import { useEffect, useState } from 'react'
import type { LegStatus, MetricsSnapshot, PoolWorker, ProcGroup } from '../api/types'
import { useDismiss } from '../lib/dismiss'
import type { WorkerUpdateJob } from '../api/local'
import {
  AMBER,
  CPU_C,
  cssColor,
  DISK_C,
  fmtAgo,
  fmtBytes,
  fmtRate,
  fmtRTT,
  fmtUptime,
  gb,
  GREEN,
  isRelayed,
  MEM_C,
  memPct,
  NET_RX_C,
  NET_TX_C,
  num,
  RED,
  RTT_C,
  since,
} from '../lib/format'
import { ADMIN_LABEL, fmtLastSeen, isAdminNode, offlineSeconds, type PoolMachine } from '../lib/machines'
import { dismissedUpdates, dismissUpdate, jobsNote, machineUpdate, updateRunning, updateView, type UpdateView } from '../lib/update'
import type { NodeUpdate } from '../api/types'
import { Bar, niceMax, Sparkline } from './charts'
import { IconMore, IconShield, IconTerminal } from './icons'

const AGENTS: [string, string, string][] = [
  ['claude', 'Claude Code', 'var(--claude)'],
  ['copilot', 'Copilot CLI', 'var(--copilot)'],
]

export interface CardActions {
  attach?: (w: string) => void
  ssh?: (w: string) => void
  /** longarm desktop: Windows App for a Dev Box, Remote Desktop otherwise (Windows workers) */
  desktop?: (w: string) => void
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
  /** the Claude Code / Copilot CLI counters open the Sessions window (worker, agent) */
  sessions?: (w: string, agent: string) => void
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

/** The text with a capital first letter (a hover card value). */
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

/**
 * The state shield and, on hover or focus, the machine's facts in a card of
 * ours (not the webview's title tooltip): state, the end-to-end encryption,
 * the worker and admin worker, labels, uptime, node ID.
 */
function MachineInfo({
  state,
  color,
  name,
  rows,
}: {
  state: string
  color: string
  name: string
  rows: [string, React.ReactNode][]
}) {
  return (
    // the shield and the name: hovering either shows the card
    <span className={`shield is-${state}`} data-testid="state-shield" tabIndex={0} aria-label={`${name}: ${state}, end-to-end encrypted`}>
      <IconShield size={14} aria-hidden="true" style={{ color }} />
      <span className="name">{name}</span>
      <span className="hovercard" role="tooltip" data-testid="machine-info">
        <span className="hovercard-title">
          <span className="hovercard-dot" style={{ background: color }} /> {name} · {state}
        </span>
        <span className="hovercard-rows">
          {rows.map(([k, v]) => (
            <span key={k} className="hovercard-row">
              <span className="k">{k}</span>
              <span className="v">{v}</span>
            </span>
          ))}
        </span>
      </span>
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

function capText(label: string, w: PoolWorker, now: number, updating = false): string {
  const st = stateOf(w)
  if (updating && st !== 'online') return `${label} updating`
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
/** The badges dismissed on this computer, kept current across cards and windows. */
function useDismissedUpdates(): [Set<string>, (key: string) => void] {
  const [set, setSet] = useState(dismissedUpdates)
  useEffect(() => {
    const on = () => setSet(dismissedUpdates())
    window.addEventListener('longarm-update-dismissed', on)
    window.addEventListener('storage', on)
    return () => {
      window.removeEventListener('longarm-update-dismissed', on)
      window.removeEventListener('storage', on)
    }
  }, [])
  return [set, dismissUpdate]
}

/**
 * The update of a machine the card shows: the hub's (NodeInfo.update of
 * its session or admin worker - one MSI updates both), else, for a worker
 * too old to report one, the update this tray started (a generic
 * "updating" with the time it takes).
 */
function cardUpdate(session?: PoolWorker, admin?: PoolWorker, job?: WorkerUpdateJob): NodeUpdate | undefined {
  const u = machineUpdate([session?.node, admin?.node])
  if (u) return u
  if (job?.state === 'running')
    return { state: 'updating', to: job.to, started_at: job.started ? new Date(job.started).toISOString() : undefined, reported: false }
  return undefined
}

export function WorkerCard({ w, m, actions, now = Date.now() }: { w?: PoolWorker; m?: PoolMachine; actions?: CardActions; now?: number }) {
  const [menu, setMenu] = useState(false)
  const menuRef = useDismiss<HTMLSpanElement>(menu, () => setMenu(false))
  const [dismissed, dismiss] = useDismissedUpdates()
  const mm = m ?? machineOf(w!)
  const session = mm.session
  const admin = mm.admin
  const p = mm.primary
  const target0 = updateTarget(session, admin)
  const u = cardUpdate(session, admin, target0 ? actions?.updates?.[target0.name] : undefined)
  // while it updates the machine is "updating", not offline: no red dot,
  // no "offline, reconnecting", no link errors (the restart is expected)
  const updating = updateRunning(u)
  const state = updating && mm.state !== 'online' ? 'updating' : mm.state
  const view = u ? updateView(u, mm.state === 'online', now, p.node?.id || mm.key) : undefined
  const badge = view?.badge && !dismissed.has(view.key) ? view : undefined
  const dot = cssColor(state === 'online' ? GREEN : state === 'offline' ? RED : AMBER)
  const metrics = p.state === 'online' || p.source === 'hub' ? p.metrics : undefined
  const draining = session?.node?.draining
  const canRemove = (x?: PoolWorker) => !!x && !!actions?.remove && (actions.canRemove?.(x) ?? false)
  const menuItems: [string, () => void][] = []
  // attach is the terminal (a session that outlives its window); ssh - a plain
  // login for scp, port forwards, editors' remote - is in the menu
  if (session && actions?.ssh) menuItems.push([`SSH ${session.name} (a plain SSH login)`, () => actions.ssh!(session.name)])
  if (session && actions?.desktop && (session.node?.os ?? 'windows') === 'windows')
    menuItems.push(['Desktop (Remote Desktop / Windows App)', () => actions.desktop!(session.name)])
  if (session && actions?.drain)
    menuItems.push([session.node?.admin_drain ? 'Resume (take jobs again)' : 'Drain (no new jobs)', () => actions.drain!(session.name, !session.node?.admin_drain)])
  if (admin && actions?.attach) menuItems.push([`Run as admin (attach ${admin.name})`, () => actions.attach!(admin.name)])
  if (admin && actions?.ssh) menuItems.push([`SSH as admin (${admin.name})`, () => actions.ssh!(admin.name)])
  for (const x of [session, admin]) if (x && canRemove(x)) menuItems.push([`Remove ${x.name}…`, () => actions!.remove!([x.name])])
  const version = machineVersion(session, admin)
  const target = updateTarget(session, admin)
  const ua = target?.node?.update_available
  const avail = ua?.version ?? ''
  const job = target ? actions?.updates?.[target.name] : undefined
  const running = job?.state === 'running' || updating
  // "none": longarm worker update can't install it there (no Longarm Update
  // service, no self-update): no button, the version pill says why
  const remote = ua?.method !== 'none'
  const canUpdate =
    !!target && !!avail && remote && !!actions?.update && (actions.canUpdate?.(target) ?? false) && target.state === 'online'
  const uptime = num(metrics?.uptime_seconds)
  const infoRows: [string, React.ReactNode][] = [
    ['Status', <span data-testid="status">{cap(updating && view ? view.title : statusText(session ?? admin ?? p, now))}</span>],
    ['Encryption', state === 'offline' ? 'Not connected' : 'End-to-end: mutual TLS between this computer and the machine; relays and the hub only pass ciphertext'],
  ]
  if (admin)
    infoRows.push([
      'Workers',
      <>
        <span data-testid="cap-state">
          {cap(session ? `${capText('worker', session, now, updating)} · ${capText('admin', admin, now, updating)}` : `admin only · ${capText('admin', admin, now, updating)}`)}
        </span>{' '}
        <AdminBadge admin={admin} />
      </>,
    ])
  if (mm.labels.length)
    infoRows.push([
      'Labels',
      mm.labels.map(([k, v]) => (
        <span key={k} className="pill chip" data-testid="label-chip">
          {k}={v}
        </span>
      )),
    ])
  if (uptime > 0) infoRows.push(['Uptime', fmtUptime(uptime)])
  if (p.node?.id) infoRows.push(['Node ID', <span className="mono">{p.node.id}</span>])
  return (
    <div className="wcard" data-testid={`worker-${mm.key}`} data-state={state}>
      <div className="head">
        {/* the state's color as a shield (the connection is end-to-end encrypted); the machine's facts on hover */}
        <MachineInfo state={state} color={dot} name={mm.key} rows={infoRows} />
        {version &&
          (canUpdate ? (
            // the version is the update button: one control, the arrow says there is a newer one
            <button
              className="pill warn ver upd"
              data-testid="update-btn"
              disabled={running}
              aria-label={`Update to ${avail}`}
              title={`${avail} is available: click to update (longarm worker update ${target!.name} --version ${avail} installs the signed release; running jobs and sessions keep running)`}
              onClick={() => actions!.update!(target!.name, avail)}
            >
              <span data-testid="version">
                {version}
                {running ? '' : ' ↑'}
              </span>
              {running && <span> · updating…</span>}
            </button>
          ) : (
            <span
              className={`pill ${avail ? 'warn' : 'chip'} ver`}
              data-testid="version"
              title={avail ? `Longarm ${avail} is available${remote ? '' : ` - it can't be updated from here: ${ua?.reason ?? ''}`}` : 'Longarm version'}
            >
              <span>
                {version}
                {avail ? ' ↑' : ''}
              </span>
            </span>
          ))}
        {draining && (
          <span className="pill warn" title={session?.node?.drain_reason}>
            draining
          </span>
        )}
        <span className="actions">
          {session && actions?.attach && (
            <button
              className="act"
              title={`a terminal on ${session.name} in a session that keeps running when you close it - attach again to go on (longarm attach ${session.name}; ssh: in the … menu)`}
              onClick={() => actions.attach!(session.name)}
            >
              <IconTerminal size={14} aria-hidden="true" />
              <span>Attach</span>
            </button>
          )}
          {menuItems.length > 0 && (
            <span className="menuwrap" ref={menuRef}>
              <button
                className="act icon"
                aria-label="more actions"
                aria-haspopup="menu"
                aria-expanded={menu}
                onClick={() => setMenu((v) => !v)}
              >
                <IconMore />
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
      {updating && view && <UpdateStatus view={view} u={u!} />}
      {!updating && badge && <UpdateBadge view={badge} onDismiss={() => dismiss(badge.key)} />}
      {!u && job && job.state !== 'idle' && <UpdateLine job={job} />}
      <LinkRows w={p} now={now} updating={updating} chart={!metrics} />
      {!metrics ? (
        updating ? (
          <div className="msg muted" data-testid="update-wait">
            metrics resume when the worker is back
          </div>
        ) : (
          <div className={`msg ${state === 'offline' ? 'red' : 'muted'}`}>
            {(p.error || (state === 'connecting' ? 'waiting for the first sample…' : 'unreachable')).slice(0, 120)}
          </div>
        )
      ) : (
        <Metrics m={metrics} w={p} now={now} rtt={rttOf(p, updating)} onAgent={actions?.sessions && ((a) => actions.sessions!((session ?? p).name, a))} />
      )}
    </div>
  )
}

/** An update in progress: "Updating to v0.1.30…", the phase, a progress bar, what happens to the jobs. */
export function UpdateStatus({ view, u }: { view: UpdateView; u: NodeUpdate }) {
  return (
    <div className="upd-status" data-testid="update-status" data-kind={view.kind}>
      <div className="upd-head">
        <span className="upd-title">{view.title}</span>
        <span className="upd-detail" data-testid="update-detail">
          {view.detail}
        </span>
      </div>
      {view.progress !== undefined && (
        <div className="upd-bar" role="progressbar" aria-valuenow={Math.round(view.progress * 100)} aria-valuemin={0} aria-valuemax={100}>
          <span style={{ width: `${Math.round(view.progress * 100)}%` }} />
        </div>
      )}
      <div className="upd-note">{jobsNote(u)}</div>
    </div>
  )
}

/** How an update ended badly (failed, didn't finish, another version, restart pending): dismissible. */
export function UpdateBadge({ view, onDismiss }: { view: UpdateView; onDismiss: () => void }) {
  return (
    <div className={`upd-badge is-${view.kind}`} data-testid="update-badge" data-kind={view.kind} role="status">
      <span className="upd-title">{view.title}</span>
      {view.detail && <span className="upd-detail">{view.detail}</span>}
      <button type="button" className="upd-x" aria-label="dismiss" title="dismiss (on this computer)" onClick={onDismiss}>
        ×
      </button>
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
        {m.update?.state === 'timed_out' && (
          <span className="small amber" data-testid="offline-update">
            update to v{(m.update.to ?? '').replace(/^v/, '')} didn't finish
          </span>
        )}
        <span className="spacer" />
        {errors.length > 0 && (
          <button className="link small" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {open ? 'Hide details' : 'Details'}
          </button>
        )}
        {removable.length > 0 && (
          <button className="link small danger" onClick={() => actions!.remove!(removable)}>
            Remove
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

/** The link's latency over the last minutes (the active leg) and its failed probes - red bands, amber while the machine updates. */
function rttOf(w: PoolWorker, updating?: boolean): { values: (number | null)[]; marks: { at: number; color: string }[] } | undefined {
  const legs = w.link?.legs ?? []
  if (!legs.length) return undefined
  const active = legs.find((l) => l.active) ?? legs[0]
  const h = active.history ?? []
  return {
    values: h.map((s) => (s.rtt_ms !== undefined && s.rtt_ms >= 0 ? s.rtt_ms : null)),
    marks: h
      .map((s, i, a) => ({ i: a.length - 1 - i, bad: (s.rtt_ms ?? 0) < 0 }))
      .filter((x) => x.bad)
      .map((x) => ({ at: x.i, color: updating ? AMBER : RED })),
  }
}

/** The LINK line (path, NAT, paths, latency); chart: its own RTT chart (when no metrics chart takes the RTT line). */
function LinkRows({ w, now, updating, chart }: { w: PoolWorker; now: number; updating?: boolean; chart?: boolean }) {
  const [showLegs, setShowLegs] = useState(false)
  const legs = w.link?.legs ?? []
  if (!legs.length) {
    // The worker advertises only endpoints of its own machine (loopback):
    // nothing this client can dial - say so instead of a bare "down".
    const eps = w.node?.endpoints ?? []
    if (updating || !eps.length || !eps.every(isLocalOnly)) return null
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
  const rtt = rttOf(w, updating)!
  const down = !active.up && !updating
  const relayed = active.up && isRelayed(active.kind)
  const relayFor = relayed && active.relay_since ? since(active.relay_since, now) : 0
  return (
    <div data-testid="link">
      <div className="linkrow">
        <span className="muted small linklabel">
          LINK
        </span>
        <span
          className={`path ${down ? 'is-down' : !active.up ? 'is-updating' : relayed ? 'is-relayed' : 'is-up'}`}
          style={{ color: cssColor(down ? RED : !active.up || relayed ? AMBER : GREEN) }}
          data-testid="link-path"
          title={!active.up && updating ? 'the worker restarts for its update' : undefined}
        >
          {active.label || active.transport}
        </span>
        {active.nat && <span className="pill">nat {active.nat}</span>}
        {relayed && relayFor > 0 && <span className="small amber">relayed {fmtAgo(relayFor)}</span>}
        {legs.length > 1 && (
          <button
            type="button"
            className="link-more"
            aria-expanded={showLegs}
            data-testid="legs-toggle"
            title="every way this computer reaches the worker, and its latency"
            onClick={() => setShowLegs((v) => !v)}
          >
            {legs.length} paths {showLegs ? '▴' : '▾'}
          </button>
        )}
        <span className="spacer" />
        <span className="mono" data-testid="link-rtt">
          {active.up ? fmtRTT(active.rtt_ms) : updating ? 'updating' : 'down'}
        </span>
      </div>
      {showLegs && <Legs legs={legs} updating={updating} />}
      {chart && (
        <Sparkline
          series={[{ values: rtt.values, color: RTT_C, label: 'RTT', fmt: (v) => fmtRTT(v) }]}
          max="auto"
          height={30}
          legend
          right={updating && rtt.marks.length ? 'gap: updating · 5 min' : '5 min'}
          marks={rtt.marks}
          fmtMax={(v) => `${Math.round(v)} ms`}
        />
      )}
    </div>
  )
}

/** Every leg to the worker (the link row's dropdown): path, endpoint, latency; the one in use marked. */
function Legs({ legs, updating }: { legs: LegStatus[]; updating?: boolean }) {
  return (
    <div className="legs-list" data-testid="legs">
      {legs.map((l, i) => (
        <div
          key={i}
          className={`leg-row ${l.active ? 'active' : ''} ${l.up || updating ? '' : 'down'}`}
          title={updating && !l.up ? 'the worker restarts for its update' : l.error || l.detail || l.endpoint}
        >
          <span className="leg-mark">{l.active ? '✓' : ''}</span>
          <span className="leg-name">{l.label || l.transport}</span>
          <span className="leg-ep">{l.endpoint}</span>
          <span className="leg-rtt mono">{l.up ? fmtRTT(l.rtt_ms) : updating ? '—' : l.error ? 'down' : '…'}</span>
        </div>
      ))}
    </div>
  )
}

/** Metrics older than this are stale: 2 × the 5 s the hub's and the direct samples arrive within. */
export const METRICS_STALE_S = 10

/** Where the metrics come from and how old they are; stale ones are grayed out. */
export function metricsSource(w: PoolWorker, m: MetricsSnapshot, now: number): { text: string; short: string; stale: boolean } {
  const age = m.time ? since(m.time, now) : undefined
  const fresh = w.source === 'hub' || w.source === 'direct'
  const via = w.source === 'hub' ? 'metrics via hub' : w.source === 'direct' ? 'metrics direct' : 'last known metrics'
  return {
    text: age === undefined ? via : `${via} · updated ${fmtAgo(age)} ago`,
    // the chart's corner: how old they are (where from: text, its title)
    short: fresh ? (age === undefined ? 'live' : `updated ${fmtAgo(age)} ago`) : age === undefined ? 'last known' : `last known · ${fmtAgo(age)} ago`,
    stale: !fresh || (age !== undefined && age > METRICS_STALE_S),
  }
}

/** Which extra series the charts show (off by default; remembered on this computer). */
const EXTRA_KEY = 'longarm.metrics.extra'
type Extra = { disk: boolean; net: boolean }
function loadExtra(): Extra {
  try {
    const v = JSON.parse(localStorage.getItem(EXTRA_KEY) ?? '{}') as Partial<Extra>
    return { disk: !!v.disk, net: !!v.net }
  } catch {
    return { disk: false, net: false }
  }
}
/** The extra series, shared by every card (one toggle shows disk everywhere). */
function useExtra(): [Extra, (k: keyof Extra) => void] {
  const [extra, setExtra] = useState<Extra>(loadExtra)
  useEffect(() => {
    const on = (e: StorageEvent | Event) => {
      if (!(e instanceof StorageEvent) || e.key === EXTRA_KEY) setExtra(loadExtra())
    }
    window.addEventListener('storage', on)
    window.addEventListener('longarm-metrics-extra', on)
    return () => {
      window.removeEventListener('storage', on)
      window.removeEventListener('longarm-metrics-extra', on)
    }
  }, [])
  const toggle = (k: keyof Extra) => {
    const next = { ...loadExtra(), [k]: !extra[k] }
    try {
      localStorage.setItem(EXTRA_KEY, JSON.stringify(next))
    } catch {
      /* private mode: this card only */
    }
    setExtra(next)
    window.dispatchEvent(new Event('longarm-metrics-extra'))
  }
  return [extra, toggle]
}

function Metrics({
  m,
  w,
  now,
  rtt,
  onAgent,
}: {
  m: MetricsSnapshot
  w: PoolWorker
  now: number
  /** the link's latency: a line of the same chart, on its own scale */
  rtt?: ReturnType<typeof rttOf>
  onAgent?: (agent: string) => void
}) {
  const cpu = m.cpu_percent ?? 0
  const mp = memPct(m)
  const hist = w.history ?? []
  const diskFree = gb(m.disk_free_bytes)
  const diskTotal = gb(m.disk_total_bytes)
  const diskPct = diskTotal > 0 ? (100 * (diskTotal - diskFree)) / diskTotal : 0
  const [extra, toggle] = useExtra()
  // this card's top processes only: opening them doesn't move the cards below the pointer
  const [tops, setTops] = useState(false)
  const src = metricsSource(w, m, now)
  // the network lines' scale: a round top over both directions in the window
  const netPeak = niceMax(Math.max(0, ...hist.map((p) => Math.max(p.net_rx ?? 0, p.net_tx ?? 0))))
  return (
    <div style={{ marginTop: 6, opacity: src.stale ? 0.45 : undefined }} data-stale={src.stale ? 'true' : undefined}>
      {/* the chart (its legend and toggles on top), then the bars: a toggle adds a bar below, nothing above it moves */}
      <div style={{ marginTop: 4 }}>
        <Sparkline
          series={[
            { values: hist.map((p) => p.mem ?? 0), color: MEM_C, label: 'MEM' },
            { values: hist.map((p) => p.cpu ?? 0), color: CPU_C, label: 'CPU' },
            ...(extra.disk ? [{ values: hist.map((p) => p.disk ?? null), color: DISK_C, label: 'DISK' }] : []),
            // network in the same chart, on its own scale (the NET toggle is its key)
            ...(extra.net
              ? [
                  { values: hist.map((p) => p.net_rx ?? null), color: NET_RX_C, label: '↓ in', fmt: fmtRate, peak: netPeak, key: false },
                  { values: hist.map((p) => p.net_tx ?? null), color: NET_TX_C, label: '↑ out', fmt: fmtRate, peak: netPeak, key: false },
                ]
              : []),
            // the link's latency, on its own scale (the LINK line above says it now)
            ...(rtt ? [{ values: rtt.values, color: RTT_C, label: 'RTT', fmt: (v: number) => fmtRTT(v), peak: niceMax(Math.max(10, ...rtt.values.map((v) => v ?? 0))) }] : []),
          ]}
          legend
          legendTop
          right={
            // how fresh they are
            <span data-testid="metrics-source">
              {src.short}
            </span>
          }
          marks={rtt?.marks}
          toggles={[
            { label: 'DISK', color: DISK_C, on: extra.disk, onToggle: () => toggle('disk'), title: 'disk space used (the data dir\'s volume)' },
            { label: 'NET', color: NET_RX_C, on: extra.net, onToggle: () => toggle('net'), title: 'network traffic, bytes per second' },
          ]}
        />
      </div>
      <div className="bars">
        <Bar label="CPU" pct={cpu} value={`${cpu.toFixed(0)}%`} />
        <Bar label="MEM" pct={mp} value={`${gb(m.mem_used_bytes).toFixed(1)} / ${gb(m.mem_total_bytes).toFixed(0)} GB`} />
        {extra.disk && diskTotal > 0 && (
          <Bar label="DISK" pct={diskPct} value={`${(diskTotal - diskFree).toFixed(0)} / ${diskTotal.toFixed(0)} GB`} />
        )}
        {extra.net && (
          // a row like the bars (no track: bytes/s have no 100 %); the chart
          // draws in and out against netPeak, its top
          <div
            className="bar net"
            data-testid="net-now"
            title={`network traffic of the worker's interfaces (loopback excluded); the chart's top is ${fmtRate(netPeak)}`}
          >
            <span className="label">NET</span>
            <span />
            <span className="value">
              <span style={{ color: cssColor(NET_RX_C) }}>↓ {fmtRate(num(m.net_rx_bytes_per_sec))}</span>{' '}
              <span style={{ color: cssColor(NET_TX_C) }}>↑ {fmtRate(num(m.net_tx_bytes_per_sec))}</span>
            </span>
          </div>
        )}
      </div>
      <div className="agents">
        {AGENTS.map(([key, label, color]) => {
          const a = m.agents?.[key]
          const n = a?.sessions ?? 0
          const body = (
            <>
              <span style={{ color: n ? 'var(--fg)' : 'var(--muted)' }}>{label}</span>
              <span className="n" style={{ color: n ? color : 'var(--faint)' }}>
                {n}
              </span>
              <span className="m">{n ? `${gb(a?.mem_bytes).toFixed(1)}G` : '—'}</span>
            </>
          )
          const style = { ['--c' as string]: n ? color : undefined }
          // a button when the Sessions window can open (the tray), a plain counter elsewhere
          return onAgent ? (
            <button key={key} className="agent" style={{ ...style, width: '100%' }} data-testid={`agent-${key}`} title={`${label} sessions on ${w.name}`} onClick={() => onAgent(key)}>
              {body}
            </button>
          ) : (
            <div key={key} className="agent" style={style} data-testid={`agent-${key}`}>
              {body}
            </div>
          )
        })}
      </div>
      {/* uptime: in the state shield's tooltip; free disk: the DISK toggle */}
      <div className="info">
        <span>
          {m.threads ?? '?'} threads · {m.nproc ?? '?'} procs · longarm jobs {m.jobs_running ?? 0}
          {w.my_jobs ? ` (mine ${w.my_jobs})` : ''}
        </span>
        <button type="button" className="link tops-toggle" aria-expanded={tops} data-testid="tops-toggle" onClick={() => setTops((v) => !v)}>
          Top processes {tops ? '▴' : '▾'}
        </button>
      </div>
      {tops && <TopProcs m={m} />}
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
