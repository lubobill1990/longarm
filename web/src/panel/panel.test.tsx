import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Toasts } from '../components/common'
import { LoginPage } from './LoginPage'
import { PanelApp } from './PanelApp'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
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

class FakeEventSource {
  addEventListener() {}
  close() {}
}

describe('LoginPage', () => {
  it('posts user, password and TOTP as JSON and shows errors', async () => {
    let ok = false
    const calls = mockFetch({ '/api/login': () => (ok ? [200, { user: 'ops' }] : [401, { error: 'wrong user, password or code' }]) })
    const onLogin = vi.fn()
    render(
      <Toasts>
        <LoginPage onLogin={onLogin} />
      </Toasts>,
    )
    fireEvent.change(screen.getByLabelText('user'), { target: { value: 'ops' } })
    fireEvent.change(screen.getByLabelText('password'), { target: { value: 'secret secret' } })
    fireEvent.change(screen.getByLabelText('TOTP code'), { target: { value: '123 456' } })
    fireEvent.click(screen.getByText('Log in'))
    await screen.findByText('wrong user, password or code')
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ user: 'ops', password: 'secret secret', totp: '123456' })
    expect((calls[0].init?.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    ok = true
    fireEvent.change(screen.getByLabelText('TOTP code'), { target: { value: '654321' } })
    fireEvent.click(screen.getByText('Log in'))
    await waitFor(() => expect(onLogin).toHaveBeenCalled())
  })
})

describe('PanelApp', () => {
  it('shows the login page when /api/me is 401', async () => {
    mockFetch({ '/api/me': () => [401, { error: 'login required' }] })
    render(<PanelApp />)
    expect(await screen.findByText('Log in')).toBeTruthy()
  })

  it('lists pending nodes with an approve button and workers', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/nodes')
    const calls = mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: 'dev' }],
      '/api/nodes/new1/approve': () => [200, { name: 'new1', state: 'approved' }],
      '/api/nodes': () => [
        200,
        {
          nodes: [
            { name: 'new1', role: 'worker', state: 'pending', os: 'linux' },
            { name: 'wa', role: 'worker', state: 'approved', online: true, metrics: { cpu_percent: 12, mem_total_bytes: '100', mem_used_bytes: '50' }, labels: { pool: 'x', os: 'windows' } },
          ],
        },
      ],
      '/api/history': () => [200, { nodes: [{ node: 'wa', points: [{ cpu: 1, mem: 2 }] }] }],
      '/api/links': () => [200, { reports: [{ client: 'alice', links: [{ worker: 'wa', state: 'online', active: 'tcp', rtt_ms: 2.5, legs: [{ label: 'tcp', active: true, up: true, kind: 'direct' }] }] }] }],
      '/api/profiles': () => [200, { profiles: [] }],
    })
    render(<PanelApp />)
    expect(await screen.findByTestId('pending-new1')).toBeTruthy()
    expect(await screen.findByTestId('node-wa')).toBeTruthy()
    expect(screen.getByTestId('node-wa').textContent).toContain('pool=x')
    expect((await screen.findByText(/alice:/)).parentElement?.textContent).toContain('2.5 ms')
    fireEvent.click(screen.getByText('Approve'))
    await waitFor(() => expect(calls.some((c) => c.url === '/api/nodes/new1/approve' && c.init?.method === 'POST')).toBe(true))
  })
})

describe('multi-tenancy', () => {
  const userMe = {
    user: 'alice',
    hub: 'hub1',
    version: 'dev',
    role: 'user',
    tenant: 'alice',
    teams: ['dev'],
    tenancy: true,
    quota: { maxJobs: 4, maxMemGb: 32 }, // plain protojson: lowerCamelCase
    usage: { jobs: 1, memGb: 8 },
  }
  const running = (id: string, owner: string, share?: string[]) => ({
    worker: 'wa',
    worker_online: true,
    job: { spec: { id, argv: ['make', 'test'], owner, submitted_by: `client/${owner}`, share_teams: share }, state: 'JOB_STATE_RUNNING', created_at: '2026-01-01T00:00:00Z' },
  })

  it('shows user-role logins only My jobs, Nodes and Me (with quota and usage)', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/')
    const calls = mockFetch({
      '/api/me': () => [200, userMe],
      '/api/jobs': () => [200, { jobs: [] }],
      '/api/nodes': () => [200, { nodes: [{ name: 'wa', role: 'worker', state: 'approved', online: true, isolation: 'per-user' }] }],
      '/api/history': () => [200, { nodes: [] }],
    })
    render(<PanelApp />)
    expect(await screen.findByRole('heading', { name: 'My jobs' })).toBeTruthy()
    const nav = screen.getByRole('navigation')
    expect(Array.from(nav.querySelectorAll('a')).map((a) => a.textContent)).toEqual(['My jobs', 'Nodes', 'Me'])
    fireEvent.click(screen.getByText('Nodes'))
    expect(await screen.findByTestId('node-wa')).toBeTruthy()
    expect(screen.queryByText('Drain')).toBeNull()
    expect(screen.queryByText('Revoke')).toBeNull()
    expect(screen.queryByText('edit')).toBeNull()
    fireEvent.click(screen.getByText('Me'))
    const me = await screen.findByTestId('me')
    expect(me.textContent).toContain('1/4')
    expect(me.textContent).toContain('8/32 GB')
    expect(screen.getByTestId('me-teams').textContent).toBe('dev')
    // no admin-only endpoints were called
    expect(calls.some((c) => /\/api\/(links|profiles|tenants|users|joincodes)/.test(c.url))).toBe(false)
  })

  it('hides Kill on jobs of other users for user-role logins', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/jobs')
    mockFetch({
      '/api/me': () => [200, userMe],
      '/api/jobs': () => [200, { jobs: [running('j-own', 'alice'), running('j-bob', 'bob', ['dev'])] }],
      '/api/nodes': () => [200, { nodes: [] }],
    })
    render(<PanelApp />)
    const own = await screen.findByTestId('job-j-own')
    const shared = screen.getByTestId('job-j-bob')
    expect(own.textContent).toContain('Kill')
    expect(shared.textContent).not.toContain('Kill')
    expect(shared.textContent).toContain('bob')
    expect(screen.getByTestId('shared-j-bob').textContent).toContain('dev')
  })

  it('lets administrators kill any job and shows the owner', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/jobs')
    mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: 'dev', role: 'admin', tenant: 'ops' }],
      '/api/jobs': () => [200, { jobs: [running('j-bob', 'bob')] }],
      '/api/nodes': () => [200, { nodes: [] }],
    })
    render(<PanelApp />)
    const row = await screen.findByTestId('job-j-bob')
    expect(row.textContent).toContain('Kill')
    expect(row.textContent).toContain('bob')
    expect(screen.getByRole('heading', { name: 'Jobs' })).toBeTruthy()
  })

  it('TenantsPage lists users and teams and posts a PutUserRequest on add', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/tenants')
    const calls = mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: 'dev', role: 'admin', hub_address: 'hub.example.com:7443', ca_fingerprint: 'sha256:ab' }],
      '/api/tenants': (init) =>
        init?.method === 'POST'
          ? [
              200,
              {
                user: { name: 'carol', teams: ['dev', 'ops'], panel: true },
                join_code: { code: 'K7P-Q2M', role: 'client', name: 'carol', user: 'carol', expires_at: '2026-01-02T00:00:00Z' },
                panel_password: 'pw-placeholder',
                totp_secret: 'TOTPSECRET',
                otpauth: 'otpauth://totp/longarm:carol?secret=TOTPSECRET',
              },
            ]
          : [
              200,
              {
                users: [
                  { name: 'alice', teams: ['dev'], devices: ['alice', 'alice-2'], usage: { jobs: 2, mem_gb: 12 }, quota: { max_jobs: 5 }, panel: true },
                  { name: 'bob', disabled: true },
                ],
                default_quota: { max_jobs: 8, max_mem_gb: 64 },
                tenancy: true,
              },
            ],
      '/api/teams': () => [200, { teams: [{ name: 'dev', members: ['alice'] }] }],
      '/api/install': () => [200, { windows: 'irm https://hub.example.com/install.ps1', linux: 'curl -fsSL https://hub.example.com/install.sh', longarmd: '' }],
    })
    render(<PanelApp />)
    const alice = await screen.findByTestId('tenant-alice')
    expect(alice.textContent).toContain('2/5') // own quota override
    expect(alice.textContent).toContain('12/64 GB') // default quota
    expect(alice.textContent).toContain('alice-2')
    expect(screen.getByTestId('tenant-bob').textContent).toContain('disabled')
    expect(screen.getByTestId('tenancy').textContent).toContain('on')
    expect(screen.getByTestId('default-quota').textContent).toContain('64 GB')
    expect((await screen.findByTestId('team-dev')).textContent).toContain('alice')

    fireEvent.click(screen.getByRole('button', { name: 'Add user' }))
    fireEvent.change(screen.getByLabelText('user name'), { target: { value: 'carol' } })
    fireEvent.change(screen.getByLabelText('teams'), { target: { value: 'dev, ops' } })
    fireEvent.change(screen.getByLabelText('quota active jobs (fleet)'), { target: { value: '3' } })
    fireEvent.click(screen.getByLabelText('web panel login'))
    fireEvent.click(screen.getByText('Create user'))
    expect((await screen.findByTestId('join-code')).textContent).toBe('K7P-Q2M')
    const post = calls.find((c) => c.url === '/api/tenants' && c.init?.method === 'POST')
    expect((post?.init?.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(JSON.parse(String(post?.init?.body))).toEqual({
      name: 'carol',
      create: true,
      set_teams: true,
      teams: ['dev', 'ops'],
      join_code: true,
      panel: true,
      set_quota: true,
      quota: { max_jobs: 3 },
      ttl: '86400s',
      admin: false,
    })
    expect(calls.some((c) => c.url === '/api/install?code=K7P-Q2M&role=client')).toBe(true)
    expect((await screen.findByTestId('install-windows')).textContent).toContain('install.ps1')
    expect(screen.getByTestId('panel-password').textContent).toBe('pw-placeholder')
    expect(screen.getByTestId('panel-totp').textContent).toBe('TOTPSECRET')
  })

  it('TenantsPage removes a user with purge and changes team members', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/tenants')
    const calls = mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: 'dev', role: 'admin' }],
      '/api/tenants/bob': () => [200, { revoked: ['bob'], purge: true }],
      '/api/tenants': () => [200, { users: [{ name: 'alice', teams: ['dev'] }, { name: 'bob' }], tenancy: true }],
      '/api/teams/dev': () => [200, { teams: [{ name: 'dev', members: ['alice', 'bob'] }] }],
      '/api/teams': () => [200, { teams: [{ name: 'dev', members: ['alice'] }] }],
    })
    render(<PanelApp />)
    const bob = await screen.findByTestId('tenant-bob')
    fireEvent.click(bob.querySelector('button.danger')!)
    fireEvent.click(screen.getByLabelText('purge data and OS account on workers'))
    fireEvent.click(screen.getByText('Remove bob'))
    await waitFor(() => expect(calls.some((c) => c.url === '/api/tenants/bob?purge=1' && c.init?.method === 'DELETE')).toBe(true))

    await screen.findByTestId('team-dev')
    fireEvent.change(screen.getByLabelText('add member to dev'), { target: { value: 'bob' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    await waitFor(() => {
      const c = calls.find((x) => x.url === '/api/teams/dev')
      expect(c && JSON.parse(String(c.init?.body))).toEqual({ add: ['bob'], remove: [] })
    })
  })
})
