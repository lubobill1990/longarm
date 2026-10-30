import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { NodeInfo, PoolView, PoolWorker } from '../api/types'
import { Toasts } from '../components/common'
import { ClientJobs } from './ClientTab'
import { PoolTab } from './PoolTab'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function mockFetch(routes: Record<string, (init?: RequestInit) => [number, unknown]>) {
  const calls: { url: string; init?: RequestInit }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      const key = Object.keys(routes).find((k) => url.startsWith(k))
      const [status, body] = key ? routes[key](init) : [404, { error: 'not found' }]
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }),
  )
  return calls
}

const now = Date.now()
const ago = (s: number) => new Date(now - s * 1000).toISOString()
const GiB = 1024 ** 3

function w(name: string, over: Partial<PoolWorker> = {}, node: Partial<NodeInfo> = {}): PoolWorker {
  return {
    name,
    state: 'online',
    since: ago(600),
    source: 'hub',
    metrics: {
      cpu_percent: 20,
      mem_total_bytes: String(64 * GiB),
      mem_used_bytes: String(16 * GiB),
      agents: { claude: { sessions: 2, processes: 2, mem_bytes: String(GiB) }, copilot: { sessions: 1, processes: 1 } },
    },
    node: { name, role: 'worker', state: 'approved', online: true, owner: 'alice', hostname: name.replace(/-admin$/, ''), ...node },
    ...over,
  }
}

function devboxPool(identity = 'client/alice-cpc'): PoolView {
  const old = (name: string, hours: number) =>
    w(name, { state: 'offline', source: 'none', metrics: undefined, error: `rpc error: code = Unavailable desc = ${name} unreachable` }, { online: false, last_seen: ago(hours * 3600) })
  return {
    source: 'hub',
    hub: { address: 'hub.example.com:7443', connected: true },
    transport: 'race',
    identity,
    user: 'alice',
    workers: [
      old('old-pair', 20),
      old('old-pc', 21),
      w('cpc-alice-x7k2q', {}, { labels: { pool: 'devbox', os: 'windows' } }),
      w('cpc-alice-x7k2q-admin', {}, { admin_worker: true, labels: { 'longarm.io/admin': 'true' } }),
      old('e2e-worker', 22),
      w('fleet-1', {}, { owner: '' }),
    ],
  }
}

describe('PoolTab', () => {
  it('shows the versions and "Update to X" for the owner; runs the update through the daemon', async () => {
    const pool = devboxPool()
    pool.workers = pool.workers!.map((x) =>
      x.name === 'cpc-alice-x7k2q'
        ? { ...x, node: { ...x.node!, version: '0.1.8', update_available: { version: '0.1.9' } } }
        : x.name === 'cpc-alice-x7k2q-admin'
          ? { ...x, node: { ...x.node!, version: '0.1.7' } }
          : x.name === 'fleet-1'
            ? { ...x, node: { ...x.node!, version: '0.1.8', update_available: { version: '0.1.9' } } }
            : x,
    )
    const calls = mockFetch({
      '/local/longarm/v1/workers/cpc-alice-x7k2q/update': (init) =>
        init?.method === 'POST'
          ? [202, { state: 'running', progress: [] }]
          : [
              200,
              {
                state: 'done',
                outcome: {
                  worker: 'cpc-alice-x7k2q',
                  phase: 'done',
                  summary: 'cpc-alice-x7k2q: 0.1.8 → 0.1.9; 2 jobs and 1 session kept running',
                  jobs: 2,
                  kept_jobs: 2,
                  sessions: 1,
                  kept_sessions: 1,
                },
              },
            ],
    })
    render(
      <Toasts>
        <PoolTab pool={pool} />
      </Toasts>,
    )
    const card = screen.getByTestId('worker-cpc-alice-x7k2q')
    expect(within(card).getByTestId('version').textContent).toBe('v0.1.8 · admin v0.1.7 ↑')
    // a fleet worker (no owner) gets no button: its updates are the hub's rollouts
    expect(within(screen.getByTestId('worker-fleet-1')).queryByTestId('update-btn')).toBeNull()
    fireEvent.click(within(card).getByTestId('update-btn'))
    await waitFor(() => expect(calls.some((c) => c.url.endsWith('/update') && c.init?.method === 'POST')).toBe(true))
    await waitFor(() => expect(within(card).getByTestId('update-line').textContent).toContain('2 jobs and 1 session kept running'), {
      timeout: 5000,
    })
  })

  it('no "Update to" button for workers of other users', () => {
    const pool = devboxPool()
    pool.user = 'bob'
    pool.workers = pool.workers!.map((x) => ({ ...x, node: { ...x.node!, version: '0.1.8', update_available: { version: '0.1.9' } } }))
    render(<PoolTab pool={pool} />)
    expect(screen.queryByTestId('update-btn')).toBeNull()
    expect(within(screen.getByTestId('worker-cpc-alice-x7k2q')).getByTestId('version').textContent).toBe('v0.1.8 ↑')
  })

  it('no "Update to" button when the worker says longarm worker update cannot install it (method none)', () => {
    const pool = devboxPool()
    pool.workers = pool.workers!.map((x) => ({
      ...x,
      node: { ...x.node!, version: '0.1.8', update_available: { version: '0.1.9', method: 'none', reason: 'no Longarm Update service' } },
    }))
    render(<PoolTab pool={pool} />)
    expect(screen.queryByTestId('update-btn')).toBeNull()
    const v = within(screen.getByTestId('worker-cpc-alice-x7k2q')).getByTestId('version')
    expect(v.getAttribute('title')).toContain('no Longarm Update service')
  })

  it('one machine card for the session worker + admin worker; old workers collapsed, no red errors', () => {
    render(<PoolTab pool={devboxPool()} />)
    expect(screen.queryByTestId('worker-cpc-alice-x7k2q-admin')).toBeNull()
    const card = screen.getByTestId('worker-cpc-alice-x7k2q')
    expect(within(card).getByTestId('admin-badge').textContent).toContain('admin commands: on')
    expect(within(card).getByTestId('admin-badge').getAttribute('title')).toMatch(/SYSTEM.*owner.*explicitly/)
    expect(within(card).getByTestId('cap-state').textContent).toBe('worker online · admin online')
    expect(within(card).getByTestId('label-chip').textContent).toBe('pool=devbox')
    // the session worker's counts, once
    expect(within(card).getByTestId('agent-claude').textContent).toContain('2')
    expect(within(card).getByTestId('agent-copilot').textContent).toContain('1')
    // online first; the long-offline ones in the collapsed section, compact
    const cards = screen.getAllByTestId(/^worker-/).map((e) => e.getAttribute('data-testid'))
    expect(cards).toEqual(['worker-cpc-alice-x7k2q', 'worker-fleet-1'])
    const off = screen.getByTestId('offline-section')
    expect(off.tagName).toBe('DETAILS')
    expect(off.hasAttribute('open')).toBe(false)
    expect(off.querySelector('summary')?.textContent).toContain('Offline (3)')
    expect(within(off).getByTestId('offline-old-pair').textContent).toContain('last seen 20h ago')
    expect(within(off).queryByText(/rpc error/)).toBeNull()
    fireEvent.click(within(within(off).getByTestId('offline-old-pair')).getByText('details'))
    expect(within(off).getByText(/rpc error/)).toBeTruthy()
    expect(document.querySelector('.msg.red')).toBeNull()
  })

  it('attach / ssh go to the session worker; admin actions name the admin worker; remove confirms', async () => {
    const calls = mockFetch({
      '/trayapi/attach': () => [200, { ok: true }],
      '/trayapi/desktop': () => [200, { ok: true }],
      '/local/longarm/v1/workers/': () => [200, { name: 'x', state: 'revoked' }],
    })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    render(
      <Toasts>
        <PoolTab pool={devboxPool()} />
      </Toasts>,
    )
    const card = screen.getByTestId('worker-cpc-alice-x7k2q')
    fireEvent.click(within(card).getByText('attach'))
    fireEvent.click(within(card).getByLabelText('more actions'))
    fireEvent.click(within(card).getByText('run as admin (attach cpc-alice-x7k2q-admin)'))
    await waitFor(() => expect(calls.filter((c) => c.url === '/trayapi/attach')).toHaveLength(2))
    const bodies = calls.filter((c) => c.url === '/trayapi/attach').map((c) => JSON.parse(String(c.init?.body)).worker)
    expect(bodies).toEqual(['cpc-alice-x7k2q', 'cpc-alice-x7k2q-admin'])
    // desktop: the session worker (longarm desktop picks Windows App for a Dev Box)
    fireEvent.click(within(card).getByText('desktop'))
    await waitFor(() => expect(calls.filter((c) => c.url === '/trayapi/desktop').map((c) => JSON.parse(String(c.init?.body)).worker)).toEqual(['cpc-alice-x7k2q']))
    // not an admin: no drain; the fleet worker isn't hers to remove
    expect(within(card).queryByText('drain')).toBeNull()
    expect(within(screen.getByTestId('worker-fleet-1')).queryByLabelText('more actions')).toBeNull()
    // remove an old registration of hers from the collapsed section
    fireEvent.click(within(screen.getByTestId('offline-old-pc')).getByText('remove'))
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Remove old-pc from the hub?'))
    await waitFor(() => expect(calls.some((c) => c.url === '/local/longarm/v1/workers/old-pc/remove' && c.init?.method === 'POST')).toBe(true))
  })

  it('a declined confirmation removes nothing; admins get drain and remove for fleet workers', () => {
    const calls = mockFetch({})
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    render(<PoolTab pool={devboxPool('admin/root-laptop')} />)
    expect(within(screen.getByTestId('worker-cpc-alice-x7k2q')).getByText('drain')).toBeTruthy()
    const fleet = screen.getByTestId('worker-fleet-1')
    fireEvent.click(within(fleet).getByLabelText('more actions'))
    fireEvent.click(within(fleet).getByText('remove fleet-1…'))
    expect(calls).toHaveLength(0)
  })

  it('an admin-only machine: one card, admin capability only, no default attach', () => {
    render(<PoolTab pool={{ ...devboxPool(), workers: [w('box-admin', {}, { admin_worker: true })] }} />)
    const card = screen.getByTestId('worker-box-admin')
    expect(within(card).getByTestId('cap-state').textContent).toBe('admin only · admin online')
    expect(within(card).queryByText('attach')).toBeNull()
  })

  it('empty: points at the Account tab before sign-in', () => {
    const nav = vi.fn()
    render(
      <PoolTab
        pool={{ ...devboxPool(), workers: [] }}
        onNavigate={nav}
        info={{ version: 'x', roles: { client: true, worker: false }, client: { running: true }, worker: { running: false, enrolled: false }, pinned: false, account: { signed_in: false, needed: true, targets: { client: true, worker: false, admin_worker: false } } }}
      />,
    )
    expect(screen.getByTestId('empty-pool').textContent).toContain('Sign in on the Account tab')
    fireEvent.click(screen.getByText('Account tab'))
    expect(nav).toHaveBeenCalledWith('account')
  })
})

describe('ClientJobs', () => {
  it('confirms a kill with the job, its owner and worker', async () => {
    const job = { worker: 'cpc-alice-x7k2q', job: { spec: { id: 'j-123456789', name: 'build', owner: 'alice' }, state: 'JOB_STATE_RUNNING' } }
    const calls = mockFetch({ '/local/longarm/v1/jobs?': () => [200, { jobs: [job] }], '/local/longarm/v1/jobs/': () => [200, {}] })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    render(<ClientJobs />)
    expect(screen.getByText('loading jobs…')).toBeTruthy()
    fireEvent.click(await screen.findByText('kill'))
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('Kill job build (alice) on cpc-alice-x7k2q?'))
    expect(calls.some((c) => c.url.includes('/kill'))).toBe(false)
  })
})
