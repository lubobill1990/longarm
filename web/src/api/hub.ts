// The hub's panel API (/api/..., cookie session after password + TOTP).
import { get, post, put, request, sse } from './http'
import type {
  AuditRecord,
  FleetEvent,
  FleetJob,
  JoinCode,
  LinkReport,
  LogChunk,
  Me,
  MetricsPoint,
  NodeInfo,
  Profile,
  ToolSpec,
} from './types'

const enc = encodeURIComponent

export const hubApi = {
  login: (user: string, password: string, totp: string) => post<{ user: string }>('/api/login', { user, password, totp }),
  logout: () => post('/api/logout'),
  me: () => get<Me>('/api/me'),
  nodes: (all = false) => get<{ nodes?: NodeInfo[] }>(`/api/nodes${all ? '?all=1' : ''}`).then((r) => r.nodes ?? []),
  approve: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/approve`),
  revoke: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/revoke`),
  drain: (name: string, on: boolean) => post<NodeInfo>(`/api/nodes/${enc(name)}/${on ? 'drain' : 'undrain'}`),
  setLabels: (name: string, labels: Record<string, string>) => put<NodeInfo>(`/api/nodes/${enc(name)}/labels`, labels),
  assignProfile: (name: string, profile: string) => put<NodeInfo>(`/api/nodes/${enc(name)}/profile`, { profile }),
  toolchainApply: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/toolchain/apply`),
  jobs: (q: { worker?: string; states?: string[]; limit?: number } = {}) => {
    const p = new URLSearchParams()
    if (q.worker) p.set('worker', q.worker)
    for (const s of q.states ?? []) p.append('state', s)
    if (q.limit) p.set('limit', String(q.limit))
    return get<{ jobs?: FleetJob[] }>(`/api/jobs?${p}`).then((r) => r.jobs ?? [])
  },
  job: (id: string, worker?: string) => get<FleetJob>(`/api/jobs/${enc(id)}${worker ? `?worker=${enc(worker)}` : ''}`),
  submit: (body: { worker: string; command: string; name?: string; cwd?: string; require?: Record<string, string> }) =>
    post<FleetJob>('/api/jobs', body),
  kill: (id: string, worker?: string) => post<FleetJob>(`/api/jobs/${enc(id)}/kill${worker ? `?worker=${enc(worker)}` : ''}`),
  logs: (id: string, worker: string | undefined, onChunk: (c: LogChunk) => void, onDone: (err?: string) => void, follow = true) =>
    sse(
      `/api/jobs/${enc(id)}/logs?follow=${follow ? 1 : 0}${worker ? `&worker=${enc(worker)}` : ''}`,
      { log: (d) => onChunk(d as LogChunk) },
      onDone,
      { reconnect: false },
    ),
  events: (kinds: string[], onEvent: (e: FleetEvent) => void) => {
    const h: Record<string, (d: unknown) => void> = {}
    for (const k of kinds) h[k] = (d) => onEvent(d as FleetEvent)
    return sse(`/api/events?kinds=${kinds.join(',')}`, h)
  },
  history: () => get<{ nodes?: { node: string; points?: MetricsPoint[] }[] }>('/api/history').then((r) => r.nodes ?? []),
  links: () => get<{ reports?: LinkReport[] }>('/api/links').then((r) => r.reports ?? []),
  joinCodes: () => get<{ codes?: JoinCode[] }>('/api/joincodes').then((r) => r.codes ?? []),
  createJoinCode: (body: { role: string; ttl?: string; labels?: Record<string, string>; profile?: string; name?: string }) =>
    post<JoinCode>('/api/joincodes', body),
  install: (code: string, role: string) =>
    get<{ windows: string; linux: string; longarmd: string }>(`/api/install?code=${enc(code)}&role=${enc(role)}`),
  profiles: () => get<{ profiles?: Profile[] }>('/api/profiles').then((r) => r.profiles ?? []),
  putProfile: (name: string, tools: ToolSpec[]) => put<Profile>(`/api/profiles/${enc(name)}`, { tools }),
  audit: (node?: string, limit = 200) =>
    get<{ records?: AuditRecord[] }>(`/api/audit?limit=${limit}${node ? `&node=${enc(node)}` : ''}`).then((r) => r.records ?? []),
  users: () => get<{ users: { name: string; created_at?: string }[] }>('/api/users').then((r) => r.users ?? []),
  addUser: (name: string, password: string, current_password: string, reset = false) =>
    post<{ name: string; totp_secret: string; otpauth: string }>('/api/users', { name, password, reset, current_password }),
  deleteUser: (name: string, current_password: string) => request('DELETE', `/api/users/${enc(name)}`, { current_password }),
}
