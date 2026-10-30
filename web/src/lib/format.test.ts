import { describe, expect, it } from 'vitest'
import { cssColor, decodeB64, fmtAgo, fmtBytes, fmtRTT, fmtUptime, isActive, isRelayed, levelColor, logDecoder, memPct, nodeKey, num, shortIds, stateName, stripAnsi, until } from './format'
import { niceMax } from '../components/charts'
import { formatTools, parseTools } from '../panel/ProfilesPage'

describe('node ids', () => {
  it('shortIds: shortest unambiguous prefix, at least 8 characters (internal/nodeid.ShortIDs)', () => {
    const a = '3f2a9c1e-4b5d-4c3e-8f00-1234567890ab'
    const b = '3f2a9c1e-4b6d-4c3e-8f00-1234567890ab'
    const c = '77aa0000-0000-4000-8000-000000000001'
    const s = shortIds([a, b, c, undefined, ''])
    expect(s[c]).toBe('77aa0000')
    expect(s[a]).toBe('3f2a9c1e-4b5')
    expect(s[b]).toBe('3f2a9c1e-4b6')
    expect(Object.keys(s)).toHaveLength(3)
  })
  it('nodeKey: the id, else the name (an older hub)', () => {
    expect(nodeKey({ id: 'x', name: 'box' })).toBe('x')
    expect(nodeKey({ name: 'box' })).toBe('box')
  })
})

describe('format', () => {
  it('levelColor thresholds 60/85 (internal/tray/levels.json)', () => {
    expect(levelColor(82)).toBe('#d29922')
    expect(levelColor(10)).toBe('#3fb950')
    expect(levelColor(60)).toBe('#d29922')
    expect(levelColor(84.9)).toBe('#d29922')
    expect(levelColor(85)).toBe('#f85149')
    // the tray's themes recolour them; elsewhere the colour itself
    expect(cssColor(levelColor(10))).toBe('var(--c-green, #3fb950)')
    expect(cssColor('#123456')).toBe('#123456')
  })
  it('fmtUptime / fmtAgo', () => {
    expect(fmtUptime(3 * 86400 + 4 * 3600 + 5)).toBe('3d 4h')
    expect(fmtUptime(5 * 3600 + 12 * 60)).toBe('5h 12m')
    expect(fmtAgo(45)).toBe('45s')
    expect(fmtAgo(600)).toBe('10m')
    expect(fmtAgo(7200)).toBe('2h')
  })
  it('until: seconds to an RFC 3339 time, negative once past', () => {
    const now = Date.parse('2026-01-01T00:00:00Z')
    expect(until('2026-01-01T01:00:00Z', now)).toBe(3600)
    expect(until('2025-12-31T23:59:00Z', now)).toBe(-60)
    expect(until(undefined, now)).toBe(0)
  })
  it('64-bit numbers arrive as strings', () => {
    expect(num('17179869184')).toBe(17179869184)
    expect(num(undefined)).toBe(0)
    expect(memPct({ mem_total_bytes: '200', mem_used_bytes: '50' })).toBe(25)
    expect(memPct({})).toBe(0)
    expect(fmtBytes('5905580032')).toBe('5.5G')
  })
  it('RTT and path helpers', () => {
    expect(fmtRTT(0)).toBe('—')
    expect(fmtRTT(3.25)).toBe('3.3 ms')
    expect(fmtRTT(351.4)).toBe('351 ms')
    expect(isRelayed('relay')).toBe(true)
    expect(isRelayed('derp')).toBe(true)
    expect(isRelayed('direct')).toBe(false)
    expect(isRelayed('unknown')).toBe(false)
    expect(niceMax(37)).toBe(50)
    expect(niceMax(351)).toBe(500)
  })
  it('job states', () => {
    expect(stateName('JOB_STATE_RUNNING')).toBe('running')
    expect(isActive('JOB_STATE_PREPARING')).toBe(true)
    expect(isActive('JOB_STATE_DONE')).toBe(false)
  })
  it('decodes UTF-8 log data and strips ANSI', () => {
    const b64 = btoa(String.fromCharCode(...new TextEncoder().encode('héllo \x1b[31mred\x1b[0m\n')))
    expect(stripAnsi(decodeB64(b64))).toBe('héllo red\n')
  })
  it('profile tools text round trip', () => {
    const tools = parseTools('git=latest\nnode 22\n pnpm@9 , claude')
    expect(tools).toEqual([
      { name: 'git', version: 'latest' },
      { name: 'node', version: '22' },
      { name: 'pnpm', version: '9' },
      { name: 'claude', version: 'latest' },
    ])
    expect(formatTools(tools)).toBe('git=latest\nnode=22\npnpm=9\nclaude=latest')
  })
})

describe('logDecoder', () => {
  const b64 = (bytes: number[]) => btoa(String.fromCharCode(...bytes))
  it('joins UTF-8 characters and ANSI sequences split across chunks', () => {
    const d = logDecoder()
    const all = [...new TextEncoder().encode('é ok \x1b[31mred\x1b[0m!')]
    // split inside "é" (2 bytes) and inside the first escape sequence
    const out = d(b64(all.slice(0, 1))) + d(b64(all.slice(1, 8))) + d(b64(all.slice(8)))
    expect(out).toBe('é ok red!')
  })
})
