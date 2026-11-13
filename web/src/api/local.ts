// longarm-tray's API: the local control APIs of longarm daemon (/local/longarm/...) and
// longarmd (/local/longarmd/...) proxied by longarm-tray, plus the tray's own desktop
// actions (/trayapi/...: open a terminal, open a URL, pin the popup ...).
import { get, post, request, sse } from './http'
import type { FirewallFixResult } from '../components/common'
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
  /** the firewall card can allow the program (an administrator prompt; Windows) */
  firewall_fix?: boolean
  /** where the longarm.exe installed with the tray is */
  install_dir?: string
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
  /** what of it is not signed in (older trays: absent) */
  need?: SignInTargets
  targets: SignInTargets
  /** the worker: session (runs as you, at logon) or service */
  run_mode?: string
  admin?: AdminStatus
  /** when the sign-in (certificate) ends; expired: it has */
  expires?: string
  expired?: boolean
  /** valid here, but the hub refuses that certificate (removed, revoked): sign in again */
  client_refused?: boolean
  worker_refused?: boolean
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

/** One enrollment this machine holds already. */
export interface SignInHeld {
  kind: 'device' | 'worker'
  /** "admin/alice-pc" */
  identity: string
  /** a device's user, a worker's owner */
  user?: string
  hub?: string
  same_hub: boolean
  expired?: boolean
}

/** A sign-in that would replace what this machine is signed in as. */
export interface SignInConfirm {
  hub: string
  other_hub?: boolean
  current: SignInHeld[]
  /** the admin worker is signed in and stays its owner's */
  admin_worker_stays?: boolean
  request: { hub?: string; labels?: string }
}

export interface SignInState {
  state: 'idle' | 'running' | 'done' | 'error' | 'canceled' | 'confirm'
  confirm?: SignInConfirm
  hub?: string
  code?: { url: string; code: string; expires: string; kind: string; host: string; admin_worker?: boolean }
  steps?: string[]
  result?: { user?: string; hub: string; device?: string; worker?: string; admin_worker?: string; summary: string }
  error?: string
  targets: SignInTargets
}

export const trayApi = {
  info: () => get<TrayInfo>('/trayapi/info'),
  /** Longarm's license and the third-party notices (plain text) */
  licenses: () => get<string>('/trayapi/licenses'),
  attach: (worker: string, session?: string) => post('/trayapi/attach', { worker, session }),
  ssh: (worker: string) => post('/trayapi/ssh', { worker }),
  desktop: (worker: string) => post('/trayapi/desktop', { worker }),
  open: (url: string) => post('/trayapi/open', { url }),
  openPanel: (path = '/') => post('/trayapi/panel', { path }),
  /** opens (or focuses) the Sessions window, filtered to a worker and agent, or at a session */
  openSessions: (to: { worker?: string; agent?: string; key?: string } = {}) => post('/trayapi/sessions/open', to),
  pin: (pinned: boolean) => post<{ pinned: boolean }>('/trayapi/pin', { pinned }),
  hide: () => post('/trayapi/hide'),
  startDaemon: () => post('/trayapi/daemon/start'),
  join: (hub: string, code: string, fingerprint: string, name: string) =>
    post<{ output: string }>('/trayapi/join', { hub, code, fingerprint, name }),
  signinState: () => get<SignInState>('/trayapi/signin/state'),
  /** needed: only what is not signed in (the Sign in card), else everything (Switch account) */
  signinStart: (hub = '', labels = '', replace = false, needed = false) =>
    post<SignInState>('/trayapi/signin/start', { hub, labels, replace, needed }),
  signinCancel: () => post('/trayapi/signin/cancel'),
  /** forget a finished sign-in's result (its banner's ×) */
  signinDismiss: () => post<SignInState>('/trayapi/signin/dismiss'),
  signout: () => post<{ output: string }>('/trayapi/signout'),
  adminDisable: () => post<{ output: string }>('/trayapi/admin/disable'),
  rolesChange: (want: AccountRoles) => post<RolesState>('/trayapi/roles', want),
  rolesState: () => get<RolesState>('/trayapi/roles/state'),
  /** allow the program the daemon (client) or longarmd (worker) runs from, elevated */
  firewallAllow: (target: 'client' | 'worker') => post<FirewallFixResult>('/trayapi/firewall/allow', { for: target }),
  // the update banner: the hub's newest release; install = download, verify (sha256 + signature), run the MSI
  update: (refresh = false) => get<TrayUpdate>(`/trayapi/update${refresh ? '?refresh=1' : ''}`),
  installUpdate: (version: string) => post<{ state: string }>('/trayapi/update/install', { version }),
  /** "Add a machine": the hub, its newest stable release, where to download from */
  addMachine: () => get<AddMachineInfo>('/trayapi/addmachine'),
}

/** GET /trayapi/addmachine */
export interface AddMachineInfo {
  /** the hub's domain ("" = no hub configured) */
  hub_host: string
  /** the hub's newest stable release ("" = unknown: the latest GitHub Release) */
  version?: string
  /** the hub's Entra tenant (an older hub: absent) */
  tenant?: string
  /** where the installers come from: github.com/OWNER/REPO (the hub's repository, else this tray's; '' = unknown) */
  repo: string
}

/** GET /trayapi/update */
export interface TrayUpdate {
  current: string
  latest?: string
  /** the hub's own version (its /healthz) */
  hub?: string
  /** where the hub sends feedback and bug reports (its /healthz; missing: an older hub) */
  feedback_url?: string
  /** the source repository the hub points at (its /healthz; missing: no hub or an older one) */
  repo_url?: string
  /** a newer release with an MSI for this machine */
  available: boolean
  msi?: string
  /** "" | downloading | installing | failed */
  state?: string
  error?: string
}

export const longarmApi = {
  status: () => get<ClientStatus>('/local/longarm/v1/status'),
  pool: (history = true) => get<PoolView>(`/local/longarm/v1/pool?history=${history ? 1 : 0}`),
  watchPool: (onView: (v: PoolView) => void, onDone?: (err?: string) => void) =>
    sse('/local/longarm/v1/pool/watch?history=1', { pool: (d) => onView(d as PoolView) }, onDone),
  /** summary: without the command, env and workspace (a list's columns); job() has them */
  jobs: (q: { mine?: boolean; active?: boolean; worker?: string; limit?: number; summary?: boolean } = {}) => {
    const p = new URLSearchParams()
    if (q.mine) p.set('mine', '1')
    if (q.summary) p.set('summary', '1')
    if (q.active) p.set('active', '1')
    if (q.worker) p.set('worker', q.worker)
    if (q.limit) p.set('limit', String(q.limit))
    return get<{ jobs?: FleetJob[] }>(`/local/longarm/v1/jobs?${p}`).then((r) => r.jobs ?? [])
  },
  job: (id: string, worker?: string) => get<FleetJob>(`/local/longarm/v1/jobs/${enc(id)}${worker ? `?worker=${enc(worker)}` : ''}`),
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
  /** longarm worker rename through the daemon: its owner, admins also fleet workers; it keeps its ID */
  renameWorker: (name: string, newName: string) => post<NodeInfo>(`/local/longarm/v1/workers/${enc(name)}/rename`, { new_name: newName }),
  /** longarm worker update <name> --wait through the daemon (the worker's owner): starts it */
  updateWorker: (name: string, version = '') =>
    post<WorkerUpdateJob>(`/local/longarm/v1/workers/${enc(name)}/update`, version ? { version } : { latest: true }),
  /** the state of that update */
  workerUpdate: (name: string) => get<WorkerUpdateJob>(`/local/longarm/v1/workers/${enc(name)}/update`),
}

/** longarm worker update's outcome (--json). */
export interface WorkerUpdateOutcome {
  worker: string
  method?: 'msi' | 'self'
  from?: string
  to?: string
  phase: 'up_to_date' | 'requested' | 'done' | 'failed'
  summary: string
  jobs: number
  kept_jobs: number
  sessions: number
  kept_sessions: number
  companion?: string
  /** the admin worker that took the request (a user-session worker too old to reach the Longarm Update service) */
  via?: string
  code?: string
  error?: string
}

/** An update the daemon runs for the tray (GET /v1/workers/{name}/update). */
export interface WorkerUpdateJob {
  state: 'idle' | 'running' | 'done' | 'failed'
  /** the tray's own: when it started the update and to which version (an older worker's card shows "updating" from them) */
  started?: number
  to?: string
  progress?: string[]
  outcome?: WorkerUpdateOutcome
  error?: string
  exit_code?: number
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
  /** "Start Longarm at login", as the OS has it (older trays: absent) */
  autostart?: AutostartState
}

/** The tray's login item: the Run key (Windows), a LaunchAgent (macOS), an autostart .desktop file (Linux). */
export interface AutostartState {
  /** this OS has one (else no switch) */
  supported: boolean
  enabled: boolean
  /** the installer's entry for every user of this computer (Windows MSI): off asks for an administrator */
  machine_wide?: boolean
  where?: string
  error?: string
}

/** This machine's Longarm Update service (Windows MSI installs), through the tray. */
export interface LocalUpdateStatus {
  /** the service is installed (else: no automatic updates, the tray's banner uses UAC) */
  service: boolean
  installed?: string
  available?: string
  state?: string
  target?: string
  message?: string
  auto?: boolean
  auto_interval_hours?: number
  /** the release channel this machine follows: stable | preview | dev (older services: absent = stable) */
  channel?: string
  last?: { from: string; to: string; ok: boolean; error?: string; at: string }
  /** the tray's user may change automatic updates (the machine's owner, or an administrator) */
  can_set_auto?: boolean
  error?: string
}

export const localUpdateApi = {
  /** the service's status (the update banner installs through it when present: trayApi.installUpdate) */
  status: () => get<LocalUpdateStatus>('/trayapi/update/service'),
  setAuto: (auto: boolean, hours = 0) => post<LocalUpdateStatus>('/trayapi/update/auto', { auto, hours }),
  /** the machine's release channel (the owner or an administrator) */
  setChannel: (channel: string) => post<LocalUpdateStatus>('/trayapi/update/channel', { channel }),
}

export const traySettingsApi = {
  get: () => get<TraySettings>('/trayapi/settings'),
  set: (s: { mute_workers?: boolean; mute_jobs?: boolean; theme?: 'system' | 'light' | 'dark'; telemetry_disabled?: boolean; autostart?: boolean }) =>
    post<TraySettings>('/trayapi/settings', s),
  /** opens a known folder (never a path from the page) in the file manager */
  openFolder: (which: 'client-logs' | 'worker-logs' | 'diag') => post<{ folder: string }>('/trayapi/folder', { which }),
  /** starts this machine's worker the way it runs: the session task or the service */
  startWorker: () => post<{ output: string; mode: string }>('/trayapi/worker/start'),
  /** longarm diag into the diag folder (nothing uploaded), then opens it */
  collectDiag: () => post<{ zip: string; output: string }>('/trayapi/diag'),
}

/** One coding agent's copy of the Longarm skill (internal/agentskill.Status). */
export interface AgentSkillStatus {
  agent: string
  name: string
  detected: boolean
  detected_by?: string
  /** the skill's folder in the agent's user-global skills directory */
  dir: string
  /** absent | current | outdated | modified (changed by the user) | foreign (not Longarm's) */
  state: 'absent' | 'current' | 'outdated' | 'modified' | 'foreign'
  installed_version?: string
  bundled_version: string
  modified?: string[]
  extra?: string[]
  note?: string
}

export interface AgentSkills {
  bundled_version: string
  agents: AgentSkillStatus[]
  message?: string
}

/** The Longarm agent skill: opt-in install into each agent's skills directory (Settings). */
export const skillsApi = {
  list: () => get<AgentSkills>('/trayapi/skills'),
  install: (agent: string) => post<AgentSkills>('/trayapi/skills/install', { agent }),
  remove: (agent: string) => post<AgentSkills>('/trayapi/skills/remove', { agent }),
}
