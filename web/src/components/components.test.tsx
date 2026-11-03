import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { PoolWorker } from '../api/types'
import { Bar, Sparkline } from './charts'
import { FirewallWarning, psCommand, psQuote } from './common'
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
    // every leg only on request: the one in use outside, all of them in the dropdown
    expect(screen.queryByTestId('legs')).toBeNull()
    fireEvent.click(screen.getByTestId('legs-toggle'))
    const legs = screen.getByTestId('legs')
    expect(legs.querySelector('.leg-row.active')?.textContent).toContain('longnet relay-udp')
    expect(legs.querySelector('.leg-row.active')?.textContent).toContain('✓')
    expect(legs.querySelector('.leg-row.down')?.textContent).toContain('tcp')
    fireEvent.click(screen.getByTestId('legs-toggle'))
    expect(screen.queryByTestId('legs')).toBeNull()
  })

  it('disk and network are off until toggled in the chart, and stay on for every card', () => {
    localStorage.removeItem('longarm.metrics.extra')
    const w = onlineWorker()
    w.metrics = { ...w.metrics, net_rx_bytes_per_sec: '1572864', net_tx_bytes_per_sec: '2048' }
    w.history = [
      { cpu: 10, mem: 50, disk: 40, net_rx: 1000, net_tx: 10 },
      { cpu: 20, mem: 60, disk: 41, net_rx: 1572864, net_tx: 2048 },
    ]
    const { container } = render(<WorkerCard w={w} now={now} />)
    const charts = () => container.querySelectorAll('svg.spark').length
    // the metrics chart: the one with the MEM key
    const lines = () => screen.getByText(/MEM/, { selector: '.spark-key' }).closest('.spark-wrap')!.querySelectorAll('path').length
    expect(screen.queryByLabelText('DISK')).toBeNull()
    expect(lines()).toBe(2)
    const before = charts()
    fireEvent.click(screen.getByRole('button', { name: /disk/i }))
    expect(screen.getByLabelText('DISK')).toBeTruthy()
    expect(lines()).toBe(3)
    fireEvent.click(screen.getByRole('button', { name: /net/i }))
    expect(screen.getByTestId('net-now').textContent).toContain('↓ 1.5 MB/s')
    expect(screen.getByTestId('net-now').textContent).toContain('↑ 2 KB/s')
    // in and out join the same chart (on their own scale), no second chart
    expect(lines()).toBe(5)
    expect(charts()).toBe(before)
    expect(JSON.parse(localStorage.getItem('longarm.metrics.extra') ?? '{}')).toEqual({ disk: true, net: true })
    cleanup()
    render(<WorkerCard w={w} now={now} />) // another card, later: still on
    expect(screen.getByLabelText('DISK')).toBeTruthy()
    expect(screen.getByRole('button', { name: /net/i }).getAttribute('aria-pressed')).toBe('true')
    localStorage.removeItem('longarm.metrics.extra')
  })

  it('hovering a chart shows every series at that moment', () => {
    const w = onlineWorker()
    w.history = [
      { cpu: 10, mem: 50 },
      { cpu: 22, mem: 61 },
    ]
    render(<WorkerCard w={w} now={now} />)
    const svg = screen.getAllByRole('img', { name: 'sparkline' }).find((el) => el.closest('[data-testid="link"]') === null)!
    svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 400, height: 46, right: 400, bottom: 46, x: 0, y: 0, toJSON: () => ({}) })
    fireEvent.mouseMove(svg, { clientX: 400 })
    const tip = screen.getByTestId('spark-tip')
    expect(tip.textContent).toContain('now')
    expect(tip.textContent).toContain('CPU')
    expect(tip.textContent).toContain('22%')
    expect(tip.textContent).toContain('61%')
    fireEvent.mouseLeave(svg)
    expect(screen.queryByTestId('spark-tip')).toBeNull()
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
    // PowerShell: a quoted path needs the call operator (older builds send it without)
    expect(screen.getByTestId('firewall-fix').textContent).toBe("& 'C:/Program Files/Longarm/longarm.exe' firewall allow")
    expect(screen.getByTestId('firewall-warning').textContent).toContain('elevated PowerShell')
    expect(screen.queryByTestId('firewall-allow')).toBeNull()
    const current = "& 'C:/Program Files/Longarm/longarm.exe' firewall allow"
    rerender(<FirewallWarning fw={{ ok: false, problem: 'p', fix: current }} />)
    expect(screen.getByTestId('firewall-fix').textContent).toBe(current)
    rerender(<FirewallWarning fw={{ ok: true }} />)
    expect(screen.queryByTestId('firewall-warning')).toBeNull()
    rerender(<FirewallWarning />)
    expect(screen.queryByTestId('firewall-warning')).toBeNull()
  })

  it('allows the program with one click; says when it is a copy another app ships', async () => {
    const program = 'C:\\Users\\me\\tmia-desktop-live\\longarm-1.2.3\\longarm.exe'
    const onAllow = vi.fn().mockResolvedValue({ program, ok: true, message: 'Allowed: inbound connections pass.' })
    render(
      <FirewallWarning
        fw={{ ok: false, program, problem: 'no inbound allow rule', fix: program + ' firewall allow' }}
        installDir={'C:\\Program Files\\Longarm'}
        onAllow={onAllow}
      />,
    )
    expect(screen.getByTestId('firewall-copy').textContent).toContain('TMIA Desktop')
    expect(screen.getByTestId('firewall-warning').textContent).toContain('Or in an elevated PowerShell')
    fireEvent.click(screen.getByTestId('firewall-allow'))
    expect(await screen.findByTestId('firewall-fixed')).toBeTruthy()
    expect(onAllow).toHaveBeenCalledOnce()
    expect(screen.queryByTestId('firewall-warning')).toBeNull()
  })

  it('no copy note for the installed longarm.exe', () => {
    render(
      <FirewallWarning
        fw={{ ok: false, program: 'C:/Program Files/Longarm/longarm.exe', problem: 'p' }}
        installDir={'c:\\program files\\longarm\\'}
        onAllow={vi.fn()}
      />,
    )
    expect(screen.queryByTestId('firewall-copy')).toBeNull()
    expect(screen.getByTestId('firewall-allow')).toBeTruthy()
  })
})

describe('psCommand', () => {
  it('quotes the old double-quoted form again, so a path stays one argument', () => {
    // PowerShell takes \u201c \u201d \u201e for " and \u2018 \u2019 \u201a \u201b for '
    const evil = 'C:/x\u201d; calc; #/longarm.exe'
    expect(psCommand(`"${evil}" firewall allow`)).toBe("& 'C:/x\u201d; calc; #/longarm.exe' firewall allow")
    expect(psCommand('longarm firewall allow --program "C:/a\u2019; calc; #/longarm.exe"')).toBe(
      "longarm firewall allow --program 'C:/a\u2019\u2019; calc; #/longarm.exe'",
    )
    expect(psCommand('"C:/a $x `b/longarm.exe" firewall allow')).toBe("& 'C:/a $x `b/longarm.exe' firewall allow")
    expect(psCommand('"C:\\x\\longarm.exe" firewall allow')).toBe('C:\\x\\longarm.exe firewall allow')
    expect(psQuote("it's \u2018\u2019\u201a\u201b")).toBe("'it''s \u2018\u2018\u2019\u2019\u201a\u201a\u201b\u201b'")
  })
  it('refuses what no paste-ready line can carry', () => {
    expect(psCommand('"C:/a\nb/longarm.exe" firewall allow')).toBeUndefined()
    expect(psCommand('& "C:/x/longarm.exe" firewall allow; calc')).toBeUndefined()
    expect(psCommand('"-x" firewall allow')).toBeUndefined()
    render(<FirewallWarning fw={{ ok: false, problem: 'p', fix: '"C:/a\u0007/longarm.exe" firewall allow' }} />)
    expect(screen.queryByTestId('firewall-fix')).toBeNull()
    expect(screen.getByTestId('firewall-fix-unsupported').textContent).toContain('unsupported characters')
  })
})
