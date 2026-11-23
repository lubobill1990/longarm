import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { PoolView, PoolWorker } from '../api/types'
import { Toasts } from '../components/common'
import { ConfirmHost } from '../components/confirm'
import { nodeNameError } from '../lib/names'
import { PoolTab } from './PoolTab'

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

const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString()

function w(name: string, owner: string, online = true): PoolWorker {
  return {
    name,
    state: online ? 'online' : 'offline',
    source: online ? 'hub' : 'none',
    since: ago(online ? 60 : 30 * 3600),
    node: { name, role: 'worker', state: 'approved', online, owner, hostname: name, last_seen: ago(online ? 1 : 30 * 3600) },
  }
}

function pool(identity = 'client/alice-laptop'): PoolView {
  return {
    source: 'hub',
    hub: { address: 'hub.example.com:7443', connected: true },
    identity,
    user: 'alice',
    workers: [w('box', 'alice'), w('bobs', 'bob'), w('fleet-1', ''), w('old-box', 'alice', false)],
  }
}

/** Opens a card's … menu: its items. */
const menu = (name: string) => {
  const card = screen.getByTestId(`worker-${name}`)
  fireEvent.click(within(card).getByLabelText('more actions'))
  return within(card)
    .queryAllByRole('menuitem')
    .map((m) => m.textContent)
}

describe('rename', () => {
  it('checks names as the hub does', () => {
    expect(nodeNameError('box-2')).toBe('')
    expect(nodeNameError('-box')).not.toBe('')
    expect(nodeNameError('a b')).not.toBe('')
    expect(nodeNameError('x'.repeat(64))).not.toBe('')
    expect(nodeNameError('id:box')).not.toBe('')
    expect(nodeNameError('beef-01')).toContain('node ID')
  })

  it("the owner's workers only; our dialog checks the name; the daemon renames", async () => {
    const calls = mockFetch({ '/local/longarm/v1/workers/box/rename': () => [200, { name: 'box2', state: 'approved' }] })
    render(
      <Toasts>
        <ConfirmHost />
        <PoolTab pool={pool()} />
      </Toasts>,
    )
    expect(menu('bobs')).not.toContain('Rename bobs…')
    expect(menu('fleet-1')).not.toContain('Rename fleet-1…')
    expect(menu('box')).toContain('Rename box…')
    fireEvent.click(within(screen.getByTestId('worker-box')).getByText('Rename box…'))
    const field = (await screen.findByLabelText('new name')) as HTMLInputElement
    expect(field.value).toBe('box')
    fireEvent.change(field, { target: { value: 'beef' } })
    expect(screen.getByTestId('ask-problem').textContent).toContain('node ID')
    const ok = within(screen.getByRole('alertdialog')).getByText('Rename') as HTMLButtonElement
    expect(ok.disabled).toBe(true)
    fireEvent.change(field, { target: { value: ' box2 ' } })
    expect(screen.queryByTestId('ask-problem')).toBeNull()
    fireEvent.click(ok)
    await waitFor(() => {
      const c = calls.find((x) => x.url === '/local/longarm/v1/workers/box/rename')
      expect(c?.init?.method).toBe('POST')
      expect(JSON.parse(String(c?.init?.body))).toEqual({ new_name: 'box2' })
    })
    expect(await screen.findByText('box renamed to box2')).toBeTruthy()
    // a long-offline one of hers: Rename on its line
    expect(within(screen.getByTestId('offline-old-box')).getByText('Rename')).toBeTruthy()
  })

  it("admins: every worker; the hub's refusal without gRPC's prefix", async () => {
    mockFetch({
      '/local/longarm/v1/workers/box/rename': () => [400, { error: 'rpc error: code = AlreadyExists desc = the name "box2" is taken' }],
    })
    vi.spyOn(window, 'prompt').mockReturnValue('box2')
    render(
      <Toasts>
        <PoolTab pool={pool('admin/root')} />
      </Toasts>,
    )
    expect(menu('bobs')).toContain('Rename bobs…')
    expect(menu('fleet-1')).toContain('Rename fleet-1…')
    menu('box')
    fireEvent.click(within(screen.getByTestId('worker-box')).getByText('Rename box…'))
    expect(await screen.findByText('Rename box: the name "box2" is taken')).toBeTruthy()
  })
})
