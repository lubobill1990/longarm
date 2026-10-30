import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Toasts } from '../components/common'
import { UpdateBanner } from './UpdateBanner'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

function mockTray(update: () => unknown, install?: () => [number, unknown]) {
  const calls: { url: string; init?: RequestInit }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      const r: [number, unknown] = url.startsWith('/trayapi/update/install') ? (install?.() ?? [200, { state: 'downloading' }]) : [200, update()]
      const [status, body] = r
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }),
  )
  return calls
}

const show = () =>
  render(
    <Toasts>
      <UpdateBanner />
    </Toasts>,
  )

describe('UpdateBanner', () => {
  it('offers "Update to X" and asks the tray to install exactly that version', async () => {
    const calls = mockTray(() => ({ current: '0.1.2', latest: '0.1.3', available: true, msi: 'longarm-windows-amd64.msi' }))
    show()
    const btn = await screen.findByText('Update to 0.1.3')
    expect(screen.getByTestId('update-banner').textContent).toContain('you have 0.1.2')
    fireEvent.click(btn)
    await waitFor(() => expect(calls.some((c) => c.url === '/trayapi/update/install')).toBe(true))
    const post = calls.find((c) => c.url === '/trayapi/update/install')!
    expect(post.init?.method).toBe('POST')
    expect(JSON.parse(String(post.init?.body))).toEqual({ version: '0.1.3' })
  })

  it('shows nothing when up to date or without a hub', async () => {
    const calls = mockTray(() => ({ current: '0.1.3', latest: '0.1.3', available: false }))
    show()
    await waitFor(() => expect(calls.length).toBeGreaterThan(0))
    expect(screen.queryByTestId('update-banner')).toBeNull()
  })

  it('reports a refused download (verification failed) and keeps the button', async () => {
    mockTray(() => ({
      current: '0.1.2',
      latest: '0.1.3',
      available: true,
      state: 'failed',
      error: "refusing longarm-windows-amd64.msi: its sha256 00ff doesn't match the hub's release record",
    }))
    show()
    const err = await screen.findByTestId('update-error')
    expect(err.textContent).toContain("doesn't match the hub's release record")
    expect(screen.getByText('Update to 0.1.3')).toBeTruthy()
  })
})
