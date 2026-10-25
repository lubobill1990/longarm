// JSON shapes of the hub API and the local control APIs (protojson with
// proto field names: 64-bit integers arrive as strings, timestamps as
// RFC 3339 strings, enums as their names, bytes as base64).

export type Int64 = string | number

export interface AgentStat {
  sessions?: number
  processes?: number
  mem_bytes?: Int64
}

export interface ProcGroup {
  name: string
  count?: number
  mem_bytes?: Int64
  cpu_percent?: number
}

export interface MetricsSnapshot {
  time?: string
  cpu_percent?: number
  threads?: number
  mem_total_bytes?: Int64
  mem_used_bytes?: Int64
  jobs_running?: number
  uptime_seconds?: Int64
  nproc?: number
  agents?: Record<string, AgentStat>
  top_cpu?: ProcGroup[]
  top_mem?: ProcGroup[]
  disk_free_bytes?: Int64
  disk_total_bytes?: Int64
}

export interface ToolchainReport {
  profile?: string
  revision?: Int64
  state?: string
  error?: string
  versions?: Record<string, string>
  updated_at?: string
}

export interface NodeInfo {
  name: string
  role?: string
  state?: string
  online?: boolean
  draining?: boolean
  drain_reason?: string
  labels?: Record<string, string>
  os?: string
  arch?: string
  version?: string
  hostname?: string
  endpoints?: string[]
  metrics?: MetricsSnapshot
  last_seen?: string
  joined_at?: string
  cert_not_after?: string
  mirrors?: string[]
  profile?: string
  toolchain?: ToolchainReport
  jobs_active?: number
  reserved_gb?: number
  admin_drain?: boolean
  warm_repos?: string[]
  max_jobs?: number
  reserved_mem_gb?: number
  isolation?: string // workers: "shared" | "per-user"
  user?: string // clients/admins: the user the node's certificates belong to
}

export type JobState =
  | 'JOB_STATE_UNSPECIFIED'
  | 'JOB_STATE_QUEUED'
  | 'JOB_STATE_ASSIGNED'
  | 'JOB_STATE_PREPARING'
  | 'JOB_STATE_RUNNING'
  | 'JOB_STATE_DONE'
  | 'JOB_STATE_FAILED'
  | 'JOB_STATE_KILLED'
  | 'JOB_STATE_LOST'

export interface JobSpec {
  id: string
  name?: string
  argv?: string[]
  cwd?: string
  labels?: Record<string, string>
  submitted_by?: string
  owner?: string // the user the job belongs to (older jobs: from submitted_by)
  share_teams?: string[] // read-only sharing: "team" (every team of the owner) or "team:<name>"
}

export interface Job {
  spec: JobSpec
  state?: JobState
  has_exit_code?: boolean
  exit_code?: number
  created_at?: string
  started_at?: string
  ended_at?: string
  log_size?: Int64
  reason?: string
}

export interface FleetJob {
  worker: string
  job: Job
  updated_at?: string
  worker_online?: boolean
}

export interface FleetEvent {
  time?: string
  kind: string
  node?: string
  job?: FleetJob
  node_info?: NodeInfo
  message?: string
}

export interface LogChunk {
  offset?: Int64
  data?: string // base64
}

export interface JoinCode {
  code?: string
  id?: string
  role?: string
  expires_at?: string
  labels?: Record<string, string>
  profile?: string
  created_by?: string
  used?: boolean
  used_by?: string
  name?: string
}

export interface ToolSpec {
  name: string
  version: string
}

export interface Profile {
  name: string
  tools?: ToolSpec[]
  revision?: Int64
  updated_at?: string
  rollout?: string
  canary?: string
  message?: string
}

export interface AuditRecord {
  node?: string
  time?: string
  who?: string
  method?: string
  json?: string
}

export interface MetricsPoint {
  time?: string
  cpu?: number
  mem?: number
}

export interface LinkSample {
  time?: string
  rtt_ms?: number
}

export interface LegStatus {
  transport?: string
  endpoint?: string
  kind?: string
  detail?: string
  nat?: string
  rtt_ms?: number
  up?: boolean
  error?: string
  since?: string
  active?: boolean
  history?: LinkSample[]
  relay_since?: string
  label?: string
}

export interface WorkerLink {
  worker?: string
  legs?: LegStatus[]
  state?: string
  since?: string
  active?: string
  rtt_ms?: number
}

export interface LinkReport {
  client?: string
  time?: string
  links?: WorkerLink[]
}

export interface HubLink {
  address?: string
  connected?: boolean
  error?: string
  since?: string
  node_state?: string
}

export interface PoolWorker {
  name: string
  state?: string
  since?: string
  source?: string
  metrics?: MetricsSnapshot
  history?: MetricsPoint[]
  link?: WorkerLink
  node?: NodeInfo
  error?: string
  my_jobs?: number
}

export interface PoolView {
  time?: string
  source?: string
  hub?: HubLink
  workers?: PoolWorker[]
  transport?: string // transport policy (longarm transport)
  identity?: string
  firewall?: FirewallStatus // the daemon's longarm.exe (Windows only)
}

/** Windows Firewall verdict for longarm.exe / longarmd.exe (unset outside Windows). */
export interface FirewallStatus {
  program?: string
  ok?: boolean
  allowed?: boolean
  blocking?: string[]
  problem?: string
  fix?: string // run elevated
}

export interface ForwardInfo {
  worker?: string
  target?: string
  local_addr?: string
  url?: string
  connections?: Int64
  started_at?: string
  error?: string
}

export interface LocalLimits {
  max_jobs?: number
  reserved_mem_gb?: number
}

export interface WorkerStatus {
  name?: string
  version?: string
  data_dir?: string
  endpoints?: string[]
  hub?: HubLink
  state?: { draining?: boolean; reason?: string; max_jobs?: number; reserved_mem_gb?: number }
  jobs_running?: number
  sessions_running?: number
  toolchain?: ToolchainReport
  cert_not_after?: string
  metrics?: MetricsSnapshot
  revoked_certs?: number
  pid?: number
  limits?: LocalLimits
  os?: string
  arch?: string
  firewall?: FirewallStatus
}

export interface ClientStatus {
  identity?: string
  hub?: HubLink
  forwards?: number
  started_at?: string
  pid?: number
  version?: string
  config_dir?: string
  transport?: string
  firewall?: FirewallStatus
}

export interface Me {
  user: string
  hub: string
  version: string
  hub_address?: string
  ca_fingerprint?: string
  base_url?: string
  role?: 'admin' | 'user' // missing: an older hub (administrator)
  tenant?: string // the Longarm user the login acts as
  teams?: string[]
  tenancy?: boolean // users are separated
  quota?: Quota // effective quota (normalized from protojson camelCase)
  usage?: QuotaUsage
}

// ---------------------------------------------------------------- tenancy

/** A user's quota; zero / missing fields: no limit (or the hub's default). */
export interface Quota {
  max_jobs?: number
  max_mem_gb?: number
  max_jobs_per_worker?: number
  max_mem_gb_per_worker?: number
  max_sessions_per_worker?: number
  job_mem_gb?: number
  max_job_mem_gb?: number
  job_cpus?: number
  max_job_cpus?: number
}

export interface QuotaUsage {
  jobs?: number
  mem_gb?: number
}

/** A Longarm user (a person sharing the hub), not a panel login. */
export interface TenantUser {
  name: string
  teams?: string[]
  quota?: Quota // overrides of the default quota
  disabled?: boolean
  created_at?: string
  created_by?: string
  devices?: string[]
  usage?: QuotaUsage
  panel?: boolean // has a web panel login
}

export interface ListUsersResponse {
  users: TenantUser[]
  default_quota?: Quota
  tenancy?: boolean
}

export interface PutUserRequest {
  name: string
  create?: boolean
  set_teams?: boolean
  teams?: string[]
  set_quota?: boolean
  quota?: Quota
  set_disabled?: boolean
  disabled?: boolean
  join_code?: boolean // also issue a client join code for a device
  ttl?: string // protojson Duration, e.g. "86400s"
  device?: string
  admin?: boolean // the join code issues an admin certificate
  panel?: boolean // create / reset a panel login (password + TOTP, shown once)
}

export interface PutUserResponse {
  user?: TenantUser
  join_code?: JoinCode
  panel_password?: string
  totp_secret?: string
  otpauth?: string
}

export interface RemoveUserResponse {
  revoked?: string[]
  purge?: boolean
}

export interface Team {
  name: string
  members?: string[]
}

/** A web panel account (GET /api/users). */
export interface PanelLogin {
  name: string
  created_at?: string
  role?: string // "admin" | "user"
  tenant?: string // role user: the Longarm user it acts as
}
