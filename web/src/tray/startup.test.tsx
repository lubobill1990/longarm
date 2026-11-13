import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Toasts } from '../components/common'
import { Settings } from './ClientTab'

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

const settings = (autostart?: object) => ({ version: '1', telemetry_disabled: false, ...(autostart ? { autostart } : {}) })

describe('Settings > Startup', () => {
  it('shows the OS state and changes it through /trayapi/settings', async () => {
    let on = true
    const calls = mockFetch({
      '/trayapi/settings': (init) => {
        if (init?.method === 'POST') on = JSON.parse(String(init.body)).autostart
        return [200, settings({ supported: true, enabled: on })]
      },
      '/local/longarm/v1/': () => [200, {}],
    })
    render(
      <Toasts>
        <Settings client={false} />
      </Toasts>,
    )
    const sw = (await screen.findByLabelText('start Longarm at login')) as HTMLInputElement
    expect(sw.checked).toBe(true)
    fireEvent.click(sw)
    await waitFor(() =>
      expect(calls.some((c) => c.url === '/trayapi/settings' && c.init?.method === 'POST' && String(c.init.body) === '{"autostart":false}')).toBe(true),
    )
    await waitFor(() => expect((screen.getByLabelText('start Longarm at login') as HTMLInputElement).checked).toBe(false))
  })

  it("the installer's entry for every user: off needs an administrator; a declined prompt leaves it on", async () => {
    mockFetch({
      '/trayapi/settings': (init) =>
        init?.method === 'POST'
          ? [409, { error: 'start at login: canceled at the administrator prompt' }]
          : [200, settings({ supported: true, enabled: true, machine_wide: true })],
      '/local/longarm/v1/': () => [200, {}],
    })
    render(
      <Toasts>
        <Settings client={false} />
      </Toasts>,
    )
    expect((await screen.findByTestId('startup')).textContent).toContain('asks for an administrator')
    fireEvent.click(screen.getByLabelText('start Longarm at login'))
    expect(await screen.findByText(/canceled at the administrator prompt/)).toBeTruthy()
    await waitFor(() => expect((screen.getByLabelText('start Longarm at login') as HTMLInputElement).checked).toBe(true))
  })

  it('no switch where the OS has none (or an older tray)', async () => {
    mockFetch({ '/trayapi/settings': () => [200, settings({ supported: false, enabled: false })], '/local/longarm/v1/': () => [200, {}] })
    render(
      <Toasts>
        <Settings client={false} />
      </Toasts>,
    )
    await screen.findByTestId('this-client')
    await waitFor(() => expect(screen.queryByTestId('startup')).toBeNull())
  })
})
