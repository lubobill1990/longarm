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
