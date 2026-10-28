import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { PoolWorker } from '../api/types'
import { Bar, Sparkline } from './charts'
import { FirewallWarning } from './common'
import { WorkerCard } from './WorkerCard'

afterEach(cleanup)

const now = Date.parse('2026-10-23T12:00:00Z')
const ago = (s: number) => new Date(now - s * 1000).toISOString()
const GiB = 1024 ** 3

function onlineWorker(over: Partial<PoolWorker> = {}): PoolWorker {
  return {
    name: 'w1',
    state: 'online',
    since: ago(600),
    source: 'hub',
    metrics: {
      cpu_percent: 42,
      threads: 32,
      nproc: 410,
      mem_total_bytes: String(128 * GiB),
      mem_used_bytes: String(120 * GiB),
      jobs_running: 2,
      uptime_seconds: '90000',
      agents: { claude: { sessions: 3, mem_bytes: String(4.5 * GiB) }, copilot: { sessions: 0 } },
      top_cpu: [{ name: 'node', count: 21, cpu_percent: 33, mem_bytes: String(5.5 * GiB) }],
      top_mem: [{ name: 'node', count: 21, mem_bytes: String(5.5 * GiB) }],
      disk_free_bytes: String(300 * GiB),
      disk_total_bytes: String(1000 * GiB),
    },
    history: [
      { cpu: 10, mem: 50 },
      { cpu: 40, mem: 90 },
    ],
    link: {
      state: 'online',
      active: 'longnet relay-udp',
      rtt_ms: 351,
      legs: [
        { transport: 'tcp', label: 'tcp', up: false, error: 'refused' },
        {
          transport: 'longnet',
          label: 'longnet relay-udp',
          kind: 'relay',
          nat: 'hard/hard',
          up: true,
          active: true,
          rtt_ms: 351,
          relay_since: ago(400),
          history: [{ rtt_ms: 340 }, { rtt_ms: -1 }, { rtt_ms: 351 }],
        },
      ],
    },
    my_jobs: 1,
    ...over,
  }
}

describe('WorkerCard', () => {
  it('shows metrics, agents, top processes and the link (path, NAT, RTT, relayed time)', () => {
    render(<WorkerCard w={onlineWorker()} now={now} />)
    expect(screen.getByText('w1')).toBeTruthy()
    expect(screen.getByText('online 10m')).toBeTruthy()
    expect(screen.getByLabelText('MEM').getAttribute('aria-valuenow')).toBe('94')
    expect(screen.getByText('120.0 / 128 GB')).toBeTruthy()
    expect(screen.getByTestId('agent-claude').textContent).toContain('3')
    expect(screen.getByTestId('agent-claude').textContent).toContain('4.5G')
    expect(screen.getByTestId('agent-copilot').textContent).toContain('—')
    expect(screen.getAllByText('node ×21').length).toBe(2)
    expect(screen.getByText('5.5G')).toBeTruthy()
    expect(screen.getByTestId('link-path').textContent).toBe('longnet relay-udp')
    expect(screen.getByTestId('link-rtt').textContent).toBe('351 ms')
    expect(screen.getByText('nat hard/hard')).toBeTruthy()
    expect(screen.getByText('relayed 6m')).toBeTruthy()
    expect(screen.getByText(/mine 1/)).toBeTruthy()
    expect(screen.getByText(/300 GB free/)).toBeTruthy()
    // the failed leg is struck through
    expect(document.querySelector('.leg.down')?.textContent).toContain('tcp')
  })

  it('says where the metrics come from and grays them out when stale', () => {
    render(<WorkerCard w={onlineWorker({ metrics: { ...onlineWorker().metrics, time: ago(3) } })} now={now} />)
    expect(screen.getByTestId('metrics-source').textContent).toBe('metrics via hub · updated 3s ago')
    expect(document.querySelector('[data-stale]')).toBeNull()
    cleanup()
    render(<WorkerCard w={onlineWorker({ source: 'none', metrics: { ...onlineWorker().metrics, time: ago(120) } })} now={now} />)
    expect(screen.getByTestId('metrics-source').textContent).toBe('last known metrics · updated 2m ago')
    expect(document.querySelector('[data-stale="true"]')).not.toBeNull()
  })

  it('a worker on its loopback only: unreachable from here, not a bare "down"', () => {
    render(<WorkerCard w={onlineWorker({ link: undefined, node: { name: 'w1', endpoints: ['tcp://127.0.0.1:7701'] } })} now={now} />)
    expect(screen.getByTestId('link-path').textContent).toBe('tcp (loopback only): unreachable from here')
    expect(screen.queryByText('down')).toBeNull()
  })

  it('offline: red status line with the error instead of metrics', () => {
    render(<WorkerCard w={onlineWorker({ state: 'offline', source: 'none', since: ago(120), error: 'connection refused', link: undefined })} now={now} />)
    expect(screen.getByText('offline 2m, reconnecting')).toBeTruthy()
    expect(screen.getByText('connection refused')).toBeTruthy()
    expect(screen.queryByLabelText('CPU')).toBeNull()
    expect(screen.getByTestId('worker-w1').getAttribute('data-state')).toBe('offline')
  })

  it('actions call back with the worker name', () => {
    const attach = vi.fn()
    const drain = vi.fn()
    render(<WorkerCard w={onlineWorker({ node: { name: 'w1', draining: true, admin_drain: true } })} now={now} actions={{ attach, drain }} />)
    fireEvent.click(screen.getByText('attach'))
    fireEvent.click(screen.getByText('resume'))
    expect(attach).toHaveBeenCalledWith('w1')
    expect(drain).toHaveBeenCalledWith('w1', false)
    expect(screen.getByText('draining')).toBeTruthy()
  })
})

describe('charts', () => {
  it('Bar clamps and colours by level', () => {
    render(<Bar label="CPU" pct={140} value="140%" />)
    const m = screen.getByLabelText('CPU')
    expect(m.getAttribute('aria-valuenow')).toBe('100')
    // red (levels.json), themed where the page has the tokens (cssColor)
    expect((m.querySelector('.fill') as HTMLElement).getAttribute('style')).toContain('#f85149')
  })
  it('Sparkline breaks lines at failed samples', () => {
    const { container } = render(<Sparkline series={[{ values: [1, 2, null, 3, 4], color: '#fff' }]} slots={5} max="auto" legend />)
    const d = container.querySelector('path')!.getAttribute('d')!
    expect(d.match(/M/g)!.length).toBe(2)
    expect(screen.getByText('max 10')).toBeTruthy()
  })
})

describe('FirewallWarning', () => {
  it('shows the problem and the exact fix command, nothing when ok', () => {
    const fix = '"C:/Program Files/Longarm/longarm.exe" firewall allow'
    const { rerender } = render(
      <FirewallWarning
        fw={{ ok: false, problem: 'Windows Firewall blocks inbound UDP to C:/x/longarm.exe (Block rule "longarm.exe (public)")', fix }}
        relayed={['wa (longnet relay-udp)']}
      />,
    )
    expect(screen.getByTestId('firewall-warning').textContent).toContain('Block rule')
    expect(screen.getByTestId('firewall-warning').textContent).toContain('wa (longnet relay-udp)')
    expect(screen.getByTestId('firewall-fix').textContent).toBe(fix)
    rerender(<FirewallWarning fw={{ ok: true }} />)
    expect(screen.queryByTestId('firewall-warning')).toBeNull()
    rerender(<FirewallWarning />)
    expect(screen.queryByTestId('firewall-warning')).toBeNull()
  })
})
