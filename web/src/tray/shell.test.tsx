import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { TrayInfo } from '../api/local'
import { Toasts } from '../components/common'
import type { FleetJob } from '../api/types'
import { findJob, Settings } from './ClientTab'
import { applyTheme, parseTheme, resolveTheme } from './theme'
import { TrayApp } from './TrayApp'
import { Banner, EmptyState, Skeleton } from './ui'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  localStorage.clear()
  delete document.documentElement.dataset.theme
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

describe('theme', () => {
  it('system follows prefers-color-scheme; light and dark override it', () => {
    expect(resolveTheme('system', true)).toBe('dark')
    expect(resolveTheme('system', false)).toBe('light')
    expect(resolveTheme('light', true)).toBe('light')
    expect(resolveTheme('dark', false)).toBe('dark')
    expect(parseTheme('dark')).toBe('dark')
    expect(parseTheme('purple')).toBe('system')
    expect(parseTheme(undefined)).toBe('system')
    applyTheme('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('Settings > Appearance: a segmented System / Light / Dark choice', async () => {
    mockFetch({ '/trayapi/settings': () => [200, { version: '1', telemetry_disabled: false }], '/local/longarm/v1/': () => [200, {}] })
    const onTheme = vi.fn(async () => {})
    render(
      <Toasts>
        <Settings clientUp={false} theme="system" onTheme={onTheme} />
      </Toasts>,
    )
    const group = screen.getByRole('radiogroup', { name: 'Theme' })
    expect((screen.getByRole('radio', { name: 'System' }) as HTMLInputElement).checked).toBe(true)
    expect(group.querySelector('.is-active')?.textContent).toBe('System')
    fireEvent.click(screen.getByRole('radio', { name: 'Dark' }))
    await waitFor(() => expect(onTheme).toHaveBeenCalledWith('dark'))
  })
})

describe('ui', () => {
  it('banners, empty states and skeletons say what they show', () => {
    render(
      <>
        <Banner tone="error" testId="b" action={<button>Fix</button>}>
          broken
        </Banner>
        <EmptyState testId="e" title="No forwards">
          none yet
        </EmptyState>
        <Skeleton label="loading jobs…" />
      </>,
    )
    expect(screen.getByTestId('b').getAttribute('role')).toBe('alert')
    expect(screen.getByTestId('b').className).toContain('banner-error')
    expect(screen.getByText('Fix')).toBeTruthy()
    expect(screen.getByTestId('e').textContent).toBe('No forwardsnone yet')
    expect(screen.getByText('loading jobs…')).toBeTruthy()
  })
})

const info = (over: Partial<TrayInfo> = {}): TrayInfo => ({
  version: '1',
  roles: { client: true, worker: true },
  client: { running: false, error: 'the pipe is closed' },
  worker: { running: false, enrolled: true },
  pinned: true,
  ...over,
})

describe('TrayApp', () => {
  it('fixed head and tab bar; the arrows, Home and End move between tabs', async () => {
    mockFetch({ '/trayapi/info': () => [200, info()], '/trayapi/settings': () => [200, { version: '1', telemetry_disabled: false }] })
    render(<TrayApp />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'This worker' })).toBeTruthy())
    expect(screen.getByTestId('tray-head').textContent).toContain('Longarm')
    await waitFor(() => expect(screen.getByLabelText('pin').getAttribute('aria-pressed')).toBe('true'))
    const tabs = screen.getAllByRole('tab').map((t) => t.textContent)
    expect(tabs).toEqual(['Workers', 'My jobs', 'Forwards', 'This worker', 'Settings'])
    const workers = screen.getByRole('tab', { name: 'Workers' })
    expect(workers.getAttribute('aria-selected')).toBe('true')
    expect(workers.tabIndex).toBe(0)
    expect(screen.getByRole('tab', { name: 'My jobs' }).tabIndex).toBe(-1)
    // the daemon is down: a banner with Start it, not a red block of text
    expect(screen.getByTestId('daemon-down').textContent).toContain('longarm daemon is not running (the pipe is closed)')
    expect(screen.getByRole('button', { name: 'Start it' })).toBeTruthy()
    fireEvent.keyDown(workers, { key: 'ArrowRight' })
    expect(screen.getByRole('tab', { name: 'My jobs' }).getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(screen.getByRole('tab', { name: 'My jobs' }))
    fireEvent.keyDown(document.activeElement!, { key: 'End' })
    expect(screen.getByRole('tab', { name: 'Settings' }).getAttribute('aria-selected')).toBe('true')
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowRight' }) // wraps
    expect(screen.getByRole('tab', { name: 'Workers' }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('tab-pool')
  })

  it("applies the theme saved in the tray's settings", async () => {
    mockFetch({ '/trayapi/info': () => [200, info()], '/trayapi/settings': () => [200, { version: '1', telemetry_disabled: false, theme: 'dark' }] })
    render(<TrayApp />)
    await waitFor(() => expect(document.documentElement.dataset.theme).toBe('dark'))
    expect(localStorage.getItem('longarm.tray.theme')).toBe('dark')
  })
})

class FakeEventSource {
  addEventListener() {}
  close() {}
}

const fleetJob = (id: string, name = ''): FleetJob => ({
  worker: 'w1',
  job: { spec: { id, name, argv: ['pnpm', 'test'] }, state: 'JOB_STATE_RUNNING' } as FleetJob['job'],
})

describe('longarm ui open pages', () => {
  afterEach(() => history.replaceState(null, '', '/'))

  it('?tab=pool&add=1 opens Add a machine, once', async () => {
    mockFetch({ '/trayapi/info': () => [200, info({ client: { running: false } })], '/trayapi/addmachine': () => [200, { hub_host: '' }] })
    history.replaceState(null, '', '/tray?tab=pool&add=1')
    render(<TrayApp />)
    expect(await screen.findByTestId('add-machine')).toBeTruthy()
    // back on the tab later: the workers, not Add a machine again
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Settings' })).toBeTruthy())
    fireEvent.click(screen.getByRole('tab', { name: 'Settings' }))
    fireEvent.click(screen.getByRole('tab', { name: 'Workers' }))
    expect(screen.queryByTestId('add-machine')).toBeNull()
  })

  it('?tab=jobs&job=ID opens that job in My jobs; ?tab=worker selects This worker', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    mockFetch({
      '/trayapi/info': () => [200, info({ client: { running: true } })],
      '/local/longarm/v1/jobs': () => [200, { jobs: [fleetJob('01JZAAAAAAAAAAAAAAAAAAAAAA', 'build'), fleetJob('01JZBBBBBBBBBBBBBBBBBBX7K2')] }],
    })
    history.replaceState(null, '', '/tray?tab=jobs&job=x7k2')
    render(<TrayApp />)
    const row = await screen.findByRole('row', { name: /BBX7K2 on w1/ })
    await waitFor(() => expect(row.getAttribute('aria-expanded')).toBe('true'))
    expect(screen.getByRole('row', { name: /build on w1/ }).getAttribute('aria-expanded')).toBe('false')
    cleanup()
    history.replaceState(null, '', '/tray?tab=worker')
    render(<TrayApp />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'This worker' }).getAttribute('aria-selected')).toBe('true'))
  })

  it('findJob: the id, a unique start or end of it, else the name', () => {
    const jobs = [fleetJob('01JZAAAAAAAAAAAAAAAAAAX7K2', 'build'), fleetJob('01JZBBBBBBBBBBBBBBBBBBBBB1', 'test'), fleetJob('01JZBBBBBBBBBBBBBBBBBBBBB2')]
    expect(findJob(jobs, '01JZBBBBBBBBBBBBBBBBBBBBB1')?.job.spec.name).toBe('test')
    expect(findJob(jobs, 'x7k2')?.job.spec.name).toBe('build')
    expect(findJob(jobs, '01JZA')?.job.spec.name).toBe('build')
    expect(findJob(jobs, '01JZB')).toBeUndefined() // two of them
    expect(findJob(jobs, 'test')?.job.spec.id).toBe('01JZBBBBBBBBBBBBBBBBBBBBB1')
    expect(findJob(jobs, 'nope')).toBeUndefined()
  })
})
