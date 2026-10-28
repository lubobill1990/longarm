// Demo data for the tray screenshots (shots/tray.spec.ts): the local APIs
// longarm-tray serves (/trayapi, /local/longarm, /local/longarmd), answered
// in the browser - no tray, daemon or hub runs.

const now = Date.now()
const ago = (s: number) => new Date(now - s * 1000).toISOString()
const GiB = 1024 ** 3

/** A smooth-ish 5-minute series. */
function wave(n: number, base: number, amp: number, phase = 0) {
  return Array.from({ length: n }, (_, i) => Math.max(0, Math.min(100, base + amp * Math.sin(i / 9 + phase) + amp * 0.4 * Math.sin(i / 3.1 + phase * 2))))
}

function history(cpuBase: number, memBase: number, phase = 0) {
  const cpu = wave(150, cpuBase, 14, phase)
  const mem = wave(150, memBase, 3, phase + 1)
  return cpu.map((c, i) => ({ cpu: c, mem: mem[i] }))
}

function rtt(base: number, fail = -1) {
  return Array.from({ length: 150 }, (_, i) => ({ rtt_ms: i === fail ? -1 : base + 4 * Math.sin(i / 5) + (i % 17 === 0 ? 9 : 0) }))
}

function metrics(cpu: number, memUsedG: number, memTotalG: number, claude: number, copilot: number) {
  return {
    time: ago(2),
    cpu_percent: cpu,
    mem_total_bytes: String(memTotalG * GiB),
    mem_used_bytes: String(memUsedG * GiB),
    threads: 3120,
    nproc: 412,
    jobs_running: 2,
    uptime_seconds: String(3 * 86400 + 5 * 3600),
    disk_free_bytes: String(412 * GiB),
    disk_total_bytes: String(1024 * GiB),
    agents: {
      claude: { sessions: claude, processes: claude * 3, mem_bytes: String(claude * 1.4 * GiB) },
      copilot: { sessions: copilot, processes: copilot, mem_bytes: String(copilot * 0.8 * GiB) },
    },
    top_cpu: [
      { name: 'node', count: 21, cpu_percent: 31, mem_bytes: String(5.5 * GiB) },
      { name: 'MsMpEng', count: 1, cpu_percent: 9, mem_bytes: String(0.4 * GiB) },
      { name: 'go', count: 4, cpu_percent: 6, mem_bytes: String(1.2 * GiB) },
    ],
    top_mem: [
      { name: 'node', count: 21, mem_bytes: String(5.5 * GiB) },
      { name: 'Code', count: 14, mem_bytes: String(3.1 * GiB) },
      { name: 'claude', count: 3, mem_bytes: String(2.2 * GiB) },
    ],
  }
}

function node(name: string, extra: Record<string, unknown> = {}) {
  return { name, role: 'worker', state: 'approved', online: true, owner: 'alice', hostname: name.replace(/-admin$/, ''), version: '0.9.2', ...extra }
}

export function pool(opts: { empty?: boolean } = {}) {
  if (opts.empty) return { source: 'hub', hub: { address: 'longarm.example.com:7443', connected: true }, transport: 'race', identity: 'client/alice-laptop', user: 'alice', workers: [] }
  const offline = (name: string, hours: number) => ({
    name,
    state: 'offline',
    source: 'none',
    error: `rpc error: code = Unavailable desc = ${name} unreachable`,
    node: node(name, { online: false, last_seen: ago(hours * 3600) }),
  })
  return {
    time: ago(0),
    source: 'hub',
    hub: { address: 'longarm.example.com:7443', connected: true, since: ago(7200) },
    transport: 'race',
    identity: 'client/alice-laptop',
    user: 'alice',
    workers: [
      {
        name: 'cpc-alice-x7k2q',
        state: 'online',
        since: ago(5 * 3600),
        source: 'hub',
        metrics: metrics(38, 41.2, 64, 3, 1),
        history: history(38, 64),
        my_jobs: 1,
        node: node('cpc-alice-x7k2q', { labels: { pool: 'devbox', os: 'windows' } }),
        link: {
          state: 'online',
          rtt_ms: 12,
          legs: [
            { transport: 'tcp', label: 'tcp', kind: 'direct', up: true, active: true, rtt_ms: 12, history: rtt(12, 120) },
            { transport: 'longnet', label: 'longnet relay-udp', kind: 'relay', up: true, rtt_ms: 48 },
          ],
        },
      },
      {
        name: 'cpc-alice-x7k2q-admin',
        state: 'online',
        since: ago(5 * 3600),
        source: 'hub',
        node: node('cpc-alice-x7k2q-admin', { admin_worker: true, labels: { 'longarm.io/admin': 'true' } }),
      },
      {
        name: 'build-linux-02',
        state: 'online',
        since: ago(26 * 3600),
        source: 'hub',
        metrics: metrics(91, 118, 128, 0, 2),
        history: history(84, 90, 2),
        node: node('build-linux-02', { owner: '', labels: { pool: 'build' } }),
        link: {
          state: 'online',
          rtt_ms: 351,
          legs: [
            { transport: 'tcp', label: 'tcp', kind: 'direct', up: false, error: 'connection refused' },
            {
              transport: 'longnet',
              label: 'longnet relay-udp',
              kind: 'relay',
              nat: 'hard/hard',
              up: true,
              active: true,
              rtt_ms: 351,
              relay_since: ago(380),
              history: rtt(345),
            },
          ],
        },
      },
      {
        name: 'mac-mini',
        state: 'offline',
        since: ago(140),
        source: 'none',
        error: 'connection refused (the machine is restarting?)',
        node: node('mac-mini', { online: false, last_seen: ago(140) }),
      },
      offline('old-devbox', 20),
      offline('e2e-worker', 46),
    ],
  }
}

export const jobs = [
  { worker: 'cpc-alice-x7k2q', job: { spec: { id: 'j-01hf3c9a7d', name: 'build', owner: 'alice', argv: ['pnpm', 'build'] }, state: 'JOB_STATE_RUNNING', created_at: ago(95) } },
  {
    worker: 'build-linux-02',
    job: { spec: { id: 'j-01hf3b2e11', name: 'test-e2e', owner: 'alice', argv: ['go', 'test', './e2e'] }, state: 'JOB_STATE_DONE', has_exit_code: true, exit_code: 0, created_at: ago(1800) },
  },
  {
    worker: 'build-linux-02',
    job: { spec: { id: 'j-01hf3a0c55', name: 'lint', owner: 'alice', argv: ['golangci-lint', 'run'] }, state: 'JOB_STATE_FAILED', has_exit_code: true, exit_code: 1, created_at: ago(5400) },
  },
  { worker: 'cpc-alice-x7k2q', job: { spec: { id: 'j-01hf39aa02', name: '', owner: 'alice', argv: ['npm', 'run', 'dev'] }, state: 'JOB_STATE_KILLED', created_at: ago(86400) } },
]

export const forwards = [
  { worker: 'cpc-alice-x7k2q', target: '3000', local_addr: '127.0.0.1:53000', url: 'http://127.0.0.1:53000/', connections: 4, started_at: ago(600) },
  { worker: 'build-linux-02', target: 'localhost:9229', local_addr: '127.0.0.1:59229', url: 'http://127.0.0.1:59229/', connections: 0, error: 'dial: connection refused' },
]

export function info(o: { needSignIn?: boolean; daemonDown?: boolean } = {}) {
  return {
    version: '0.9.2',
    roles: { client: true, worker: true },
    client: { running: !o.daemonDown, error: o.daemonDown ? 'the pipe is closed' : undefined },
    worker: { running: true, enrolled: true },
    hub_url: 'https://longarm.example.com',
    pinned: true,
    account: {
      signed_in: !o.needSignIn,
      user: o.needSignIn ? undefined : 'alice@example.com',
      hub: 'longarm.example.com',
      hub_from: 'installer',
      needed: !!o.needSignIn,
      targets: { client: true, worker: true, admin_worker: true },
      run_mode: 'session',
      expires: new Date(now + 26 * 86400 * 1000).toISOString(),
      roles: { client: true, worker: true, admin_worker: true },
      roles_editable: true,
      admin: { enabled: true, service: 'longarmd-admin', state: 'running', signed_in: !o.needSignIn, pending: false },
    },
  }
}

export function signinState(kind: 'idle' | 'running') {
  if (kind === 'idle') return { state: 'idle', targets: { client: true, worker: true, admin_worker: true } }
  return {
    state: 'running',
    hub: 'longarm.example.com',
    code: { url: 'https://longarm.example.com/link', code: 'BCDF-GHJK', expires: new Date(now + 9 * 60000).toISOString(), kind: 'both', host: 'ALICE-LAPTOP', admin_worker: true },
    steps: ['contacting longarm.example.com', 'asked for a code for this device, the worker and the admin worker', 'waiting for your approval in the browser'],
    targets: { client: true, worker: true, admin_worker: true },
  }
}

export const workerStatus = {
  name: 'alice-laptop',
  version: '0.9.2',
  os: 'windows',
  arch: 'amd64',
  endpoints: ['tcp://10.0.0.12:7701', 'longnet://relay'],
  hub: { address: 'longarm.example.com:7443', connected: true, since: ago(7200), node_state: 'approved' },
  state: { draining: false },
  jobs_running: 1,
  sessions_running: 2,
  cert_not_after: new Date(now + 26 * 86400 * 1000).toISOString(),
  limits: { max_jobs: 4, reserved_mem_gb: 8 },
  toolchain: { profile: 'devbox-default', revision: 7, state: 'ok', versions: { node: '24.4.0', go: '1.26.1', git: '2.51.0', gh: '' } },
  metrics: metrics(22, 18.5, 32, 1, 0),
}

export const workerJobs = [
  { spec: { id: 'j-01hf3d0001', name: 'nightly-sync', owner: 'alice' }, state: 'JOB_STATE_RUNNING', created_at: ago(300) },
  { spec: { id: 'j-01hf3c0002', name: 'index', owner: 'bob' }, state: 'JOB_STATE_DONE', created_at: ago(7200) },
]

export const traySettings = {
  version: '0.9.2',
  telemetry_disabled: false,
  client_logs: 'C:\\Users\\alice\\AppData\\Roaming\\longarm',
  worker_logs: 'C:\\Users\\alice\\AppData\\Local\\longarmd',
}

export const update = { available: true, current: '0.9.2', latest: '0.10.0', state: 'idle' }
