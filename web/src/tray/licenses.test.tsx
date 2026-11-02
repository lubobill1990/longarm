import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Licenses } from './ClientTab'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('Settings > Licenses', () => {
  it('loads the license texts when first opened, once', async () => {
    const fetchMock = vi.fn(async () => new Response('Longarm itself is licensed under the MIT License', { status: 200, headers: { 'Content-Type': 'text/plain' } }))
    vi.stubGlobal('fetch', fetchMock)
    render(<Licenses />)
    expect(fetchMock).not.toHaveBeenCalled()
    const details = screen.getByText('Show the licenses').closest('details')!
    details.open = true
    fireEvent(details, new Event('toggle'))
    expect(await screen.findByText(/licensed under the MIT License/)).toBeTruthy()
    expect(String((fetchMock.mock.calls[0] as unknown[])[0])).toBe('/trayapi/licenses')
    details.open = false
    fireEvent(details, new Event('toggle'))
    details.open = true
    fireEvent(details, new Event('toggle'))
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
