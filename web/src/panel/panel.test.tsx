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
    fireEvent.change(await screen.findByLabelText('user'), { target: { value: 'ops' } })
    fireEvent.change(screen.getByLabelText('password'), { target: { value: 'secret secret' } })
    fireEvent.change(screen.getByLabelText('TOTP code'), { target: { value: '123 456' } })
    fireEvent.click(screen.getByText('Log in'))
    await screen.findByText('wrong user, password or code')
    const login = calls.find((c) => c.url === '/api/login')!
    expect(JSON.parse(String(login.init?.body))).toEqual({ user: 'ops', password: 'secret secret', totp: '123456' })
    expect((login.init?.headers as Record<string, string>)['Content-Type']).toBe('application/json')
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

  it('shows user-role logins only My jobs, My machines, Nodes, Port links, Get Longarm and Me (with quota and usage)', async () => {
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
    expect(Array.from(nav.querySelectorAll('a')).map((a) => a.textContent)).toEqual(['My jobs', 'My machines', 'Nodes', 'Port links', 'Get Longarm', 'Me'])
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

describe('PortLinksPage', () => {
  const userMe = { user: 'alice', hub: 'hub1', version: 'dev', role: 'user', tenant: 'alice' }
  const inHours = (h: number) => new Date(Date.now() + h * 3600e3).toISOString()
  // plain protojson (lowerCamelCase, int64 as strings, zero fields omitted)
  const link = {
    id: 'pl-1',
    owner: 'alice',
    from: 'wa',
    to: 'wb',
    port: 5432,
    listenPort: 15432,
    expiresAt: inHours(3),
    jobId: 'job-abc',
    createdBy: 'client/alice-laptop',
    status: { id: 'pl-1', state: 'listening', connections: '7', active: 2, bytesSent: '2048', bytesReceived: '3145728', path: 'tcp/direct 1ms', refused: '1' },
  }

  it('lists the links with their state, counters and expiry', async () => {
    history.pushState(null, '', '/port-links')
    mockFetch({
      '/api/me': () => [200, userMe],
      '/api/port-links': () => [
        200,
        {
          links: [
            link,
            // proto names (writePB), not reported yet
            { id: 'pl-2', owner: 'alice', from: 'wb', to: 'wa', port: 8080, listen_port: 9090, expires_at: inHours(-1), status: { state: 'error', error: 'address in use' } },
          ],
        },
      ],
    })
    render(<PanelApp />)
    const row = await screen.findByTestId('port-link-pl-1')
    expect(screen.getByRole('heading', { name: 'Port links' })).toBeTruthy()
    expect(row.textContent).toContain('wa → 127.0.0.1:15432')
    expect(row.textContent).toContain('wb:5432')
    expect(row.textContent).toContain('listening')
    expect(row.textContent).toContain('tcp/direct 1ms')
    expect(row.textContent).toContain('7 / 2 / 1')
    expect(row.textContent).toContain('2K / 3M')
    expect(row.textContent).toMatch(/in (2|3)h/)
    expect(row.textContent).toContain('job-abc')
    const bad = screen.getByTestId('port-link-pl-2')
    expect(bad.textContent).toContain('wb → 127.0.0.1:9090')
    expect(bad.textContent).toContain('address in use')
    expect(bad.textContent).toContain('expired')
    expect(Array.from(screen.getByRole('navigation').querySelectorAll('a')).map((a) => a.textContent)).toContain('Port links')
  })

  it('explains port links and the CLI when there are none', async () => {
    history.pushState(null, '', '/port-links')
    mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: 'dev', role: 'admin' }],
      '/api/port-links': () => [200, {}],
    })
    render(<PanelApp />)
    const empty = await screen.findByTestId('port-links-empty')
    expect(empty.textContent).toContain('longarm link create <worker>:<port> --on <worker> [--port N] [--ttl 8h] [--job <id>]')
    expect(screen.getByText('Port links', { selector: 'nav a' })).toBeTruthy()
  })

  it('removes a link with DELETE /api/port-links/<id> and reloads', async () => {
    history.pushState(null, '', '/port-links')
    vi.stubGlobal('confirm', vi.fn(() => true))
    let removed = false
    const calls = mockFetch({
      '/api/me': () => [200, userMe],
      '/api/port-links/pl-1': (init) => {
        removed = init?.method === 'DELETE'
        return [200, link]
      },
      '/api/port-links': () => [200, { links: removed ? [] : [link] }],
    })
    render(<PanelApp />)
    const row = await screen.findByTestId('port-link-pl-1')
    fireEvent.click(row.querySelector('button.danger')!)
    await waitFor(() => expect(calls.some((c) => c.url === '/api/port-links/pl-1' && c.init?.method === 'DELETE')).toBe(true))
    expect(await screen.findByTestId('port-links-empty')).toBeTruthy()
    expect(screen.queryByTestId('port-link-pl-1')).toBeNull()
  })
})

describe('sign-in, machines, updates', () => {
  const userMe = { user: 'alice', hub: 'hub1', version: '0.1.3', role: 'user', tenant: 'alice', base_url: 'https://hub.example.com' }
  const latest = { version: '0.1.3', files: [{ name: 'longarm-windows-amd64.msi', sha256: 'ab', size: 10, signed: true }] }

  it('LoginPage: "Sign in with Microsoft" first when the hub has Entra, the panel account folded', async () => {
    mockFetch({ '/auth/info': () => [200, { entra: true, mode: 'spa' }] })
    history.pushState(null, '', '/jobs')
    render(
      <Toasts>
        <LoginPage onLogin={() => {}} />
      </Toasts>,
    )
    const ms = await screen.findByTestId('signin-microsoft')
    expect(ms.getAttribute('href')).toBe('/auth/login?next=%2Fjobs')
    expect(ms.className).toContain('primary')
    const details = screen.getByText('Panel account').closest('details')
    expect(details?.open).toBe(false)
    expect(details?.querySelector('input[aria-label="TOTP code"]')).toBeTruthy()
  })

  it('LoginPage: one button per provider (work or school, Microsoft account, Google, Apple)', async () => {
    mockFetch({
      '/auth/info': () => [
        200,
        {
          entra: false,
          providers: [
            { id: 'microsoft', label: 'Microsoft account' },
            { id: 'google', label: 'Google' },
            { id: 'apple', label: 'Apple' },
          ],
        },
      ],
    })
    history.pushState(null, '', '/jobs')
    render(
      <Toasts>
        <LoginPage onLogin={() => {}} />
      </Toasts>,
    )
    const g = await screen.findByTestId('signin-google')
    expect(g.getAttribute('href')).toBe('/auth/login?provider=google&next=%2Fjobs')
    expect(g.textContent).toBe('Sign in with Google')
    expect(screen.getByTestId('signin-microsoft-account').className).toBe('button primary')
    expect(screen.getByTestId('signin-apple').getAttribute('href')).toBe('/auth/login?provider=apple&next=%2Fjobs')
    expect(g.className).toBe('button')
  })

  it('LoginPage: the password form alone without Entra', async () => {
    mockFetch({ '/auth/info': () => [200, { entra: false }] })
    render(
      <Toasts>
        <LoginPage onLogin={() => {}} />
      </Toasts>,
    )
    expect(await screen.findByLabelText('TOTP code')).toBeTruthy()
    expect(screen.queryByTestId('signin-microsoft')).toBeNull()
  })

  it('Nodes marks outdated versions with the reason a worker cannot update itself', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/nodes')
    mockFetch({
      '/api/me': () => [200, { user: 'ops', hub: 'hub1', version: '0.1.3', role: 'admin' }],
      '/releases/latest.json': () => [200, latest],
      '/api/nodes': () => [
        200,
        {
          nodes: [
            { name: 'old', role: 'worker', state: 'approved', online: true, version: '0.1.2', update_available: { version: '0.1.3', reason: 'MSI install' } },
            { name: 'new', role: 'worker', state: 'approved', online: true, version: '0.1.3' },
            { name: 'dev', role: 'worker', state: 'approved', online: true, version: 'dev' },
          ],
        },
      ],
      '/api/history': () => [200, { nodes: [] }],
      '/api/links': () => [200, { reports: [] }],
      '/api/profiles': () => [200, { profiles: [] }],
    })
    render(<PanelApp />)
    const tag = await screen.findByTestId('outdated-old')
    expect(tag.textContent).toContain('0.1.3')
    expect(tag.getAttribute('title')).toContain('MSI install')
    expect(screen.queryByTestId('outdated-new')).toBeNull()
    expect(screen.queryByTestId('outdated-dev')).toBeNull()
  })

  it('confirms a kill with the job name, worker and owner', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/jobs')
    const ask = vi.fn((_msg?: string) => false)
    vi.stubGlobal('confirm', ask)
    const calls = mockFetch({
      '/api/me': () => [200, userMe],
      '/api/jobs': () => [
        200,
        { jobs: [{ worker: 'wa', worker_online: true, job: { spec: { id: 'j-1', name: 'tests', argv: ['make'], owner: 'alice' }, state: 'JOB_STATE_RUNNING' } }] },
      ],
      '/api/nodes': () => [200, { nodes: [] }],
    })
    render(<PanelApp />)
    const row = await screen.findByTestId('job-j-1')
    fireEvent.click(row.querySelector('button.danger')!)
    expect(ask).toHaveBeenCalledTimes(1)
    const text = String(ask.mock.calls[0][0])
    expect(text).toContain('tests')
    expect(text).toContain('worker wa')
    expect(text).toContain('alice')
    expect(calls.some((c) => c.url.includes('/kill'))).toBe(false) // refused: nothing killed
  })

  it('empty My jobs links to Get Longarm and My machines', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/jobs')
    mockFetch({ '/api/me': () => [200, userMe], '/api/jobs': () => [200, { jobs: [] }], '/api/nodes': () => [200, { nodes: [] }] })
    render(<PanelApp />)
    const empty = await screen.findByTestId('jobs-empty')
    const hrefs = Array.from(empty.querySelectorAll('a')).map((a) => a.getAttribute('href'))
    expect(hrefs).toEqual(['/get', '/machines'])
  })

  it('My machines: worker and admin worker as one machine; share, labels and remove', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/machines')
    vi.stubGlobal(
      'prompt',
      vi.fn(() => 'bob team:dev'),
    )
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const calls = mockFetch({
      '/api/me': () => [200, userMe],
      '/releases/latest.json': () => [200, latest],
      '/api/nodes/devbox/share': () => [200, { name: 'devbox' }],
      '/api/nodes/devbox/remove': () => [200, { name: 'devbox' }],
      '/api/nodes': () => [
        200,
        {
          nodes: [
            { name: 'devbox', role: 'worker', state: 'approved', hostname: 'devbox', owner: 'alice', online: true, version: '0.1.2', shares: { users: ['carol'] } },
            { name: 'devbox-admin', role: 'worker', state: 'approved', hostname: 'devbox', owner: 'alice', admin_worker: true, version: '0.1.3' },
            { name: 'shared-w', role: 'worker', state: 'approved', hostname: 'other', owner: '', version: '0.1.3' },
            { name: 'bobs', role: 'worker', state: 'approved', hostname: 'bobs', owner: 'bob', version: '0.1.3' },
          ],
        },
      ],
    })
    render(<PanelApp />)
    const m = await screen.findByTestId('machine-devbox')
    expect(m.textContent).toContain('devbox-admin')
    expect(screen.queryByTestId('machine-other')).toBeNull() // not hers
    expect(screen.queryByTestId('machine-bobs')).toBeNull()
    expect(await screen.findByTestId('outdated-devbox')).toBeTruthy()
    expect(screen.getByTestId('shares-devbox').textContent).toContain('carol')
    expect(screen.getByTestId('shares-devbox-admin').textContent).toContain('only you')
    fireEvent.click(screen.getByText('share'))
    await waitFor(() => expect(calls.some((c) => c.url === '/api/nodes/devbox/share')).toBe(true))
    const share = calls.find((c) => c.url === '/api/nodes/devbox/share')!
    expect(JSON.parse(String(share.init?.body))).toEqual({ add_users: ['bob'], add_teams: ['dev'] })
    fireEvent.click(screen.getByTestId('worker-devbox').querySelector('button.danger')!)
    await waitFor(() => expect(calls.some((c) => c.url === '/api/nodes/devbox/remove' && c.init?.method === 'POST')).toBe(true))
  })

  it('Get Longarm: the MSI of the newest release and the one-liners for this hub', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    history.pushState(null, '', '/get')
    mockFetch({ '/api/me': () => [200, userMe], '/releases/latest.json': () => [200, latest] })
    render(<PanelApp />)
    const msi = await screen.findByTestId('get-msi')
    expect(msi.getAttribute('href')).toBe('/releases/0.1.3/longarm-windows-amd64.msi')
    expect(screen.getByTestId('get-ps1').textContent).toBe("$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; irm https://hub.example.com/install.ps1 | iex")
    expect(screen.getByTestId('get-sh').textContent).toBe('curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both')
    expect(screen.getByTestId('get-steps').querySelectorAll('li').length).toBe(3)
  })
})
