import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Account, TrayInfo } from '../api/local'
import { Toasts } from '../components/common'
import { AccountTab, rolesChangeText } from './AccountTab'
import { externalHref, installAppBehaviour, menuItems } from './appShell'
import { VersionChip, versionRows } from './VersionInfo'

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

function info(account: Partial<Account>): TrayInfo {
  return {
    version: '0.1.4',
    roles: { client: true, worker: false },
    client: { running: true },
    worker: { running: false, enrolled: false },
    pinned: true,
    account: {
      signed_in: true,
      user: 'alice@contoso.com',
      hub: 'longarm.example.com',
      needed: false,
      targets: { client: true, worker: false, admin_worker: false },
      roles: { client: true, worker: false, admin_worker: false },
      roles_editable: true,
      ...account,
    },
  }
}

describe('AccountTab roles', () => {
  it('adds "run jobs" with one apply; the admin worker needs it', async () => {
    const calls = mockFetch({
      '/trayapi/signin/state': () => [200, { state: 'idle', targets: { client: true, worker: false, admin_worker: false } }],
      '/trayapi/roles/state': () => [200, { state: 'done', want: { client: true, worker: true, admin_worker: false }, signin: true }],
      '/trayapi/roles': () => [200, { state: 'applying', want: { client: true, worker: true, admin_worker: false } }],
    })
    render(
      <Toasts>
        <AccountTab info={info({ cloud_pc: 'the computer name CPC-alice-1 is a Cloud PC' })} />
      </Toasts>,
    )
    expect(screen.getByTestId('cloud-pc-hint').textContent).toContain('Cloud PC')
    const admin = screen.getByLabelText(/Admin commands/) as HTMLInputElement
    expect(admin.disabled).toBe(true)
    fireEvent.click(screen.getByTestId('role-worker'))
    expect(admin.disabled).toBe(false)
    expect(screen.getByText(/add your worker/)).toBeTruthy()
    fireEvent.click(screen.getByTestId('roles-apply'))
    await waitFor(() => expect(calls.some((c) => c.url === '/trayapi/roles' && c.init?.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.url === '/trayapi/roles')!
    expect(JSON.parse(String(post.init?.body))).toEqual({ client: true, worker: true, admin_worker: false })
    // unchecking the worker unchecks the admin worker
    fireEvent.click(screen.getByTestId('role-worker'))
  })

  it('shows an expired sign-in with Sign in again', () => {
    mockFetch({ '/trayapi/signin/state': () => [200, { state: 'idle', targets: { client: true, worker: false, admin_worker: false } }] })
    render(
      <Toasts>
        <AccountTab info={info({ signed_in: false, needed: true, expired: true, expires: '2026-10-01T10:00:00Z' })} />
      </Toasts>,
    )
    expect(screen.getByTestId('signin-expired').textContent).toContain('Your sign-in expired on')
    expect(screen.getByTestId('signin-start')).toBeTruthy()
  })

  it('shows the running code large with Copy, and the summary when done', async () => {
    let state: unknown = {
      state: 'running',
      hub: 'longarm.example.com',
      code: { url: 'https://longarm.example.com/link', code: 'BCDF-GHJK', expires: '2026-10-28T12:10:00Z', kind: 'both', host: 'box' },
      targets: { client: true, worker: true, admin_worker: false },
    }
    mockFetch({ '/trayapi/signin/state': () => [200, state] })
    const write = vi.fn(async () => {})
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
    render(
      <Toasts>
        <AccountTab info={info({ needed: true })} />
      </Toasts>,
    )
    await waitFor(() => expect(screen.getByTestId('signin-code').textContent).toBe('BCDF-GHJK'))
    fireEvent.click(screen.getByTestId('signin-copy'))
    await waitFor(() => expect(write).toHaveBeenCalledWith('BCDF-GHJK'))
    state = {
      state: 'done',
      targets: { client: true, worker: true, admin_worker: false },
      result: { hub: 'h', user: 'alice', summary: 'Signed in as alice: client device alice-box, worker box (user session)' },
    }
    await waitFor(() => expect(screen.getByTestId('signin-done').textContent).toBe('Signed in as alice: client device alice-box, worker box (user session)'), {
      timeout: 3000,
    })
  })
})

describe('AccountTab signed in already', () => {
  it('asks before replacing what the machine is signed in as, then starts again with replace', async () => {
    const confirm = {
      state: 'confirm',
      hub: 'longarm.example.com:7443',
      targets: { client: true, worker: true, admin_worker: false },
      confirm: {
        hub: 'longarm.example.com:7443',
        current: [
          { kind: 'device', identity: 'admin/alice-box', user: 'alice', same_hub: true },
          { kind: 'worker', identity: 'worker/box', user: 'alice', same_hub: true },
        ],
        admin_worker_stays: true,
        request: { hub: '', labels: '' },
      },
    }
    const calls = mockFetch({
      '/trayapi/signin/state': () => [200, confirm],
      '/trayapi/signin/start': () => [200, { state: 'running', hub: 'longarm.example.com', targets: confirm.targets }],
    })
    render(
      <Toasts>
        <AccountTab info={info({ needed: true })} />
      </Toasts>,
    )
    await waitFor(() => expect(screen.getByTestId('signin-confirm')).toBeTruthy())
    const listed = screen.getByTestId('signin-confirm-current').textContent ?? ''
    expect(listed).toContain('device admin/alice-box (user alice)')
    expect(listed).toContain('worker worker/box (owner alice)')
    expect(screen.getByTestId('signin-confirm-admin').textContent).toContain('admin worker stays')
    expect(screen.queryByTestId('signin-start')).toBeNull() // never a plain "Sign in" that would replace silently
    fireEvent.click(screen.getByTestId('signin-replace'))
    await waitFor(() => expect(calls.some((c) => c.url === '/trayapi/signin/start')).toBe(true))
    const post = calls.find((c) => c.url === '/trayapi/signin/start')!
    expect(JSON.parse(String(post.init?.body))).toEqual({ hub: '', labels: '', replace: true })
  })

  it('offers signing in with an account when signed in', async () => {
    const calls = mockFetch({
      '/trayapi/signin/state': () => [200, { state: 'idle', targets: { client: true, worker: false, admin_worker: false } }],
      '/trayapi/signin/start': () => [200, { state: 'running', targets: { client: true, worker: false, admin_worker: false } }],
    })
    render(
      <Toasts>
        <AccountTab info={info({})} />
      </Toasts>,
    )
    fireEvent.click(screen.getByTestId('signin-again'))
    await waitFor(() => expect(calls.some((c) => c.url === '/trayapi/signin/start')).toBe(true))
    expect(JSON.parse(String(calls.find((c) => c.url === '/trayapi/signin/start')!.init?.body)).replace).toBe(false)
  })
})

describe('version chip', () => {
  it('marks what is behind: the tray behind the newest release, a daemon or worker behind the tray', () => {
    const rows = versionRows({
      tray: '0.1.21',
      latest: '0.1.22',
      daemon: '0.1.20',
      worker: '0.1.21',
      admin: '0.1.22',
      hub: '0.1.19',
      client: true,
      workerRole: true,
      adminWorker: true,
    })
    const by = Object.fromEntries(rows.map((r) => [r.key, r]))
    expect(by.tray.behind && by.tray.note).toContain('0.1.22 is available')
    expect(by.daemon.behind && by.daemon.note).toContain('older than this tray')
    expect(by.worker.behind).toBeFalsy()
    expect(by.admin.behind && by.admin.note).toContain('newer than this tray')
    expect(by.hub.behind).toBe(true)
    // development builds compare with nothing; parts this machine lacks have no row
    const dev = versionRows({ tray: 'dev', latest: '0.1.22', daemon: 'dev', client: true, workerRole: false, adminWorker: false })
    expect(dev.some((r) => r.behind)).toBe(false)
    expect(dev.map((r) => r.key)).toEqual(['tray', 'daemon', 'hub'])
  })

  it('shows the tray version in the title bar; the details mark the old daemon and offer the update', async () => {
    const calls = mockFetch({
      '/trayapi/update/install': () => [200, { state: 'downloading' }],
      '/trayapi/update': () => [200, { current: '0.1.21', latest: '0.1.22', available: true, hub: '0.1.22' }],
      '/local/longarm/v1/status': () => [200, { version: '0.1.20' }],
      '/local/longarmd/v1/status': () => [200, { name: 'box', version: '0.1.21' }],
    })
    const i = info({ admin: { enabled: true, signed_in: true, pending: false } })
    i.version = '0.1.21'
    i.roles = { client: true, worker: true }
    const pool = { workers: [{ name: 'box-admin', node: { name: 'box-admin', admin_worker: true, machine: 'box', version: '0.1.21' } }] }
    render(
      <Toasts>
        <VersionChip info={i} pool={pool as never} />
      </Toasts>,
    )
    const chip = screen.getByTestId('version-chip')
    expect(chip.textContent).toContain('v0.1.21')
    await waitFor(() => expect(chip.className).toContain('warn'))
    expect(chip.getAttribute('title')).toContain('longarm daemon 0.1.20 (behind)')
    fireEvent.click(chip)
    expect(screen.getByTestId('version-details')).toBeTruthy()
    expect(screen.getByTestId('version-daemon').className).toContain('behind')
    expect(screen.getByTestId('version-worker').className).not.toContain('behind')
    expect(screen.getByTestId('version-admin').textContent).toContain('0.1.21')
    expect(screen.getByTestId('version-hub').textContent).toContain('0.1.22')
    expect(screen.getByTestId('version-tray').className).toContain('behind')
    fireEvent.click(screen.getByTestId('version-update'))
    await waitFor(() => expect(calls.some((c) => c.url === '/trayapi/update/install')).toBe(true))
    expect(JSON.parse(String(calls.find((c) => c.url === '/trayapi/update/install')!.init?.body))).toEqual({ version: '0.1.22' })
  })
})

describe('rolesChangeText', () => {
  it('names each change', () => {
    const none = { client: false, worker: false, admin_worker: false }
    expect(rolesChangeText({ ...none, client: true }, { client: true, worker: true, admin_worker: true })).toEqual([
      'add your worker (runs jobs as you, starts at your logon)',
      'add the admin worker (administrator commands as SYSTEM, only you)',
    ])
    expect(rolesChangeText({ client: true, worker: true, admin_worker: false }, { ...none, worker: true })[0]).toContain('remove the client')
  })
})

describe('app behaviour', () => {
  it('opens external links in the system browser', () => {
    const a = document.createElement('a')
    a.href = 'https://longarm.example.com/link'
    expect(externalHref(a, 'http://127.0.0.1:7490')).toBe('https://longarm.example.com/link')
    a.href = 'http://127.0.0.1:7490/tray?tab=account'
    expect(externalHref(a, 'http://127.0.0.1:7490')).toBeUndefined()
    const open = vi.fn()
    const off = installAppBehaviour(open)
    window.open('https://example.com/x')
    expect(open).toHaveBeenCalledWith('https://example.com/x')
    off()
  })

  it('offers Copy for selected text and Paste in fields, nothing elsewhere', () => {
    const div = document.createElement('div')
    expect(menuItems(div, '')).toEqual([])
    expect(menuItems(div, 'BCDF-GHJK').map((i) => i.label)).toEqual(['Copy'])
    const input = document.createElement('input')
    input.value = 'pool=devbox'
    input.setSelectionRange(0, 4)
    expect(menuItems(input, '').map((i) => i.label)).toEqual(['Cut', 'Copy', 'Paste', 'Select all'])
  })
})
