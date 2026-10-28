// One machine, one entry: a user-session worker and its admin worker (runs
// commands as SYSTEM; label longarm.io/admin=true) are two hub nodes on the
// same machine. The tray, the panel and longarm ls (Go: internal/machine)
// show them together. The hub names the pair (NodeInfo.companion /
// machine); with an older hub, an admin worker named <worker>-admin with
// the same owner and host name pairs with <worker>.
import type { NodeInfo, PoolWorker } from '../api/types'
import { since } from './format'

export const ADMIN_LABEL = 'longarm.io/admin'
export const ADMIN_SUFFIX = '-admin'
/** Offline longer than this (and not online since): the collapsed "Offline" section. */
export const LONG_OFFLINE_S = 3600

/** Labels the hub sets: not shown as chips (the card says it otherwise). */
const HIDDEN_LABELS = new Set(['os', 'arch', ADMIN_LABEL])

export function isAdminNode(n?: NodeInfo): boolean {
  return !!n && (n.admin_worker === true || n.labels?.[ADMIN_LABEL] === 'true')
}

function shortHost(h?: string): string {
  return (h ?? '').split('.')[0].toLowerCase()
}

/** Whether session and admin are one machine's session worker and admin worker. */
export function sameMachine(session: NodeInfo, admin: NodeInfo): boolean {
  const worker = (n: NodeInfo) => !n.role || n.role === 'worker'
  if (!worker(session) || !worker(admin) || session.state === 'revoked' || admin.state === 'revoked') return false
  if (isAdminNode(session) || !isAdminNode(admin) || session.name === admin.name) return false
  if (!admin.owner || session.owner !== admin.owner) return false
  const a = shortHost(session.hostname)
  const b = shortHost(admin.hostname)
  return !a || !b || a === b
}

/** The session worker an admin worker may pair with ("" = none). */
function candidate(admin: NodeInfo): string {
  if (admin.companion) return admin.companion
  if (admin.machine && admin.machine !== admin.name) return admin.machine
  return admin.name.endsWith(ADMIN_SUFFIX) ? admin.name.slice(0, -ADMIN_SUFFIX.length) : ''
}

export interface Group<T> {
  /** the machine's name: its session worker's, else the admin worker's */
  key: string
  session?: T
  admin?: T
}

/**
 * Groups items (workers, by name, with their hub NodeInfo if known) into
 * machines, in the items' order: a session worker and its admin worker
 * become one group; everything else is a group of its own (an admin worker
 * alone: admin only).
 */
export function groupMachines<T>(items: T[], name: (t: T) => string, node: (t: T) => NodeInfo | undefined): Group<T>[] {
  const byName = new Map(items.map((t) => [name(t), t]))
  const pairOf = new Map<string, string>() // session -> admin
  const paired = new Set<string>()
  for (const t of items) {
    const n = node(t)
    if (!n || !isAdminNode(n)) continue
    const s = byName.get(candidate(n))
    const sn = s && node(s)
    if (!s || !sn || pairOf.has(name(s)) || !sameMachine(sn, n)) continue
    pairOf.set(name(s), name(t))
    paired.add(name(t))
  }
  const out: Group<T>[] = []
  for (const t of items) {
    const k = name(t)
    if (paired.has(k)) continue
    if (pairOf.has(k)) out.push({ key: k, session: t, admin: byName.get(pairOf.get(k)!) })
    else if (isAdminNode(node(t))) out.push({ key: k, admin: t })
    else out.push({ key: k, session: t })
  }
  return out
}

/** A machine in the tray's pool view. */
export interface PoolMachine extends Group<PoolWorker> {
  /** the worker whose metrics the card shows: the session worker, else (offline) the admin worker */
  primary: PoolWorker
  /** online | connecting | offline: the best of its workers */
  state: string
  /** seconds since a worker of it was last seen (offline machines) */
  offlineFor: number
  /** offline for more than LONG_OFFLINE_S: the collapsed section */
  longOffline: boolean
  /** labels as chips (the hub's own os / arch / admin labels left out) */
  labels: [string, string][]
}

const RANK: Record<string, number> = { online: 0, connecting: 1, offline: 2 }

function workerState(w: PoolWorker): string {
  return w.state ?? 'connecting'
}

/** Seconds a pool worker has been offline: the hub's last_seen, else since the daemon saw it go. */
export function offlineSeconds(w: PoolWorker, now = Date.now()): number {
  if (workerState(w) !== 'offline') return 0
  const seen = w.node?.last_seen
  if (seen) return since(seen, now)
  return since(w.since, now)
}

/**
 * The tray's machines: grouped, online first (then connecting, then
 * offline; by name within), split into the active list and the
 * long-offline ones (collapsed, compact).
 */
export function poolMachines(workers: PoolWorker[], now = Date.now()): { active: PoolMachine[]; offline: PoolMachine[] } {
  const live = workers.filter((w) => w.node?.state !== 'revoked')
  const groups = groupMachines(
    live,
    (w) => w.name,
    (w) => w.node,
  )
  const machines: PoolMachine[] = groups.map((g) => {
    const members = [g.session, g.admin].filter((w): w is PoolWorker => !!w)
    const state = members.map(workerState).sort((a, b) => (RANK[a] ?? 1) - (RANK[b] ?? 1))[0]
    const offlineFor = state === 'offline' ? Math.min(...members.map((w) => offlineSeconds(w, now))) : 0
    const s = g.session
    const primary = s && (workerState(s) === 'online' || !g.admin || workerState(g.admin) !== 'online') ? s : (g.admin ?? s)!
    const labels = new Map<string, string>()
    for (const w of members) {
      for (const [k, v] of Object.entries(w.node?.labels ?? {})) if (!HIDDEN_LABELS.has(k) && !labels.has(k)) labels.set(k, v)
    }
    return {
      ...g,
      primary,
      state,
      offlineFor,
      longOffline: state === 'offline' && offlineFor > LONG_OFFLINE_S,
      labels: [...labels.entries()].sort(([a], [b]) => a.localeCompare(b)),
    }
  })
  machines.sort((a, b) => (RANK[a.state] ?? 1) - (RANK[b.state] ?? 1) || a.key.localeCompare(b.key))
  return { active: machines.filter((m) => !m.longOffline), offline: machines.filter((m) => m.longOffline) }
}

/** Nodes of the hub panel, grouped: online machines first, revoked last. */
export function nodeMachines(nodes: NodeInfo[]): Group<NodeInfo>[] {
  const groups = groupMachines(
    nodes,
    (n) => n.name,
    (n) => n,
  )
  const rank = (g: Group<NodeInfo>) => {
    const ns = [g.session, g.admin].filter((n): n is NodeInfo => !!n)
    if (ns.every((n) => n.state === 'revoked')) return 3
    if (ns.some((n) => n.online && n.state === 'approved')) return 0
    return ns.some((n) => n.state === 'approved') ? 1 : 2
  }
  return groups.sort((a, b) => rank(a) - rank(b) || a.key.localeCompare(b.key))
}

/** "45s" / "12m" / "3h" / "2d" ago. */
export function fmtLastSeen(sec: number): string {
  sec = Math.max(0, Math.floor(sec))
  if (sec >= 2 * 86400) return `${Math.floor(sec / 86400)}d`
  if (sec < 90) return `${sec}s`
  return sec < 5400 ? `${Math.floor(sec / 60)}m` : `${Math.floor(sec / 3600)}h`
}
