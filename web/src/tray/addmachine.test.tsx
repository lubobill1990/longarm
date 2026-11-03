import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AddMachineInfo } from '../api/local'
import type { PoolView } from '../api/types'
import { AddMachine, installSteps, type AddOptions } from './AddMachine'
import { PoolTab } from './PoolTab'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

const PUBLIC: AddMachineInfo = {
  hub_host: 'hub.example.com',
  version: '0.1.28',
  internal: false,
  repo: 'github.com/lubobill1990/longarm',
  public_repo: 'github.com/lubobill1990/longarm',
  internal_repo: 'github.com/gim-home/longarm',
}
const INTERNAL: AddMachineInfo = { ...PUBLIC, tenant: '72f988bf-86f1-41af-91ab-2d7cd011db47', internal: true, repo: 'github.com/gim-home/longarm' }

const opts = (o: Partial<AddOptions> = {}): AddOptions => ({ worker: true, admin: false, client: false, platform: 'windows', ...o })
const text = (o: AddOptions, info = PUBLIC) =>
  installSteps(o, info)
    .map((s) => s.cmd)
    .join('\n')

describe('installSteps', () => {
  it("Windows: the hub's install.ps1 (as on its home page), the roles, the admin worker", () => {
    expect(text(opts({ client: true, admin: true }))).toBe(
      "$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; $env:LONGARM_MSI_PROPS='ADMIN_WORKER=1'; irm https://hub.example.com/install.ps1 | iex",
    )
    expect(text(opts({ worker: false, client: true, admin: true }))).toBe("$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='client'; irm https://hub.example.com/install.ps1 | iex")
  })

  it("macOS and Linux: the hub's install.sh, the roles; never an admin worker", () => {
    expect(text(opts({ platform: 'unix', client: true, admin: true }))).toBe('curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both')
    expect(text(opts({ platform: 'unix' }))).toContain('--role worker')
  })

  it('the same on a Microsoft hub: from the hub, never GitHub', () => {
    const t = text(opts(), INTERNAL)
    expect(t).toContain('https://hub.example.com/install.ps1')
    expect(t).not.toMatch(/github|gh release/)
  })

  it('a release channel: the installer of that channel (quoted for the shell); stable: as ever', () => {
    expect(text(opts({ channel: 'dev' }))).toContain('irm https://hub.example.com/install.ps1?channel=dev | iex')
    expect(text(opts({ platform: 'unix', channel: 'preview' }))).toBe("curl -fsSL 'https://hub.example.com/install.sh?channel=preview' | sudo sh -s -- --login --role worker")
    expect(text(opts({ channel: 'stable' }))).toContain('/install.ps1 | iex')
  })

  it('no role or no hub: nothing', () => {
    expect(installSteps(opts({ worker: false }), PUBLIC)).toEqual([])
    expect(installSteps(opts(), { ...PUBLIC, hub_host: '' })).toEqual([])
  })
})

function mockAddMachine(info: AddMachineInfo) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      const body = url.startsWith('/trayapi/addmachine') ? info : { error: 'not found' }
      return new Response(JSON.stringify(body), { status: body === info ? 200 : 404, headers: { 'Content-Type': 'application/json' } })
    }),
  )
}

describe('AddMachine', () => {
  it('one command, no source to pick; the options change it', async () => {
    mockAddMachine(INTERNAL)
    render(<AddMachine onClose={() => {}} />)
    fireEvent.click(await screen.findByRole('radio', { name: 'Windows' }))
    expect(screen.queryByTestId('addm-source')).toBeNull()
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain("$env:LONGARM_ROLE='worker'; irm https://hub.example.com/install.ps1 | iex")
    fireEvent.click(screen.getByTestId('addm-client'))
    fireEvent.click(screen.getByTestId('addm-admin'))
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain("LONGARM_ROLE='both'; $env:LONGARM_MSI_PROPS='ADMIN_WORKER=1'")
    // the admin worker: Windows only
    fireEvent.click(screen.getByRole('radio', { name: 'macOS / Linux' }))
    expect((screen.getByTestId('addm-admin') as HTMLInputElement).disabled).toBe(true)
    expect(screen.getByTestId('addm-cmd-0').textContent).toBe('curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both')
    // nothing picked
    fireEvent.click(screen.getByTestId('addm-worker'))
    fireEvent.click(screen.getByTestId('addm-client'))
    expect(screen.getByTestId('addm-none')).toBeTruthy()
  })

  it('the Workers tab: the + button opens it, Back returns', async () => {
    mockAddMachine(PUBLIC)
    const pool: PoolView = {
      source: 'hub',
      hub: { address: 'hub.example.com:7443', connected: true },
      transport: 'race',
      workers: [{ name: 'w1', state: 'online', source: 'hub', node: { name: 'w1', role: 'worker', state: 'approved', online: true } }],
    } as PoolView
    render(<PoolTab pool={pool} />)
    fireEvent.click(screen.getByTestId('add-machine-fab'))
    expect(await screen.findByTestId('add-machine')).toBeTruthy()
    fireEvent.click(screen.getByTestId('add-machine-back'))
    expect(screen.queryByTestId('add-machine')).toBeNull()
    expect(screen.getByTestId('add-machine-fab')).toBeTruthy()
  })
})
