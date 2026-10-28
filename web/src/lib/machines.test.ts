import { describe, expect, it } from 'vitest'
import type { NodeInfo, PoolWorker } from '../api/types'
import { fmtLastSeen, groupMachines, LONG_OFFLINE_S, nodeMachines, poolMachines, sameMachine } from './machines'

const now = Date.parse('2026-10-28T12:00:00Z')
const ago = (s: number) => new Date(now - s * 1000).toISOString()

function node(name: string, over: Partial<NodeInfo> = {}): NodeInfo {
  return { name, role: 'worker', state: 'approved', online: true, owner: 'alice', hostname: name.replace(/-admin$/, ''), ...over }
}

function worker(name: string, over: Partial<PoolWorker> = {}, n: Partial<NodeInfo> = {}): PoolWorker {
  return { name, state: 'online', since: ago(600), source: 'hub', node: node(name, n), ...over }
}

describe('groupMachines', () => {
  it('pairs <worker>-admin with its session worker: same owner and host', () => {
    const ws = [
      node('cpc-alice-x7k2q', { hostname: 'CPC-ALICE-X7K2Q.corp' }),
      node('cpc-alice-x7k2q-admin', { admin_worker: true, hostname: 'cpc-alice-x7k2q' }),
      node('dev'),
      node('dev-admin', { admin_worker: true, owner: 'mallory' }), // another owner
      node('lab'),
      node('lab-admin', { admin_worker: true, hostname: 'elsewhere' }), // another host
      node('solo-admin', { admin_worker: true }), // admin only
      node('fleet', { owner: '' }),
      node('fleet-admin', { admin_worker: true, owner: '' }),
    ]
    const gs = groupMachines(
      ws,
      (n) => n.name,
      (n) => n,
    )
    const view = gs.map((g) => `${g.key}:${g.session?.name ?? '-'}+${g.admin?.name ?? '-'}`)
    expect(view).toEqual([
      'cpc-alice-x7k2q:cpc-alice-x7k2q+cpc-alice-x7k2q-admin',
      'dev:dev+-',
      'dev-admin:-+dev-admin',
      'lab:lab+-',
      'lab-admin:-+lab-admin',
      'solo-admin:-+solo-admin',
      'fleet:fleet+-',
      'fleet-admin:-+fleet-admin',
    ])
  })

  it("follows the hub's companion and the reserved label", () => {
    const ws = [node('box'), node('box-sys', { companion: 'box', hostname: 'box', labels: { 'longarm.io/admin': 'true' } })]
    const gs = groupMachines(
      ws,
      (n) => n.name,
      (n) => n,
    )
    expect(gs).toHaveLength(1)
    expect(gs[0].admin?.name).toBe('box-sys')
    expect(sameMachine(ws[0], { ...ws[1], state: 'revoked' })).toBe(false)
  })
})

describe('poolMachines', () => {
  it('one card per machine, online first, long-offline ones collapsed', () => {
    const ws: PoolWorker[] = [
      worker('old-pair', { state: 'offline', since: ago(60), error: 'rpc error: unavailable' }, { online: false, last_seen: ago(26 * 3600) }),
      worker('old-pc', { state: 'offline', error: 'unreachable' }, { online: false, last_seen: ago(25 * 3600) }),
      worker('blip', { state: 'offline', since: ago(120) }, { online: false, last_seen: ago(120) }), // just went: stays up top
      worker('cpc-alice-x7k2q', {}, { labels: { pool: 'devbox', os: 'windows', arch: 'amd64' } }),
      worker('cpc-alice-x7k2q-admin', {}, { admin_worker: true, labels: { 'longarm.io/admin': 'true' } }),
      worker('e2e-worker', { state: 'offline' }, { online: false, last_seen: ago(LONG_OFFLINE_S + 60) }),
      worker('connecting', { state: 'connecting' }),
    ]
    const { active, offline } = poolMachines(ws, now)
    expect(active.map((m) => m.key)).toEqual(['cpc-alice-x7k2q', 'connecting', 'blip'])
    expect(offline.map((m) => m.key)).toEqual(['e2e-worker', 'old-pair', 'old-pc'])
    const cpc = active[0]
    expect(cpc.session?.name).toBe('cpc-alice-x7k2q')
    expect(cpc.admin?.name).toBe('cpc-alice-x7k2q-admin')
    expect(cpc.primary.name).toBe('cpc-alice-x7k2q')
    expect(cpc.labels).toEqual([['pool', 'devbox']])
    expect(Math.round(offline[1].offlineFor / 3600)).toBe(26)
  })

  it("metrics: the session worker's, the admin worker's while the session worker is offline", () => {
    const ws = [worker('box', { state: 'offline' }, { online: false, last_seen: ago(30) }), worker('box-admin', {}, { admin_worker: true })]
    const { active } = poolMachines(ws, now)
    expect(active).toHaveLength(1)
    expect(active[0].state).toBe('online')
    expect(active[0].primary.name).toBe('box-admin')
  })

  it('a machine is long-offline only when all its workers are', () => {
    const ws = [worker('box', { state: 'offline' }, { online: false, last_seen: ago(5 * 3600) }), worker('box-admin', {}, { admin_worker: true })]
    expect(poolMachines(ws, now).offline).toHaveLength(0)
  })
})

describe('nodeMachines (panel)', () => {
  it('session worker then its admin worker; online machines first, revoked last', () => {
    const gs = nodeMachines([
      node('a-old', { online: false }),
      node('b', { online: true }),
      node('b-admin', { admin_worker: true, online: false }),
      node('a-gone', { state: 'revoked', online: false }),
      node('c', { online: true }),
    ])
    expect(gs.map((g) => g.key)).toEqual(['b', 'c', 'a-old', 'a-gone'])
    expect(gs[0].admin?.name).toBe('b-admin')
  })
})

describe('fmtLastSeen', () => {
  it('seconds, minutes, hours, days', () => {
    expect(fmtLastSeen(30)).toBe('30s')
    expect(fmtLastSeen(600)).toBe('10m')
    expect(fmtLastSeen(26 * 3600)).toBe('26h')
    expect(fmtLastSeen(3 * 86400 + 5)).toBe('3d')
  })
})
