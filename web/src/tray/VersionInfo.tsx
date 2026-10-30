import { useEffect, useRef, useState } from 'react'
import { longarmApi, longarmdApi, trayApi, type TrayInfo, type TrayUpdate } from '../api/local'
import type { PoolView } from '../api/types'
import { useAction } from '../components/common'
import { compareVersions } from '../lib/release'

/** One line of the version details. */
export interface VersionRow {
  key: 'tray' | 'daemon' | 'worker' | 'admin' | 'hub'
  label: string
  version?: string
  /** behind: older than it should be (the tray, or the newest release) */
  behind?: boolean
  note?: string
}

export interface Versions {
  tray: string
  /** the newest release the hub has */
  latest?: string
  daemon?: string
  worker?: string
  admin?: string
  hub?: string
  /** which parts this machine has (a row each) */
  client: boolean
  workerRole: boolean
  adminWorker: boolean
}

const parses = (v?: string) => !!v && /^v?\d+(\.\d+)*/.test(v.trim())
/** a older than b, both releases (development builds compare with nothing) */
const older = (a?: string, b?: string) => parses(a) && parses(b) && compareVersions(a!, b!) < 0
const newer = (a?: string, b?: string) => parses(a) && parses(b) && compareVersions(a!, b!) > 0

/**
 * The tray's version details: this tray, the client daemon, this machine's
 * worker and admin worker, the hub - each marked when it is behind: the
 * tray behind the newest release, a daemon or worker behind the tray (one
 * MSI installs them together: it was not restarted, or not updated).
 */
export function versionRows(v: Versions): VersionRow[] {
  const rows: VersionRow[] = []
  const tray: VersionRow = { key: 'tray', label: 'longarm-tray', version: v.tray }
  if (older(v.tray, v.latest)) Object.assign(tray, { behind: true, note: `Longarm ${v.latest} is available` })
  rows.push(tray)
  const part = (key: VersionRow['key'], label: string, version: string | undefined, what: string, missing = 'not running') => {
    const r: VersionRow = { key, label, version }
    if (!version) r.note = missing
    else if (older(version, v.tray)) Object.assign(r, { behind: true, note: `older than this tray: ${what}` })
    else if (newer(version, v.tray)) Object.assign(r, { behind: true, note: 'newer than this tray: restart the tray' })
    rows.push(r)
  }
  if (v.client) part('daemon', 'longarm daemon', v.daemon, 'restart it, or update')
  if (v.workerRole) part('worker', 'worker (longarmd)', v.worker, 'restart it, or update')
  if (v.adminWorker) part('admin', 'admin worker', v.admin, 'update this machine', 'unknown: not in your workers list')
  const hub: VersionRow = { key: 'hub', label: 'hub', version: v.hub }
  if (!v.hub) hub.note = 'unknown'
  else if (older(v.hub, v.tray)) Object.assign(hub, { behind: true, note: 'older than this machine (its admins update it)' })
  rows.push(hub)
  return rows
}

/** The admin worker of the machine whose session worker is named worker. */
function adminVersion(pool: PoolView | undefined, worker: string | undefined) {
  if (!worker) return undefined
  return pool?.workers?.find((w) => w.node?.admin_worker && (w.node.machine === worker || w.node.companion === worker))?.node?.version
}

/**
 * "v0.1.21" in the title bar: hover for a summary, click for the details
 * (this tray, the client daemon, the worker and admin worker, the hub), a
 * dot when one is behind, and the update when the hub has a newer release.
 */
export function VersionChip({ info, pool }: { info?: TrayInfo; pool?: PoolView }) {
  const [open, setOpen] = useState(false)
  const [upd, setUpd] = useState<TrayUpdate>()
  const [daemon, setDaemon] = useState<string>()
  const [worker, setWorker] = useState<{ name?: string; version?: string }>()
  const { run, busy } = useAction()
  const wrap = useRef<HTMLSpanElement>(null)
  const clientUp = !!info?.client.running
  const workerRole = !!info?.roles.worker
  useEffect(() => {
    let alive = true
    const load = () => {
      trayApi
        .update()
        .then((u) => alive && setUpd(u))
        .catch(() => {})
      if (clientUp)
        longarmApi
          .status()
          .then((s) => alive && setDaemon(s.version))
          .catch(() => alive && setDaemon(undefined))
      if (workerRole)
        longarmdApi
          .status()
          .then((s) => alive && setWorker({ name: s.name, version: s.version }))
          .catch(() => alive && setWorker(undefined))
    }
    load()
    const t = setInterval(load, open ? 5000 : 60000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [open, clientUp, workerRole])
  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent) => !wrap.current?.contains(e.target as Node) && setOpen(false)
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [open])
  if (!info?.version) return null
  const rows = versionRows({
    tray: info.version,
    latest: upd?.latest,
    daemon,
    worker: worker?.version,
    admin: adminVersion(pool, worker?.name),
    hub: upd?.hub,
    client: info.roles.client,
    workerRole,
    adminWorker: !!info.account?.admin?.enabled,
  })
  const behind = rows.filter((r) => r.behind)
  const label = /^\d/.test(info.version) ? `v${info.version}` : info.version
  const summary = rows.map((r) => `${r.label} ${r.version ?? '-'}${r.behind ? ' (behind)' : ''}`).join('\n')
  const latest = upd?.latest ?? ''
  return (
    <span className="ver-wrap" ref={wrap}>
      <button
        className={`ver-chip ${behind.length ? 'warn' : ''}`}
        data-testid="version-chip"
        aria-haspopup="dialog"
        aria-expanded={open}
        title={summary}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => e.key === 'Escape' && setOpen(false)}
      >
        {label}
        {behind.length > 0 && <span className="ver-dot" aria-label="something is behind" />}
      </button>
      {open && (
        <span className="ver-pop" role="dialog" aria-label="Versions" data-testid="version-details" onKeyDown={(e) => e.key === 'Escape' && setOpen(false)}>
          <span className="ver-head">Versions</span>
          {rows.map((r) => (
            <span key={r.key} className={`ver-row ${r.behind ? 'behind' : ''}`} data-testid={`version-${r.key}`}>
              <span className="ver-label">{r.label}</span>
              <span className="ver-val mono">{r.version ?? '—'}</span>
              {r.note && <span className="ver-note">{r.note}</span>}
            </span>
          ))}
          {upd?.latest && <span className="ver-latest small muted">newest release at the hub: {upd.latest}</span>}
          {upd?.available && (
            <button
              className="primary"
              data-testid="version-update"
              disabled={busy || upd.state === 'downloading' || upd.state === 'installing'}
              onClick={() => run(() => trayApi.installUpdate(latest).then(() => trayApi.update().then(setUpd)))}
            >
              {upd.state === 'downloading' ? 'Downloading…' : `Update to ${latest}`}
            </button>
          )}
        </span>
      )}
    </span>
  )
}
