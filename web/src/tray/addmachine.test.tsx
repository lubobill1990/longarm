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

const opts = (o: Partial<AddOptions> = {}): AddOptions => ({ worker: true, admin: false, client: false, platform: 'windows', arch: 'amd64', internal: false, ...o })
const text = (o: AddOptions, info = PUBLIC) =>
  installSteps(o, info)
    .map((s) => s.cmd)
    .join('\n')

describe('installSteps', () => {
  it('Windows: the MSI from the GitHub Release, the roles, the hub, the admin worker', () => {
    const t = text(opts({ client: true, admin: true }))
    expect(t).toContain('https://github.com/lubobill1990/longarm/releases/download/v0.1.28/longarm-windows-amd64.msi')
    expect(t).toContain('MODE=custom ROLES=both HUB=hub.example.com LOGIN=1 ADMIN_WORKER=1')
    expect(t).toContain('longarmd login hub.example.com --role both')
    expect(text(opts({ worker: false, client: true, admin: true }))).toMatch(/ROLES=client HUB=hub.example.com LOGIN=1"/)
    expect(text(opts({ arch: 'arm64' }))).toContain('longarm-windows-arm64.msi')
  })

  it('the internal copy: gh release download from gim-home', () => {
    const t = text(opts({ internal: true }), INTERNAL)
    expect(t).toContain('gh release download v0.1.28 -R github.com/gim-home/longarm -p longarm-windows-amd64.msi -D $env:TEMP --clobber')
    expect(t).not.toContain('Invoke-WebRequest')
  })

  it('Linux and macOS: the package, longarmd login, the service; a client: longarm login', () => {
    const deb = text(opts({ platform: 'deb' }))
    expect(deb).toContain('curl -fsSL -o /tmp/longarm-linux-amd64.deb https://github.com/lubobill1990/longarm/releases/download/v0.1.28/longarm-linux-amd64.deb')
    expect(deb).toContain('sudo apt-get install -y /tmp/longarm-linux-amd64.deb')
    expect(deb).toContain('longarmd login hub.example.com --role worker')
    expect(deb).toContain('sudo systemctl enable --now "longarmd@$USER"')
    expect(text(opts({ platform: 'rpm', arch: 'arm64' }))).toContain('sudo dnf install -y /tmp/longarm-linux-arm64.rpm')
    const mac = text(opts({ platform: 'macos', arch: 'arm64', admin: true }))
    expect(mac).toContain('sudo installer -pkg /tmp/longarm-darwin-arm64.pkg -target /')
    expect(mac).toContain('sudo longarmd service install --user "$USER"')
    expect(mac).not.toContain('ADMIN')
    const client = text(opts({ platform: 'deb', worker: false, client: true }))
    expect(client).toContain('longarm login hub.example.com')
    expect(client).not.toContain('longarmd')
  })

  it('no version: the latest release; no role or no hub: nothing', () => {
    expect(text(opts(), { ...PUBLIC, version: undefined })).toContain('/releases/latest/download/longarm-windows-amd64.msi')
    expect(text(opts({ internal: true }), { ...INTERNAL, version: undefined })).toContain('gh release download -R github.com/gim-home/longarm')
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
  it('an internal hub downloads from gim-home; the options change the commands', async () => {
    mockAddMachine(INTERNAL)
    render(<AddMachine onClose={() => {}} />)
    expect(await screen.findByTestId('addm-internal-note')).toBeTruthy()
    fireEvent.click(screen.getByRole('radio', { name: 'Windows' }))
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain('-R github.com/gim-home/longarm')
    fireEvent.click(screen.getByTestId('addm-client'))
    fireEvent.click(screen.getByTestId('addm-admin'))
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain('ROLES=both HUB=hub.example.com LOGIN=1 ADMIN_WORKER=1')
    // the admin worker: Windows only
    fireEvent.click(screen.getByRole('radio', { name: 'Linux (deb)' }))
    expect((screen.getByTestId('addm-admin') as HTMLInputElement).disabled).toBe(true)
    expect(screen.getByTestId('addm-cmd-1').textContent).toContain('--role both')
    // the public repository instead
    fireEvent.change(screen.getByTestId('addm-source'), { target: { value: 'public' } })
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain('https://github.com/lubobill1990/longarm/releases/download/v0.1.28/longarm-linux-amd64.deb')
    expect(screen.queryByTestId('addm-internal-note')).toBeNull()
    // nothing picked
    fireEvent.click(screen.getByTestId('addm-worker'))
    fireEvent.click(screen.getByTestId('addm-client'))
    expect(screen.getByTestId('addm-none')).toBeTruthy()
  })

  it('a public hub downloads from GitHub', async () => {
    mockAddMachine(PUBLIC)
    render(<AddMachine onClose={() => {}} />)
    fireEvent.click(await screen.findByRole('radio', { name: 'macOS' }))
    expect(screen.getByTestId('addm-cmd-0').textContent).toContain('https://github.com/lubobill1990/longarm/releases/download/v0.1.28/longarm-darwin-amd64.pkg')
    expect(screen.queryByTestId('addm-internal-note')).toBeNull()
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
