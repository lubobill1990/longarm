import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AgentSkillStatus } from '../api/local'
import { Toasts } from '../components/common'
import { AgentSkill } from './AgentSkill'

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
      const key = Object.keys(routes).find((k) => url === k)
      const [status, body] = key ? routes[key](init) : [404, { error: 'not found' }]
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }),
  )
  return calls
}

const agent = (id: string, name: string, over: Partial<AgentSkillStatus> = {}): AgentSkillStatus => ({
  agent: id,
  name,
  detected: true,
  dir: `/home/you/.${id}/skills/longarm`,
  state: 'absent',
  bundled_version: 'v0.1.25',
  ...over,
})

const list = (...agents: AgentSkillStatus[]) => ({ bundled_version: 'v0.1.25', agents })

function show() {
  render(
    <Toasts>
      <AgentSkill />
    </Toasts>,
  )
}

describe('Settings > Agent skill', () => {
  it('lists each agent with detection and the installed vs bundled version', async () => {
    mockFetch({
      '/trayapi/skills': () => [
        200,
        list(
          agent('claude', 'Claude Code', { state: 'current', installed_version: 'v0.1.25' }),
          agent('copilot', 'GitHub Copilot CLI', { state: 'outdated', installed_version: 'v0.1.24' }),
          agent('codex', 'OpenAI Codex CLI', { detected: false }),
        ),
      ],
    })
    show()
    const section = await screen.findByTestId('agent-skill')
    expect(section.textContent).toContain('Bundled with Longarm v0.1.25')
    const claude = await screen.findByTestId('skill-claude')
    expect(within(claude).getByTestId('skill-state-claude').textContent).toBe('installed v0.1.25')
    expect(within(claude).getByText('detected')).toBeTruthy()
    expect(within(claude).getByRole('button', { name: 'Remove' })).toBeTruthy()
    expect(within(claude).queryByRole('button', { name: 'Install' })).toBeNull()

    const copilot = screen.getByTestId('skill-copilot')
    expect(within(copilot).getByTestId('skill-state-copilot').textContent).toBe('v0.1.24 → v0.1.25')
    expect(within(copilot).getByRole('button', { name: 'Update' })).toBeTruthy()

    const codex = screen.getByTestId('skill-codex')
    expect(within(codex).getByText('not found')).toBeTruthy()
    expect(within(codex).getByRole('button', { name: 'Install' })).toBeTruthy()
  })

  it('installs only when clicked, naming the agent (never a path)', async () => {
    const calls = mockFetch({
      '/trayapi/skills': () => [200, list(agent('claude', 'Claude Code'))],
      '/trayapi/skills/install': () => [
        200,
        { ...list(agent('claude', 'Claude Code', { state: 'current', installed_version: 'v0.1.25' })), message: 'Claude Code: installed v0.1.25' },
      ],
    })
    show()
    const row = await screen.findByTestId('skill-claude')
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0)
    fireEvent.click(within(row).getByRole('button', { name: 'Install' }))
    await waitFor(() => expect(within(row).getByTestId('skill-state-claude').textContent).toBe('installed v0.1.25'))
    const posts = calls.filter((c) => c.init?.method === 'POST')
    expect(posts.map((c) => [c.url, JSON.parse(String(c.init?.body))])).toEqual([['/trayapi/skills/install', { agent: 'claude' }]])
    expect(await screen.findByText('Claude Code: installed v0.1.25')).toBeTruthy()
  })

  it('offers nothing for a copy the user changed or a skill Longarm did not install, and shows a refusal', async () => {
    let state: AgentSkillStatus[] = [
      agent('claude', 'Claude Code', { state: 'modified', installed_version: 'v0.1.24', modified: ['SKILL.md'] }),
      agent('copilot', 'GitHub Copilot CLI', { state: 'foreign' }),
      agent('codex', 'OpenAI Codex CLI', { state: 'current', installed_version: 'v0.1.25' }),
    ]
    mockFetch({
      '/trayapi/skills': () => [200, list(...state)],
      '/trayapi/skills/remove': () => {
        state = [state[0], state[1], { ...state[2], state: 'modified', modified: ['reference/jobs.md'] }]
        return [409, { error: 'the skill was changed since Longarm installed it: reference/jobs.md' }]
      },
    })
    show()
    const claude = await screen.findByTestId('skill-claude')
    expect(within(claude).getByTestId('skill-state-claude').textContent).toBe('changed by you')
    expect(claude.textContent).toContain('SKILL.md changed since it was installed')
    expect(within(claude).queryAllByRole('button')).toHaveLength(0)
    const copilot = screen.getByTestId('skill-copilot')
    expect(within(copilot).getByTestId('skill-state-copilot').textContent).toBe('not Longarm’s')
    expect(within(copilot).queryAllByRole('button')).toHaveLength(0)

    fireEvent.click(within(screen.getByTestId('skill-codex')).getByRole('button', { name: 'Remove' }))
    expect(await screen.findByText(/changed since Longarm installed it: reference\/jobs.md/)).toBeTruthy()
    await waitFor(() => expect(screen.getByTestId('skill-state-codex').textContent).toBe('changed by you'))
  })
})
