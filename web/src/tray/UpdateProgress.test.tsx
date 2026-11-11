import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { detailOf, stepOf, UpdateProgressApp, UpdateProgressView } from './UpdateProgress'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('update progress window', () => {
  it('maps the phases to steps and words', () => {
    expect(['waiting', 'checking', 'downloading'].map(stepOf)).toEqual([0, 0, 0])
    expect(stepOf('verifying')).toBe(1)
    expect(stepOf('restarting')).toBe(2)
    expect(stepOf('done')).toBe(3)
    expect(detailOf({ to: '0.1.35', phase: 'downloading', bytes_done: 12 << 20, bytes_total: 42 << 20 })).toBe('Downloading 12 of 42 MB')
    expect(detailOf({ to: '0.1.35', phase: 'done', reboot: true })).toBe('v0.1.35 is installed. Restart Windows to finish.')
    expect(detailOf({ to: '0.1.35', phase: 'failed', error: 'bad signature' })).toBe('bad signature')
  })

  it('shows the download, then how it ended', () => {
    const close = vi.fn()
    const { rerender } = render(
      <UpdateProgressView p={{ to: '0.1.35', from: '0.1.34', phase: 'downloading', bytes_done: 1 << 20, bytes_total: 4 << 20 }} onClose={close} />,
    )
    expect(screen.getByTestId('update-versions').textContent).toBe('v0.1.34 → v0.1.35')
    expect(screen.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('25')
    expect(screen.getByRole('button').textContent).toBe('Hide')
    rerender(<UpdateProgressView p={{ to: '0.1.35', from: '0.1.34', phase: 'failed', error: 'bad signature' }} onClose={close} />)
    expect(screen.getByText('The update did not finish')).toBeTruthy()
    expect(screen.getByRole('status').textContent).toBe('bad signature')
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(close).toHaveBeenCalled()
  })

  it('polls the window API', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ to: '0.1.35', phase: 'restarting' }), { status: 200, headers: { 'Content-Type': 'application/json' } })),
    )
    render(<UpdateProgressApp />)
    await waitFor(() => expect(screen.getByTestId('update-progress').dataset.phase).toBe('restarting'))
    expect(fetch).toHaveBeenCalledWith('/trayapi/progress')
  })
})
