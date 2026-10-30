import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Toasts } from '../components/common'
import { AutoUpdate } from './ClientTab'

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

const status = (over: Record<string, unknown> = {}) => ({
  service: true,
  installed: '0.1.20',
  available: '0.1.20',
  auto: false,
  auto_interval_hours: 6,
  can_set_auto: true,
  ...over,
})

describe('release channel', () => {
  it('shows the machine channel (stable by default) and changes it through the update service', async () => {
    const calls = mockFetch({
      '/trayapi/update/service': () => [200, status()],
      '/trayapi/update/channel': (init) => [200, status({ channel: JSON.parse(String(init?.body)).channel, available: '0.1.22-dev.7' })],
    })
    render(
      <Toasts>
        <AutoUpdate />
      </Toasts>,
    )
    const section = await screen.findByTestId('auto-update')
    const group = within(section).getByRole('radiogroup', { name: 'Release channel' })
    expect((within(group).getByLabelText('Stable') as HTMLInputElement).checked).toBe(true)
    fireEvent.click(within(group).getByLabelText('Dev'))
    await waitFor(() => expect((within(group).getByLabelText('Dev') as HTMLInputElement).checked).toBe(true))
    const posts = calls.filter((c) => c.url === '/trayapi/update/channel')
    expect(posts.map((c) => JSON.parse(String(c.init?.body)))).toEqual([{ channel: 'dev' }])
    expect(section.textContent).toContain('0.1.22-dev.7 available')
    expect(screen.getByTestId('channel-help').textContent).toContain('every change merged to main')
  })

  it("only the machine's owner or an administrator may change it", async () => {
    mockFetch({ '/trayapi/update/service': () => [200, status({ channel: 'preview', can_set_auto: false })] })
    render(
      <Toasts>
        <AutoUpdate />
      </Toasts>,
    )
    const group = await screen.findByRole('radiogroup', { name: 'Release channel' })
    const preview = within(group).getByLabelText('Preview') as HTMLInputElement
    expect(preview.checked).toBe(true)
    expect(preview.disabled).toBe(true)
    expect(screen.getByTestId('channel-help').textContent).toContain("only this machine's owner or an administrator")
  })
})
