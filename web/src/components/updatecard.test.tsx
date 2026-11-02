import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { NodeInfo, NodeUpdate, PoolWorker } from '../api/types'
import { poolMachines } from '../lib/machines'
import { updateStateText, updateView } from '../lib/update'
import { WorkerCard } from './WorkerCard'

// The card of a machine that updates (lib/update.ts; the hub's
// NodeInfo.update): "Updating to v0.1.30…" with the phase instead of
// "offline, reconnecting", link errors and red RTT bands; a dismissible
// badge when it ended badly.

afterEach(cleanup)
beforeEach(() => localStorage.clear())

const now = Date.parse('2026-11-02T12:00:00Z')
const ago = (s: number) => new Date(now - s * 1000).toISOString()

function worker(name: string, state: string, node: Partial<NodeInfo> = {}, over: Partial<PoolWorker> = {}): PoolWorker {
  return {
    name,
    state,
    since: ago(34),
    source: 'none',
    error: state === 'offline' ? 'rpc error: code = DeadlineExceeded desc = context deadline exceeded' : undefined,
    node: { name, id: `${name}-0000-4000-8000-000000000000`, state: 'approved', role: 'worker', online: state === 'online', owner: 'alice', version: '0.1.29', ...node },
    link: {
      legs: [
        {
          transport: 'tcp',
          label: 'tcp',
          up: state === 'online',
          active: true,
          error: state === 'online' ? undefined : 'rpc error: code = DeadlineExceeded',
          history: [{ rtt_ms: 3 }, { rtt_ms: -1 }, { rtt_ms: -1 }],
        },
      ],
    },
    ...over,
  }
}

const updating = (u: Partial<NodeUpdate> = {}): NodeUpdate => ({
  state: 'updating',
  from: '0.1.29',
  to: '0.1.30',
  phase: 'installing',
  method: 'msi',
  initiator: 'owner',
  jobs_keep_running: true,
  started_at: ago(120),
  attempt: 'svc-1',
  ...u,
})

function card(workers: PoolWorker[]) {
  const { active, offline } = poolMachines(workers, now)
  const m = [...active, ...offline][0]
  render(<WorkerCard m={m} now={now} />)
  return screen.getByTestId(`worker-${m.key}`)
}

describe('WorkerCard while its machine updates', () => {
  it('shows "Updating to vX…" with the download progress, not offline', () => {
    const c = card([worker('box', 'online', { update: updating({ phase: 'downloading', bytes_done: String(12 << 20), bytes_total: String(42 << 20) }) })])
    expect(within(c).getByTestId('update-status').textContent).toContain('Updating to v0.1.30…')
    expect(within(c).getByTestId('update-detail').textContent).toBe('downloading 12/42 MB · 2m · by its owner')
    expect(within(c).getByRole('progressbar').getAttribute('aria-valuenow')).toBe('29')
    expect(within(c).getByTestId('update-status').textContent).toContain('running jobs and sessions keep running')
  })

  it('while it restarts: "waiting for the worker to come back", amber, no rpc errors, no red link (edge case 11)', () => {
    const c = card([worker('box', 'offline', { update: updating({ offline_since: ago(30) }) })])
    expect(c.getAttribute('data-state')).toBe('updating')
    expect(within(c).getByTestId('status').textContent).toBe('Updating to v0.1.30…')
    expect(within(c).getByTestId('update-detail').textContent).toContain('waiting for the worker to come back (last phase: installing)')
    expect(c.textContent).not.toContain('reconnecting')
    expect(c.textContent).not.toContain('DeadlineExceeded')
    expect(within(c).getByTestId('link-rtt').textContent).toBe('updating')
    expect(within(c).getByTestId('link-path').className).toContain('is-updating')
    expect(c.querySelector('.dot')?.className).toContain('is-updating')
  })

  it('session worker and admin worker updating together are one machine (edge case 6)', () => {
    const c = card([
      worker('box', 'offline', { companion: 'box-admin', companion_id: 'box-admin-0000-4000-8000-000000000000', update: updating({ via: 'box-admin' }) }),
      worker('box-admin', 'offline', { admin_worker: true, companion: 'box', companion_id: 'box-0000-4000-8000-000000000000', update: updating() }),
    ])
    expect(screen.getAllByTestId(/^worker-/)).toHaveLength(1)
    expect(within(c).getByTestId('cap-state').textContent).toBe('worker updating · admin updating')
    expect(c.textContent).not.toContain('offline')
  })

  it('a plain network blip without an update is offline, as ever (edge case 7)', () => {
    const c = card([worker('box', 'offline')])
    expect(c.getAttribute('data-state')).toBe('offline')
    expect(c.textContent).toContain('offline 34s, reconnecting')
    expect(within(c).getByTestId('link-rtt').textContent).toBe('down')
    expect(within(c).queryByTestId('update-status')).toBeNull()
  })

  it('an older worker (no phases): a rollout\'s generic "updating" with the time it takes (edge case 9)', () => {
    const c = card([worker('old', 'offline', { version: '0.1.20', update: { state: 'requested', from: '0.1.20', to: '0.1.30', initiator: 'rollout', started_at: ago(80) } })])
    expect(within(c).getByTestId('status').textContent).toBe('Update to v0.1.30 requested')
    expect(within(c).getByTestId('update-detail').textContent).toBe('waiting for the worker to come back · 80s · by a rollout')
  })

  it('an older worker the tray updates itself: generic "updating" from the tray\'s own job (edge case 9)', () => {
    const w = worker('old', 'offline', { version: '0.1.20' })
    const { active } = poolMachines([w], now)
    render(<WorkerCard m={active[0]} now={now} actions={{ updates: { old: { state: 'running', started: now - 45_000, to: '0.1.30' } } }} />)
    const c = screen.getByTestId('worker-old')
    expect(within(c).getByTestId('status').textContent).toBe('Updating to v0.1.30…')
    expect(within(c).getByTestId('update-detail').textContent).toBe('waiting for the worker to come back · 45s')
    expect(within(c).getByTestId('update-status').textContent).toContain('not confirmed')
  })

  it('the update button is off while it updates (edge case 5)', () => {
    const c = card([worker('box', 'online', { update: updating({ phase: 'verifying' }), update_available: { version: '0.1.30', method: 'msi' } })])
    // no actions: no button at all; with the owner's actions it is disabled
    expect(within(c).queryByTestId('update-btn')).toBeNull()
    cleanup()
    const { active } = poolMachines([worker('box', 'online', { update: updating({ phase: 'verifying' }), update_available: { version: '0.1.30', method: 'msi' } })], now)
    render(<WorkerCard m={active[0]} now={now} actions={{ update: () => {}, canUpdate: () => true }} />)
    const btn = screen.getByTestId('update-btn') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
    expect(btn.textContent).toBe('Updating…')
  })
})

describe('WorkerCard after an update', () => {
  it('failed: a dismissible badge with the reason, never "updating" (edge case 1)', () => {
    const u: NodeUpdate = { state: 'failed', from: '0.1.29', to: '0.1.30', code: 'signature', error: 'refusing longarm-windows-amd64.msi 0.1.30: invalid signature', attempt: 'a1' }
    const c = card([worker('box', 'online', { update: u })])
    const b = within(c).getByTestId('update-badge')
    expect(b.textContent).toContain('Update to v0.1.30 failed: refusing longarm-windows-amd64.msi 0.1.30: invalid signature')
    expect(within(c).queryByTestId('update-status')).toBeNull()
    fireEvent.click(within(b).getByLabelText('dismiss'))
    expect(within(c).queryByTestId('update-badge')).toBeNull()
    // dismissed on this computer: stays dismissed
    cleanup()
    expect(within(card([worker('box', 'online', { update: u })])).queryByTestId('update-badge')).toBeNull()
  })

  it('another user sees the code in words (no worker text reaches them)', () => {
    const c = card([worker('box', 'online', { update: { state: 'failed', to: '0.1.30', from: '0.1.29', code: 'sha256' } })])
    expect(within(c).getByTestId('update-badge').textContent).toContain('the download is corrupt (sha256 mismatch)')
  })

  it("didn't finish by the deadline: offline, with the last phase and when it started (edge case 2)", () => {
    const c = card([worker('box', 'offline', { update: updating({ state: 'timed_out', code: 'timeout', started_at: ago(7 * 60) }) })])
    expect(c.getAttribute('data-state')).toBe('offline')
    expect(within(c).getByTestId('update-badge').textContent).toContain("Offline: update to v0.1.30 didn't finish")
    expect(within(c).getByTestId('update-badge').textContent).toContain('last phase: installing, started 7m ago')
  })

  it('restart pending (msiexec 3010): guidance, not a failure (edge case 3)', () => {
    const c = card([worker('box', 'online', { version: '0.1.30', update: { state: 'restart_pending', from: '0.1.29', to: '0.1.30' } })])
    const b = within(c).getByTestId('update-badge')
    expect(b.getAttribute('data-kind')).toBe('restart_pending')
    expect(b.textContent).toContain('restart the machine to finish the update')
  })

  it('came back on another version (edge case 12)', () => {
    const c = card([worker('box', 'online', { version: '0.1.31', update: { state: 'mismatch', to: '0.1.30', error: 'came back on 0.1.31, not 0.1.30' } })])
    expect(within(c).getByTestId('update-badge').textContent).toContain('came back on 0.1.31, not 0.1.30')
  })
})

describe('updateStateText (the panel; longarm ls says the same)', () => {
  it('names the versions and the phase', () => {
    expect(updateStateText(updating(), true)).toBe('updating 0.1.29→0.1.30 (installing)')
    expect(updateStateText(updating(), false)).toBe('updating 0.1.29→0.1.30 (restarting, last installing)')
    expect(updateStateText({ state: 'failed', to: '0.1.30', code: 'disk' }, true)).toBe('update to 0.1.30 failed (disk)')
    expect(updateStateText(undefined, true)).toBe('')
    expect(updateView(updating({ phase: 'health_check' }), true, now).detail).toContain('checking the new version')
  })
})
