// An update of a worker's machine as the hub shows it (NodeInfo.update;
// internal/hub/updatestate.go): the tray's card, the panel's nodes and
// longarm ls (cmd/longarm/updatestate.go) say the same.
import type { NodeInfo, NodeUpdate } from '../api/types'
import { fmtLastSeen } from './machines'
import { since } from './format'

/** In progress (not over): requested, updating, timed out (it may still come back). */
export function updateActive(u?: NodeUpdate): boolean {
  return u?.state === 'requested' || u?.state === 'updating' || u?.state === 'timed_out'
}

/** Updating right now (the card shows "Updating…" instead of offline / link errors). */
export function updateRunning(u?: NodeUpdate): boolean {
  return u?.state === 'requested' || u?.state === 'updating'
}

/** The update of a machine: of its workers (session, admin), the one in progress first. */
export function machineUpdate(nodes: (NodeInfo | undefined)[]): NodeUpdate | undefined {
  const us = nodes.map((n) => n?.update).filter((u): u is NodeUpdate => !!u?.state)
  return us.find(updateRunning) ?? us.find(updateActive) ?? us[0]
}

const v = (x?: string) => (x ?? '').replace(/^v/, '')

function mb(b?: string | number): number {
  return Math.round(Number(b ?? 0) / (1 << 20))
}

/** "downloading 12/42 MB", "verifying", "installing", "restarting", "checking the new version", "" (unknown). */
export function phaseText(u: NodeUpdate): string {
  switch (u.phase) {
    case 'downloading':
      return Number(u.bytes_total ?? 0) > 0 ? `downloading ${mb(u.bytes_done)}/${Math.max(1, mb(u.bytes_total))} MB` : 'downloading'
    case 'health_check':
      return 'checking the new version'
    case undefined:
    case '':
      return ''
  }
  return u.phase.replace(/_/g, ' ')
}

const INITIATORS: Record<string, string> = {
  owner: 'its owner',
  rollout: 'a rollout',
  auto: 'automatic updates',
  local: 'someone on the machine',
}

/** What the card, the panel and a confirmation say about an update. */
export interface UpdateView {
  kind: 'updating' | 'requested' | 'timed_out' | 'failed' | 'mismatch' | 'restart_pending'
  /** the headline: "Updating to v0.1.30…", "Update to v0.1.30 failed: …" */
  title: string
  /** the phase / what next: "downloading 12/42 MB · 2m · by a rollout" */
  detail: string
  /** 0..1 while downloading */
  progress?: number
  /** a badge the user may dismiss (failed, mismatch, timed out, restart pending) */
  badge: boolean
  /** dismissal key: the node and the attempt */
  key: string
}

/** The view of u for a worker that is online or not, at now (ms). */
export function updateView(u: NodeUpdate, online: boolean, now = Date.now(), nodeId = ''): UpdateView {
  const to = `v${v(u.to)}`
  const started = u.started_at ? since(u.started_at, now) : 0
  const ago = u.started_at ? fmtLastSeen(started) : ''
  const by = INITIATORS[u.initiator ?? '']
  const key = `${nodeId}:${u.attempt || u.started_at || u.to}`
  const total = Number(u.bytes_total ?? 0)
  const progress = u.phase === 'downloading' && total > 0 ? Math.min(1, Number(u.bytes_done ?? 0) / total) : undefined
  const parts = (...xs: (string | undefined | false)[]) => xs.filter(Boolean).join(' · ')
  switch (u.state) {
    case 'requested':
      return {
        kind: 'requested',
        title: `Update to ${to} requested`,
        detail: parts(online ? 'waiting for the worker to start it' : 'waiting for the worker to come back', ago && `${ago}`, by && `by ${by}`),
        badge: false,
        key,
      }
    case 'updating': {
      const p = phaseText(u)
      const what = online ? p || 'updating' : `waiting for the worker to come back${p ? ` (last phase: ${p})` : ''}`
      return { kind: 'updating', title: `Updating to ${to}…`, detail: parts(what, ago, by && `by ${by}`), progress, badge: false, key }
    }
    case 'timed_out': {
      const p = phaseText(u) || 'unknown'
      return {
        kind: 'timed_out',
        title: online ? `Update to ${to} didn't finish` : `Offline: update to ${to} didn't finish`,
        detail: `last phase: ${p}${ago ? `, started ${ago} ago` : ''}`,
        badge: true,
        key,
      }
    }
    case 'failed':
      return {
        kind: 'failed',
        title: `Update to ${to} failed: ${u.error || failReason(u.code)}`,
        detail: `still on v${v(u.from)}`,
        badge: true,
        key,
      }
    case 'mismatch':
      return { kind: 'mismatch', title: `Update to ${to}: ${u.error || 'came back on another version'}`, detail: '', badge: true, key }
    case 'restart_pending':
      return {
        kind: 'restart_pending',
        title: `Restart pending: ${to} is installed`,
        detail: 'restart the machine to finish the update (Longarm keeps running meanwhile)',
        badge: true,
        key,
      }
  }
  return { kind: 'updating', title: '', detail: '', badge: false, key }
}

/** A failure code in words (other users see the code only). */
export function failReason(code?: string): string {
  switch (code) {
    case 'signature':
      return 'the package signature is invalid'
    case 'sha256':
      return 'the download is corrupt (sha256 mismatch)'
    case 'disk':
      return 'the disk is full'
    case 'msi':
      return 'the installer failed'
    case 'downgrade':
    case 'refused':
      return 'refused (not newer than the installed version)'
    case 'health':
      return 'the new version failed its health check and was rolled back'
    case 'timeout':
      return "it didn't finish"
    case 'not_found':
      return 'the hub has no such release'
    case 'unavailable':
      return 'the hub or the update service was unreachable'
  }
  return code || 'unknown error'
}

/** The jobs line of a confirmation and of the card while it updates. */
export function jobsNote(u?: NodeUpdate): string {
  if (u && u.state && !u.jobs_keep_running && !u.method) return 'running jobs and sessions should keep running (an older worker: not confirmed)'
  return 'running jobs and sessions keep running; Longarm on the machine restarts for a minute or two'
}

/** "updating 0.1.29→0.1.30 (installing)": the panel's state column (longarm ls says the same). */
export function updateStateText(u: NodeUpdate | undefined, online: boolean): string {
  if (!u?.state) return ''
  const arrow = `${u.from ? v(u.from) : ''}→${v(u.to)}`
  switch (u.state) {
    case 'requested':
      return `update requested ${arrow}`
    case 'updating': {
      const p = phaseText(u)
      const what = online ? p : p ? `restarting, last ${p}` : 'restarting'
      return what ? `updating ${arrow} (${what})` : `updating ${arrow}`
    }
    case 'timed_out':
      return `update to ${v(u.to)} didn't finish (last phase ${phaseText(u) || 'unknown'})`
    case 'failed':
      return `update to ${v(u.to)} failed (${u.code || 'failed'})`
    case 'mismatch':
      return `came back on another version than ${v(u.to)}`
    case 'restart_pending':
      return `restart the machine to finish ${v(u.to)}`
  }
  return ''
}

const DISMISS_KEY = 'longarm.update.dismissed'

/** Badges the user dismissed on this computer (by UpdateView.key). */
export function dismissedUpdates(): Set<string> {
  try {
    return new Set(JSON.parse(localStorage.getItem(DISMISS_KEY) ?? '[]') as string[])
  } catch {
    return new Set()
  }
}

export function dismissUpdate(key: string): void {
  const s = [...dismissedUpdates(), key].slice(-50)
  try {
    localStorage.setItem(DISMISS_KEY, JSON.stringify(s))
  } catch {
    /* private mode: for this session only */
  }
  window.dispatchEvent(new Event('longarm-update-dismissed'))
}
