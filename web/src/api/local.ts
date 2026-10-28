// longarm-tray's API: the local control APIs of longarm daemon (/local/longarm/...) and
// longarmd (/local/longarmd/...) proxied by longarm-tray, plus the tray's own desktop
// actions (/trayapi/...: open a terminal, open a URL, pin the popup ...).
import { get, post, request, sse } from './http'
import type {
  ClientStatus,
  FleetJob,
  ForwardInfo,
  Job,
  LocalLimits,
  LogChunk,
  NodeInfo,
  PoolView,
  WorkerStatus,
} from './types'

const enc = encodeURIComponent

export interface TrayInfo {
  version: string
  roles: { client: boolean; worker: boolean }
  client: { running: boolean; error?: string; config_dir?: string }
  worker: { running: boolean; enrolled: boolean; error?: string; data_dir?: string }
  hub_url?: string
  pinned: boolean
  headless?: boolean
  /** this machine's sign-in (older trays: absent) */
  account?: Account
}

/** What a sign-in enrolls: the user's device, the worker, the admin worker. */
export interface SignInTargets {
  client: boolean
  worker: boolean
  admin_worker: boolean
}

/** The optional admin worker (a LocalSystem service only its owner uses). */
export interface AdminStatus {
  enabled: boolean
  service?: string
  state?: string
  signed_in: boolean
  pending: boolean
  error?: string
}

export interface Account {
  signed_in: boolean
  user?: string
  hub?: string
  /** where the hub comes from: config, installer, built-in ("" = ask) */
  hub_from?: string
  /** a sign-in would enroll something */
  needed: boolean
  targets: SignInTargets
  /** the worker: session (runs as you, at logon) or service */
  run_mode?: string
  admin?: AdminStatus
  /** when the sign-in (certificate) ends; expired: it has */
  expires?: string
  expired?: boolean
  /** what this computer does (older trays: absent) */
  roles?: AccountRoles
  /** the roles can be changed here (Windows, installed with the MSI) */
  roles_editable?: boolean
  roles_note?: string
  /** why this looks like a Cloud PC / Dev Box (suggests "run jobs") */
  cloud_pc?: string
}

/** Send jobs (client), run jobs (worker), admin commands (admin worker). */
export interface AccountRoles {
  client: boolean
  worker: boolean
  admin_worker: boolean
}

/** A role change (/trayapi/roles/state). */
export interface RolesState {
  state: 'idle' | 'applying' | 'done' | 'error'
  want: AccountRoles
  message?: string
  error?: string
  /** a sign-in of the added roles started */
  signin?: boolean
}

export interface SignInState {
  state: 'idle' | 'running' | 'done' | 'error' | 'canceled'
  hub?: string
  code?: { url: string; code: string; expires: string; kind: string; host: string; admin_worker?: boolean }
  steps?: string[]
  result?: { user?: string; hub: string; device?: string; worker?: string; admin_worker?: string; summary: string }
  error?: string
  targets: SignInTargets
}

export const trayApi = {
  info: () => get<TrayInfo>('/trayapi/info'),
  attach: (worker: string, session?: string) => post('/trayapi/attach', { worker, session }),
  ssh: (worker: string) => post('/trayapi/ssh', { worker }),
  open: (url: string) => post('/trayapi/open', { url }),
  openPanel: (path = '/') => post('/trayapi/panel', { path }),
  pin: (pinned: boolean) => post<{ pinned: boolean }>('/trayapi/pin', { pinned }),
  hide: () => post('/trayapi/hide'),
  startDaemon: () => post('/trayapi/daemon/start'),
  join: (hub: string, code: string, fingerprint: string, name: string) =>
    post<{ output: string }>('/trayapi/join', { hub, code, fingerprint, name }),
  signinState: () => get<SignInState>('/trayapi/signin/state'),
  signinStart: (hub = '', labels = '') => post<SignInState>('/trayapi/signin/start', { hub, labels }),
  signinCancel: () => post('/trayapi/signin/cancel'),
  signout: () => post<{ output: string }>('/trayapi/signout'),
  adminDisable: () => post<{ output: string }>('/trayapi/admin/disable'),
  rolesChange: (want: AccountRoles) => post<RolesState>('/trayapi/roles', want),
  rolesState: () => get<RolesState>('/trayapi/roles/state'),
}

export const longarmApi = {
  status: () => get<ClientStatus>('/local/longarm/v1/status'),
  pool: (history = true) => get<PoolView>(`/local/longarm/v1/pool?history=${history ? 1 : 0}`),
  watchPool: (onView: (v: PoolView) => void, onDone?: (err?: string) => void) =>
    sse('/local/longarm/v1/pool/watch?history=1', { pool: (d) => onView(d as PoolView) }, onDone),
  jobs: (q: { mine?: boolean; active?: boolean; worker?: string; limit?: number } = {}) => {
    const p = new URLSearchParams()
    if (q.mine) p.set('mine', '1')
    if (q.active) p.set('active', '1')
    if (q.worker) p.set('worker', q.worker)
    if (q.limit) p.set('limit', String(q.limit))
    return get<{ jobs?: FleetJob[] }>(`/local/longarm/v1/jobs?${p}`).then((r) => r.jobs ?? [])
  },
  kill: (id: string, worker?: string) => post<Job>(`/local/longarm/v1/jobs/${enc(id)}/kill${worker ? `?worker=${enc(worker)}` : ''}`),
  logs: (id: string, worker: string | undefined, onChunk: (c: LogChunk) => void, onDone: (err?: string) => void) =>
    sse(
      `/local/longarm/v1/jobs/${enc(id)}/logs?follow=1${worker ? `&worker=${enc(worker)}` : ''}`,
      { log: (d) => onChunk(d as LogChunk) },
      onDone,
      { reconnect: false },
    ),
  forwards: () => get<{ forwards?: ForwardInfo[] }>('/local/longarm/v1/forwards').then((r) => r.forwards ?? []),
  startForward: (worker: string, target: string, local_port = 0) =>
    post<ForwardInfo>('/local/longarm/v1/forwards', { worker, target, local_port }),
  stopForward: (addr: string) => request('DELETE', `/local/longarm/v1/forwards/${enc(addr)}`),
  settings: () => get<{ transport?: string }>('/local/longarm/v1/settings'),
  setTransport: (transport: string) => post<{ transport?: string }>('/local/longarm/v1/settings', { transport }),
  /** longarm admin drain / undrain through the daemon (admin certificates only). */
  drainWorker: (name: string, draining: boolean) => post(`/local/longarm/v1/workers/${enc(name)}/drain`, { draining }),
  /** longarm worker remove through the daemon: the hub revokes the worker (its owner, or an admin). */
  removeWorker: (name: string) => post<NodeInfo>(`/local/longarm/v1/workers/${enc(name)}/remove`),
}

export const longarmdApi = {
  status: () => get<WorkerStatus>('/local/longarmd/v1/status'),
  drain: (draining: boolean, reason = '') => post<WorkerStatus>('/local/longarmd/v1/drain', { draining, reason }),
  limits: (l: LocalLimits) => post<WorkerStatus>('/local/longarmd/v1/limits', l),
  toolchainApply: () => post('/local/longarmd/v1/toolchain/apply'),
  jobs: (limit = 50) => get<{ jobs?: Job[] }>(`/local/longarmd/v1/jobs?limit=${limit}`).then((r) => r.jobs ?? []),
  kill: (id: string) => post<Job>(`/local/longarmd/v1/jobs/${enc(id)}/kill`),
  logs: (id: string, onChunk: (c: LogChunk) => void, onDone: (err?: string) => void) =>
    sse(`/local/longarmd/v1/jobs/${enc(id)}/logs?follow=1`, { log: (d) => onChunk(d as LogChunk) }, onDone, { reconnect: false }),
  leave: (reason: string) => post<{ message?: string }>('/local/longarmd/v1/leave', { reason }),
}


/** The tray's Settings tab (/trayapi/settings): notifications, telemetry, logs. */
export interface TraySettings {
  version: string
  mute_workers?: boolean
  mute_jobs?: boolean
  /** Settings > Appearance: light / dark; absent = follow the system */
  theme?: 'light' | 'dark'
  telemetry_disabled: boolean
  /** LONGARM_TELEMETRY=0 in the environment: off whatever the switch says */
  telemetry_env_off?: boolean
  client_logs?: string
  worker_logs?: string
}

export const traySettingsApi = {
  get: () => get<TraySettings>('/trayapi/settings'),
  set: (s: { mute_workers?: boolean; mute_jobs?: boolean; theme?: 'system' | 'light' | 'dark'; telemetry_disabled?: boolean }) => post<TraySettings>('/trayapi/settings', s),
  /** opens a known folder (never a path from the page) in the file manager */
  openFolder: (which: 'client-logs' | 'worker-logs' | 'diag') => post<{ folder: string }>('/trayapi/folder', { which }),
  /** starts this machine's worker the way it runs: the session task or the service */
  startWorker: () => post<{ output: string; mode: string }>('/trayapi/worker/start'),
  /** longarm diag into the diag folder (nothing uploaded), then opens it */
  collectDiag: () => post<{ zip: string; output: string }>('/trayapi/diag'),
}
