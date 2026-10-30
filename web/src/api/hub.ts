// The hub's panel API (/api/..., cookie session after password + TOTP).
import { del, get, post, put, request, sse } from './http'
import type {
  AuditRecord,
  FleetEvent,
  FleetJob,
  JoinCode,
  LatestRelease,
  LinkReport,
  ListUsersResponse,
  LogChunk,
  Me,
  MetricsPoint,
  NodeInfo,
  PanelLogin,
  PortLink,
  PortLinkStatus,
  Profile,
  PutUserRequest,
  PutUserResponse,
  Quota,
  QuotaUsage,
  RemoveUserResponse,
  Team,
  TenantUser,
  ToolSpec,
} from './types'

const enc = encodeURIComponent

// Quota field names: proto (snake_case, writePB) and protojson's default
// lowerCamelCase (/api/me embeds plain protojson). Both are accepted.
const QUOTA_FIELDS: [keyof Quota, string][] = [
  ['max_jobs', 'maxJobs'],
  ['max_mem_gb', 'maxMemGb'],
  ['max_jobs_per_worker', 'maxJobsPerWorker'],
  ['max_mem_gb_per_worker', 'maxMemGbPerWorker'],
  ['max_sessions_per_worker', 'maxSessionsPerWorker'],
  ['job_mem_gb', 'jobMemGb'],
  ['max_job_mem_gb', 'maxJobMemGb'],
  ['job_cpus', 'jobCpus'],
  ['max_job_cpus', 'maxJobCpus'],
]

function field(o: Record<string, unknown>, snake: string, camel: string): number | undefined {
  const v = o[snake] ?? o[camel]
  if (v === undefined || v === null || v === '') return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

/** A Quota in either JSON spelling -> snake_case (undefined if not an object). */
export function normQuota(raw: unknown): Quota | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const o = raw as Record<string, unknown>
  const q: Quota = {}
  for (const [snake, camel] of QUOTA_FIELDS) {
    const v = field(o, snake, camel)
    if (v !== undefined) q[snake] = v
  }
  return q
}

export function normUsage(raw: unknown): QuotaUsage | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const o = raw as Record<string, unknown>
  return { jobs: field(o, 'jobs', 'jobs') ?? 0, mem_gb: field(o, 'mem_gb', 'memGb') ?? 0 }
}

function normMe(m: Me): Me {
  return { ...m, quota: normQuota(m.quota), usage: normUsage(m.usage) }
}

function normTenant(u: TenantUser): TenantUser {
  return { ...u, quota: normQuota(u.quota), usage: normUsage(u.usage) }
}

// Port links: protojson with proto names (writePB) or lowerCamelCase (plain
// protojson); both are accepted and kept under the proto (snake_case) names.
function pick(o: Record<string, unknown>, snake: string): unknown {
  return o[snake] ?? o[snake.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase())]
}

function normFields<T>(raw: unknown, fields: string[]): T {
  const o = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>
  const r: Record<string, unknown> = {}
  for (const k of fields) {
    const v = pick(o, k)
    if (v !== undefined && v !== null) r[k] = v
  }
  return r as T
}

const STATUS_FIELDS = ['id', 'state', 'error', 'connections', 'active', 'bytes_sent', 'bytes_received', 'path', 'updated_at', 'refused']
const LINK_FIELDS = ['id', 'owner', 'from', 'to', 'port', 'listen_port', 'created_at', 'expires_at', 'job_id', 'created_by']

/** A PortLink in either JSON spelling -> snake_case. */
export function normPortLink(raw: unknown): PortLink {
  const l = normFields<PortLink>(raw, LINK_FIELDS)
  const st = raw && typeof raw === 'object' ? (raw as Record<string, unknown>).status : undefined
  if (st) l.status = normFields<PortLinkStatus>(st, STATUS_FIELDS)
  return { ...l, id: l.id ?? '' }
}

/** Panel logins with role "user" see their own jobs only (no fleet administration). */
/** A sign-in button of the panel (GET /auth/info): "entra", "microsoft", "google", "apple". */
export interface AuthProvider {
  id: string
  label: string
}

export const isAdmin = (me: Me) => me.role !== 'user'

export const hubApi = {
  login: (user: string, password: string, totp: string) => post<{ user: string }>('/api/login', { user, password, totp }),
  logout: () => post('/api/logout'),
  me: () => get<Me>('/api/me').then(normMe),
  nodes: (all = false) => get<{ nodes?: NodeInfo[] }>(`/api/nodes${all ? '?all=1' : ''}`).then((r) => r.nodes ?? []),
  approve: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/approve`),
  revoke: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/revoke`),
  drain: (name: string, on: boolean) => post<NodeInfo>(`/api/nodes/${enc(name)}/${on ? 'drain' : 'undrain'}`),
  setLabels: (name: string, labels: Record<string, string>) => put<NodeInfo>(`/api/nodes/${enc(name)}/labels`, labels),
  assignProfile: (name: string, profile: string) => put<NodeInfo>(`/api/nodes/${enc(name)}/profile`, { profile }),
  toolchainApply: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/toolchain/apply`),
  // a user's own workers (owner or admin): labels, sharing, removal
  setWorkerLabels: (name: string, labels: Record<string, string>) => post<NodeInfo>(`/api/nodes/${enc(name)}/worker-labels`, labels),
  shareWorker: (name: string, req: { add_users?: string[]; remove_users?: string[]; add_teams?: string[]; remove_teams?: string[]; set_everyone?: boolean; everyone?: boolean }) =>
    post<NodeInfo>(`/api/nodes/${enc(name)}/share`, req),
  removeWorker: (name: string) => post<NodeInfo>(`/api/nodes/${enc(name)}/remove`),
  // public: the login page's sign-in buttons, the newest release
  authInfo: () => get<{ entra?: boolean; mode?: string; providers?: AuthProvider[] }>('/auth/info'),
  latestRelease: () => get<LatestRelease>('/releases/latest.json'),
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
  // port links (longarm link): the login's own, administrators see everybody's
  portLinks: () => get<{ links?: unknown[] }>('/api/port-links').then((r) => (r.links ?? []).map(normPortLink)),
  removePortLink: (id: string) => del<unknown>(`/api/port-links/${enc(id)}`).then(normPortLink),
  joinCodes: () => get<{ codes?: JoinCode[] }>('/api/joincodes').then((r) => r.codes ?? []),
  createJoinCode: (body: { role: string; ttl?: string; labels?: Record<string, string>; profile?: string; name?: string }) =>
    post<JoinCode>('/api/joincodes', body),
  install: (code: string, role: string) =>
    get<{ windows: string; linux: string; longarmd: string }>(`/api/install?code=${enc(code)}&role=${enc(role)}`),
  profiles: () => get<{ profiles?: Profile[] }>('/api/profiles').then((r) => r.profiles ?? []),
  putProfile: (name: string, tools: ToolSpec[]) => put<Profile>(`/api/profiles/${enc(name)}`, { tools }),
  audit: (node?: string, limit = 200) =>
    get<{ records?: AuditRecord[] }>(`/api/audit?limit=${limit}${node ? `&node=${enc(node)}` : ''}`).then((r) => r.records ?? []),
  users: () => get<{ users: PanelLogin[] }>('/api/users').then((r) => r.users ?? []),
  addUser: (name: string, password: string, current_password: string, reset = false) =>
    post<{ name: string; totp_secret: string; otpauth: string }>('/api/users', { name, password, reset, current_password }),
  deleteUser: (name: string, current_password: string) => request('DELETE', `/api/users/${enc(name)}`, { current_password }),
  // multi-tenancy (longarm admin user | team)
  tenants: () =>
    get<Partial<ListUsersResponse>>('/api/tenants').then(
      (r): ListUsersResponse => ({ users: (r.users ?? []).map(normTenant), default_quota: normQuota(r.default_quota), tenancy: !!r.tenancy }),
    ),
  putTenant: (req: PutUserRequest) =>
    post<PutUserResponse>('/api/tenants', req).then((r) => ({ ...r, user: r.user ? normTenant(r.user) : undefined })),
  removeTenant: (name: string, purge = false) => del<RemoveUserResponse>(`/api/tenants/${enc(name)}${purge ? '?purge=1' : ''}`),
  teams: () => get<{ teams?: Team[] }>('/api/teams').then((r) => r.teams ?? []),
  setTeam: (team: string, add: string[], remove: string[] = []) =>
    post<{ teams?: Team[] }>(`/api/teams/${enc(team)}`, { add, remove }).then((r) => r.teams ?? []),
}
