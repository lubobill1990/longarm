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

