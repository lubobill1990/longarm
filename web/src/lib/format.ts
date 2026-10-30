// Formatting helpers.
import type { Int64, JobSpec, JobState, MetricsSnapshot } from '../api/types'
// The one load-colour table, shared with the tray icon (Go embeds the same
// file): internal/tray/levels.json.
import levelTable from '../../../internal/tray/levels.json'

const LEVELS: { below: number | null; color: string }[] = levelTable.levels

export const GREEN = LEVELS[0].color
export const AMBER = LEVELS[1].color
export const RED = LEVELS[LEVELS.length - 1].color
export const CPU_C = '#58a6ff'
export const MEM_C = '#bc8cff'
export const RTT_C = '#39c5cf'

/** The key of a node: its ID (names can be reused once a node is revoked), else its name (an older hub). */
export function nodeKey(n: { id?: string; name: string }): string {
  return n.id || n.name
}

/**
 * Shortest unambiguous prefix of each ID among ids, at least 8 characters
 * (git-style; internal/nodeid.ShortIDs). Empty IDs are left out.
 */
export function shortIds(ids: (string | undefined)[]): Record<string, string> {
  const sorted = [...new Set(ids.filter((x): x is string => !!x).map((x) => x.toLowerCase()))].sort()
  const common = (a: string, b: string) => {
    let n = 0
    while (n < a.length && n < b.length && a[n] === b[n]) n++
    return n
  }
  const out: Record<string, string> = {}
  sorted.forEach((id, i) => {
    let n = 8
    if (i > 0) n = Math.max(n, common(id, sorted[i - 1]) + 1)
    if (i + 1 < sorted.length) n = Math.max(n, common(id, sorted[i + 1]) + 1)
    if (n < id.length && id[n - 1] === '-') n++
    out[id] = id.slice(0, Math.min(n, id.length))
  })
  return out
}

const THEMED: Record<string, string> = {
  [GREEN]: '--c-green',
  [AMBER]: '--c-amber',
  [RED]: '--c-red',
  [CPU_C]: '--c-cpu',
  [MEM_C]: '--c-mem',
  [RTT_C]: '--c-rtt',
}

/**
 * A palette colour as CSS: the theme's variant where the page defines one
 * (the tray's tokens.css: --c-green ...), else the colour itself (the
 * panel). For style props only (SVG attributes don't take var()).
 */
export function cssColor(c: string): string {
  const v = THEMED[c.toLowerCase()]
  return v ? `var(${v}, ${c})` : c
}

/** green < 60 % <= amber < 85 % <= red (levels.json). */
export function levelColor(pct: number): string {
  for (const l of LEVELS) {
    if (l.below === null || pct < l.below) return l.color
  }
  return RED
}

/** "3d 4h" / "5h 12m" (fmt_uptime). */
export function fmtUptime(sec: number): string {
  sec = Math.max(0, Math.floor(sec))
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  return d ? `${d}d ${h}h` : `${h}h ${m}m`
}

/** "45s" / "12m" / "3h" (fmt_ago). */
export function fmtAgo(sec: number): string {
  sec = Math.max(0, Math.floor(sec))
  return sec < 90 ? `${sec}s` : sec < 5400 ? `${Math.floor(sec / 60)}m` : `${Math.floor(sec / 3600)}h`
}

export function num(v: Int64 | undefined | null): number {
  if (v === undefined || v === null || v === '') return 0
  return typeof v === 'number' ? v : Number(v)
}

export const GiB = 1024 ** 3

export function gb(v: Int64 | undefined): number {
  return num(v) / GiB
}

export function memPct(m?: MetricsSnapshot): number {
  if (!m) return 0
  const total = num(m.mem_total_bytes)
  return total > 0 ? (100 * num(m.mem_used_bytes)) / total : 0
}

export function fmtBytes(v: Int64 | undefined): string {
  const b = num(v)
  if (b >= GiB) return `${(b / GiB).toFixed(1)}G`
  if (b >= 1024 ** 2) return `${Math.round(b / 1024 ** 2)}M`
  if (b >= 1024) return `${Math.round(b / 1024)}K`
  return `${b}B`
}

export function fmtRTT(ms?: number): string {
  if (!ms || ms <= 0) return '—'
  return ms < 10 ? `${ms.toFixed(1)} ms` : `${Math.round(ms)} ms`
}

/** Seconds since an RFC 3339 time (0 if missing). */
export function since(t?: string, now = Date.now()): number {
  if (!t) return 0
  const v = Date.parse(t)
  return Number.isNaN(v) ? 0 : Math.max(0, (now - v) / 1000)
}

export function stateName(s?: JobState | string): string {
  return (s ?? 'JOB_STATE_UNSPECIFIED').replace('JOB_STATE_', '').toLowerCase()
}

export function isActive(s?: JobState | string): boolean {
  return ['queued', 'assigned', 'preparing', 'running'].includes(stateName(s))
}

export function stateColor(s?: JobState | string): string {
  switch (stateName(s)) {
    case 'running':
    case 'preparing':
      return CPU_C
    case 'done':
      return GREEN
    case 'failed':
    case 'lost':
      return RED
    case 'killed':
      return AMBER
  }
  return '#8b949e'
}

export function fmtTime(t?: string): string {
  if (!t) return '—'
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return '—'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function b64ToBytes(data?: string): Uint8Array {
  const bin = atob(data ?? '')
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return bytes
}

/** Decodes base64 log data as UTF-8 (invalid bytes become U+FFFD). */
export function decodeB64(data?: string): string {
  return data ? new TextDecoder('utf-8').decode(b64ToBytes(data)) : ''
}

/**
 * A streaming log decoder: UTF-8 characters and ANSI escape sequences
 * split across chunks are joined before decoding / stripping.
 */
export function logDecoder(): (data?: string) => string {
  const dec = new TextDecoder('utf-8')
  let pending = ''
  // a complete escape sequence at the start of a string
  // eslint-disable-next-line no-control-regex
  const complete = /^\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07]*(\x07|\x1b\\)|[^[\]])/
  return (data) => {
    let t = pending + dec.decode(b64ToBytes(data), { stream: true })
    pending = ''
    const esc = t.lastIndexOf('\x1b')
    // an escape sequence that may continue in the next chunk
    if (esc >= 0 && t.length - esc < 64 && !complete.test(t.slice(esc))) {
      pending = t.slice(esc)
      t = t.slice(0, esc)
    }
    return stripAnsi(t)
  }
}

/** Strips ANSI escape sequences (logs of TUIs). */
export function stripAnsi(s: string): string {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*(\x07|\x1b\\)/g, '')
}

/** "tcp" / "longnet relay-udp" / "tailscale derp" -> "relayed" when not direct. */
export function isRelayed(kind?: string): boolean {
  return !!kind && kind !== 'direct' && kind !== 'unknown'
}

/** The user a job belongs to: spec.owner, else the name in "role/name" submitted_by. */
export function jobOwner(spec: JobSpec): string {
  if (spec.owner) return spec.owner
  const by = spec.submitted_by ?? ''
  const i = by.indexOf('/')
  return i >= 0 ? by.slice(i + 1) : by
}

/** Seconds until an RFC 3339 time (negative once past, 0 if missing). */
export function until(t?: string, now = Date.now()): number {
  if (!t) return 0
  const v = Date.parse(t)
  return Number.isNaN(v) ? 0 : (v - now) / 1000
}
