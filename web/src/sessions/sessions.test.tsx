import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { attachUrl, sessionsApi, type AgentMessage, type AgentSession } from '../api/sessions'
import { WorkerCard } from '../components/WorkerCard'
import { Toasts } from '../components/common'
import type { PoolWorker } from '../api/types'
import { PoolTab } from '../tray/PoolTab'
import { ConversationView, entries } from './ConversationView'
import { groupByWorker, matches, SessionList, stateMark } from './SessionList'
import { connectedTabs, continueBlocked, parseHash, sameSession, SessionsApp, terminalOf } from './SessionsApp'
import { shortcutTarget } from './SessionTabs'
import { SteerBox } from './SteerBox'
import { BACKOFF, TerminalView } from './TerminalView'

// jsdom has no canvas: a fake terminal records what xterm.js would get
const fake = vi.hoisted(() => {
  class FakeTerm {
    written: (Uint8Array | string)[] = []
    resets = 0
    input?: (b: Uint8Array) => void
    disposed = false
    cols = 80
    rows = 24
    write(d: Uint8Array | string) {
      this.written.push(d)
    }
    reset() {
      this.resets++
    }
    onInput(fn: (b: Uint8Array) => void) {
      this.input = fn
    }
    fit() {
      this.cols = 120
      this.rows = 36
      return { cols: this.cols, rows: this.rows }
    }
    size() {
      return { cols: this.cols, rows: this.rows }
    }
    focus() {}
    dispose() {
      this.disposed = true
    }
  }
  return { FakeTerm, terms: [] as InstanceType<typeof FakeTerm>[] }
})

vi.mock('./terminal', () => ({
  createTerminal: () => {
    const t = new fake.FakeTerm()
    fake.terms.push(t)
    return t
  },
}))

class FakeWebSocket {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 3
  static all: FakeWebSocket[] = []
  url: string
  readyState = 0
  binaryType = 'blob'
  sent: (string | Uint8Array)[] = []
  closedWith?: number
  onmessage?: (ev: { data: unknown }) => void
  onclose?: (ev: { code: number }) => void
  constructor(url: string) {
    this.url = url
    FakeWebSocket.all.push(this)
  }
  send(d: string | Uint8Array) {
    this.sent.push(d)
  }
  close(code = 1005) {
    this.closedWith = code
    this.readyState = 3
  }
  // the server's side
  open() {
    this.readyState = 1
  }
  text(v: unknown) {
    this.onmessage?.({ data: JSON.stringify(v) })
  }
  binary(s: string) {
    this.onmessage?.({ data: new TextEncoder().encode(s).buffer })
  }
  serverClose(code: number) {
    this.readyState = 3
    this.onclose?.({ code })
  }
}

class FakeEventSource {
  static all: FakeEventSource[] = []
  url: string
  listeners: Record<string, ((ev: { data: string }) => void)[]> = {}
  closed = false
  constructor(url: string) {
    this.url = url
    FakeEventSource.all.push(this)
  }
  addEventListener(name: string, fn: (ev: { data: string }) => void) {
    ;(this.listeners[name] ??= []).push(fn)
  }
  close() {
    this.closed = true
  }
  emit(name: string, data: unknown) {
    for (const fn of this.listeners[name] ?? []) fn({ data: JSON.stringify(data) })
  }
}

type Route = (init?: RequestInit, url?: string) => [number, unknown]

function mockFetch(routes: Record<string, Route>) {
  const calls: { url: string; init?: RequestInit }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      const key = Object.keys(routes)
        .filter((k) => url.startsWith(k))
        .sort((a, b) => b.length - a.length)[0]
      const [status, body] = key ? routes[key](init, url) : [404, { error: 'not found' }]
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }),
  )
  return calls
}

const body = (c: { init?: RequestInit }) => JSON.parse(String(c.init?.body))

function item(over: Partial<AgentSession>): AgentSession {
  return {
    key: 'cpc-a/claude/abc',
    worker: 'cpc-a',
    agent: 'claude',
    id: 'abc',
    title: 'fix the flaky relay test',
    project_dir: 'C:\\src\\longarm',
    where: 'longarm',
    session: 'agent1',
    state: 'waiting',
    last_activity: new Date(Date.now() - 120_000).toISOString(),
    ...over,
  }
}

beforeEach(() => {
  FakeWebSocket.all = []
  FakeEventSource.all = []
  fake.terms.length = 0
  vi.stubGlobal('WebSocket', FakeWebSocket)
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  history.replaceState(null, '', '/')
})

describe('sessions API', () => {
  it('builds the list query and the terminal URL', async () => {
    const calls = mockFetch({ '/local/longarm/v1/agent-sessions': () => [200, { items: [] }] })
    await sessionsApi.list({ worker: 'cpc-a', agent: 'claude', all: true })
    expect(calls[0].url).toBe('/local/longarm/v1/agent-sessions?worker=cpc-a&agent=claude&all=1')
    expect(attachUrl('cpc a', 'agent1', 120, 36)).toBe(`ws://${location.host}/local/longarm/v1/workers/cpc%20a/sessions/agent1/attach?cols=120&rows=36`)
  })
})

describe('SessionList', () => {
  const items = [
    item({ key: 'cpc-b/claude/1', worker: 'cpc-b', id: '1', title: 'b work', state: 'working' }),
    item({ key: 'cpc-a/claude/2', id: '2', title: 'waiting one', state: 'waiting' }),
    item({ key: 'cpc-a/copilot/3', agent: 'copilot', id: '3', title: 'on the desktop', where: 'desktop', session: undefined, state: 'working' }),
    item({ key: 'cpc-a/claude/4', id: '4', title: 'old one', where: 'none', session: undefined, state: 'ended' }),
  ]

  it('groups by worker and shows the states', () => {
    expect(groupByWorker(items).map(([w, l]) => [w, l.length])).toEqual([
      ['cpc-a', 3],
      ['cpc-b', 1],
    ])
    const onOpen = vi.fn()
    render(<SessionList items={items} unreachable={['cpc-c']} filter="" onFilter={() => {}} onOpen={onOpen} />)
    const a = screen.getByTestId('group-cpc-a')
    expect(within(a).getAllByRole('button')).toHaveLength(3)
    expect(screen.getByTestId('session-cpc-b/claude/1').dataset.state).toBe('working')
    expect(screen.getByTestId('session-cpc-a/claude/2').dataset.state).toBe('waiting')
    expect(screen.getByTestId('session-cpc-a/copilot/3').dataset.state).toBe('desktop')
    expect(screen.getByTestId('session-cpc-a/copilot/3').textContent).toContain('◇')
    expect(screen.getByTestId('session-cpc-a/claude/4').textContent).toContain('○')
    expect(screen.getByTestId('session-cpc-a/claude/2').textContent).toContain('2m')
    expect(screen.getByTestId('unreachable').textContent).toContain('cpc-c')
    fireEvent.click(screen.getByText('waiting one'))
    expect(onOpen).toHaveBeenCalledWith(items[1])
  })

  it('filters by title, worker and agent', () => {
    expect(items.filter((s) => matches(s, 'copilot')).map((s) => s.id)).toEqual(['3'])
    expect(items.filter((s) => matches(s, 'CPC-B')).map((s) => s.id)).toEqual(['1'])
    render(<SessionList items={items} filter="nothing like it" onFilter={() => {}} onOpen={() => {}} />)
    expect(screen.getByText('Nothing matches the filter.')).toBeTruthy()
  })
})

describe('tabs', () => {
  it('maps the shortcuts to tabs', () => {
    const ids = ['a', 'b', 'c']
    expect(shortcutTarget({ ctrlKey: true, shiftKey: false, key: 'Tab' }, ids, 'c')).toBe('a')
    expect(shortcutTarget({ ctrlKey: true, shiftKey: true, key: 'Tab' }, ids, 'a')).toBe('c')
    expect(shortcutTarget({ ctrlKey: true, shiftKey: false, key: '2' }, ids, 'a')).toBe('b')
    expect(shortcutTarget({ ctrlKey: true, shiftKey: false, key: '9' }, ids, 'a')).toBe('c')
    expect(shortcutTarget({ ctrlKey: true, shiftKey: false, key: '5' }, ids, 'a')).toBeUndefined()
    expect(shortcutTarget({ ctrlKey: false, shiftKey: false, key: '1' }, ids, 'a')).toBeUndefined()
  })

  it('keeps the active and the 5 most recently shown tabs connected', () => {
    const recent = ['g', 'f', 'e', 'd', 'c', 'b', 'a']
    expect([...connectedTabs(recent, 'a')]).toEqual(['a', 'g', 'f', 'e', 'd', 'c'])
  })

  it('reads the hash', () => {
    expect(parseHash('#worker=cpc-a&agent=claude&key=cpc-a%2Fclaude%2Fabc')).toEqual({ worker: 'cpc-a', agent: 'claude', key: 'cpc-a/claude/abc' })
    expect(parseHash('#agent=vim')).toEqual({ worker: undefined, agent: undefined, key: undefined })
  })
})

describe('SessionsApp', () => {
  const a = item({})
  const b = item({ key: 'cpc-a/claude/def', id: 'def', title: 'second', session: 'agent2', state: 'working' })
  const desk = item({ key: 'cpc-a/copilot/xyz', agent: 'copilot', id: 'xyz', title: 'desk', where: 'desktop', session: undefined, state: 'working' })

  it('opens the session of the hash filtered to its worker; tabs switch with Ctrl+1..9 and close to detach', async () => {
    location.hash = '#worker=cpc-a&agent=claude&key=cpc-a/claude/abc'
    const calls = mockFetch({ '/local/longarm/v1/agent-sessions': () => [200, { items: [a, b, desk] }] })
    render(<SessionsApp />)
    await screen.findByTestId('tab-cpc-a/claude/abc')
    expect(calls.find((c) => c.url.startsWith('/local/longarm/v1/agent-sessions'))!.url).toBe('/local/longarm/v1/agent-sessions?worker=cpc-a&agent=claude')
    expect(screen.getByTestId('scope-worker').textContent).toContain('cpc-a')
    // a Longarm session: its terminal over the WebSocket, and the steer box
    await waitFor(() => expect(FakeWebSocket.all).toHaveLength(1))
    expect(FakeWebSocket.all[0].url).toContain('/local/longarm/v1/workers/cpc-a/sessions/agent1/attach?')
    expect(within(screen.getByTestId('panel-cpc-a/claude/abc')).getByTestId('steer')).toBeTruthy()

    fireEvent.click(screen.getByText('second'))
    await screen.findByTestId('tab-cpc-a/claude/def')
    expect(screen.getByTestId('panel-cpc-a/claude/def').hidden).toBe(false)
    expect(screen.getByTestId('panel-cpc-a/claude/abc').hidden).toBe(true)
    fireEvent.keyDown(window, { key: '1', ctrlKey: true })
    expect(screen.getByTestId('panel-cpc-a/claude/abc').hidden).toBe(false)

    // output in the background tab: a badge, gone when shown
    const ws2 = FakeWebSocket.all[1]
    act(() => {
      ws2.open()
      ws2.text({ type: 'started', session: { name: 'agent2' } })
      ws2.binary('hello')
    })
    expect(within(screen.getByTestId('tab-cpc-a/claude/def')).getByTestId('tab-badge')).toBeTruthy()
    fireEvent.keyDown(window, { key: 'Tab', ctrlKey: true })
    expect(within(screen.getByTestId('tab-cpc-a/claude/def')).queryByTestId('tab-badge')).toBeNull()

    // closing detaches
    fireEvent.click(screen.getByLabelText('close second'))
    expect(screen.queryByTestId('tab-cpc-a/claude/def')).toBeNull()
    expect(ws2.closedWith).toBe(1000)
    expect(ws2.sent).toContain(JSON.stringify({ type: 'detach' }))
  })

  it('shows an agent outside Longarm read-only, with Continue here (a fork of the live session)', async () => {
    const deskClaude = { ...desk, key: 'cpc-a/claude/xyz', agent: 'claude' as const }
    location.hash = '#key=cpc-a/claude/xyz'
    const calls = mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/claude/xyz/messages': () => [200, { messages: [], next_offset: 0, size: 0 }],
      '/local/longarm/v1/agent-sessions': () => [200, { items: [a, deskClaude] }],
      '/local/longarm/v1/workers/cpc-a/sessions': () => [201, { name: 'claude-1a2b', dir: 'C:\\src' }],
    })
    render(<SessionsApp />)
    const panel = await screen.findByTestId('panel-cpc-a/claude/xyz')
    expect(within(panel).queryByTestId('steer')).toBeNull()
    expect(within(panel).queryByTestId('terminal')).toBeNull()
    await within(panel).findByTestId('conversation')
    fireEvent.click(within(panel).getByTestId('continue-here'))
    await screen.findByTestId('tab-cpc-a/session/claude-1a2b')
    const start = calls.find((c) => c.url === '/local/longarm/v1/workers/cpc-a/sessions')!
    expect(body(start)).toEqual({ agent: 'claude', resume: 'xyz', fork: true, cwd: 'C:\\src\\longarm' })
  })

  it('lists the last days first and loads older sessions page by page', async () => {
    location.hash = ''
    const old = item({ key: 'cpc-a/claude/old', id: 'old', title: 'an old one', where: 'none', session: undefined, state: 'ended' })
    const calls = mockFetch({
      '/local/longarm/v1/agent-sessions?until=': () => [200, { items: [old] }],
      '/local/longarm/v1/agent-sessions': () => [200, { items: [a], older: '2026-10-20T08:00:00.123456789Z' }],
    })
    render(<SessionsApp />)
    fireEvent.click(await screen.findByTestId('load-older'))
    await screen.findByTestId('session-cpc-a/claude/old')
    const page = calls.find((c) => c.url.includes('until='))!
    expect(new URL(page.url, 'http://x').searchParams.get('until')).toBe('2026-10-20T08:00:00.123456789Z')
    expect(screen.getByTestId(`session-${a.key}`)).toBeTruthy() // the first page stays
    expect(screen.queryByTestId('load-older')).toBeNull() // no older than that
  })

  it("a live Copilot session outside Longarm can't be copied: Continue here waits for its end", async () => {
    location.hash = '#key=cpc-a/copilot/xyz'
    mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/copilot/xyz/messages': () => [200, { messages: [], next_offset: 0, size: 0 }],
      '/local/longarm/v1/agent-sessions': () => [200, { items: [a, desk] }],
    })
    render(<SessionsApp />)
    const panel = await screen.findByTestId('panel-cpc-a/copilot/xyz')
    await within(panel).findByTestId('conversation') // its content is shown, read-only
    const cont = within(panel).getByTestId('continue-here') as HTMLButtonElement
    expect(cont.disabled).toBe(true)
    expect(cont.title).toContain('once it has ended')
  })
})

describe('TerminalView', () => {
  it('writes the output, sends the keys and the size, shows the end', () => {
    const onExit = vi.fn()
    render(<TerminalView worker="cpc-a" session="agent1" visible={false} onExit={onExit} />)
    const ws = FakeWebSocket.all[0]
    const t = fake.terms[0]
    expect(ws.url).toContain('attach?cols=80&rows=24')
    expect(ws.binaryType).toBe('arraybuffer')
    act(() => {
      ws.open()
      ws.text({ type: 'started', session: { name: 'agent1' } })
      ws.binary('$ ')
    })
    expect(t.resets).toBe(1)
    expect(new TextDecoder().decode(t.written[0] as Uint8Array)).toBe('$ ')
    t.input!(new TextEncoder().encode('ls\r'))
    expect(new TextDecoder().decode(ws.sent.find((x) => typeof x !== 'string') as Uint8Array)).toBe('ls\r')
    expect(ws.sent).toContain(JSON.stringify({ type: 'resize', cols: 80, rows: 24 }))
    act(() => {
      ws.text({ type: 'exit', code: 3 })
      ws.serverClose(1000)
    })
    expect(screen.getByTestId('term-ended').textContent).toBe('session ended (code 3)')
    expect(onExit).toHaveBeenCalledWith(3)
    expect(FakeWebSocket.all).toHaveLength(1)
  })

  it('re-attaches with backoff when the connection is lost', () => {
    vi.useFakeTimers()
    render(<TerminalView worker="cpc-a" session="agent1" visible={false} />)
    act(() => FakeWebSocket.all[0].serverClose(1006))
    expect(screen.getByTestId('term-retry').textContent).toContain('connection lost')
    act(() => vi.advanceTimersByTime(BACKOFF[0] - 1))
    expect(FakeWebSocket.all).toHaveLength(1)
    act(() => vi.advanceTimersByTime(1))
    expect(FakeWebSocket.all).toHaveLength(2)
    act(() => FakeWebSocket.all[1].serverClose(1008))
    act(() => vi.advanceTimersByTime(BACKOFF[1]))
    expect(FakeWebSocket.all).toHaveLength(3)
  })

  it('fits when shown and tells the session', () => {
    // jsdom has no layout: give the element a width
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800)
    render(<TerminalView worker="cpc-a" session="agent1" visible />)
    const ws = FakeWebSocket.all[0]
    act(() => {
      ws.open()
      ws.text({ type: 'started', session: { name: 'agent1' } })
    })
    expect(ws.sent).toContain(JSON.stringify({ type: 'resize', cols: 120, rows: 36 }))
  })
})

describe('SteerBox', () => {
  it('sends text + Enter, keeps the text on 409 (someone is typing)', async () => {
    let status = 409
    const calls = mockFetch({
      '/local/longarm/v1/workers/cpc-a/sessions/agent1/send': () => (status === 409 ? [409, { error: 'not idle' }] : [200, { input_seq: 4 }]),
    })
    render(<SteerBox worker="cpc-a" session="agent1" />)
    const box = screen.getByLabelText('message to the session') as HTMLTextAreaElement
    fireEvent.change(box, { target: { value: 'fix it\nand test' } })
    fireEvent.keyDown(box, { key: 'Enter', shiftKey: true })
    expect(calls).toHaveLength(0)
    fireEvent.keyDown(box, { key: 'Enter' })
    expect(await screen.findByText('someone is typing - retry')).toBeTruthy()
    expect(box.value).toBe('fix it\nand test')
    expect(body(calls[0])).toEqual({ text: 'fix it\nand test', keys: ['enter'], raw: true, if_idle_ms: 1500 })

    status = 200
    fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(box.value).toBe(''))
    expect(screen.queryByTestId('steer-note')).toBeNull()
  })

  it('sends Esc, y and a confirmed Ctrl-C', async () => {
    const calls = mockFetch({ '/local/longarm/v1/workers/cpc-a/sessions/agent1/send': () => [200, { input_seq: 1 }] })
    const ok = vi.fn(() => false)
    vi.stubGlobal('confirm', ok)
    render(<SteerBox worker="cpc-a" session="agent1" />)
    fireEvent.click(screen.getByText('Esc'))
    await waitFor(() => expect(calls).toHaveLength(1))
    fireEvent.click(screen.getByText('y'))
    await waitFor(() => expect(calls).toHaveLength(2))
    fireEvent.click(screen.getByText('Ctrl-C'))
    expect(ok).toHaveBeenCalled()
    ok.mockReturnValue(true)
    fireEvent.click(screen.getByText('Ctrl-C'))
    await waitFor(() => expect(calls).toHaveLength(3))
    expect(calls.map(body)).toEqual([
      { keys: ['esc'], if_idle_ms: 1500 },
      { text: 'y', raw: true, if_idle_ms: 1500 },
      { keys: ['ctrl-c'], if_idle_ms: 1500 },
    ])
  })
})

describe('ConversationView', () => {
  const msg = (seq: number, over: Partial<AgentMessage>): AgentMessage => ({ seq, offset: seq * 100, role: 'assistant', kind: 'text', text: '', ...over })
  const first = [
    msg(0, { role: 'user', text: 'fix the flaky relay test' }),
    msg(1, { kind: 'tool_call', text: 'go test ./internal/relay', tool: { id: 't1', name: 'Bash' } }),
    msg(2, { role: 'tool', kind: 'tool_result', text: 'FAIL TestUDP', tool: { id: 't1', name: 'Bash' }, is_error: true }),
  ]

  it('pairs tool calls with their results', () => {
    const e = entries(first)
    expect(e.map((x) => x.kind)).toEqual(['message', 'tool'])
    expect(e[1].kind === 'tool' && e[1].result?.text).toBe('FAIL TestUDP')
  })

  it('renders the transcript and follows it live', async () => {
    const calls = mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages': () => [200, { messages: first, next_offset: 300, size: 300, truncated: true, state: 'working' }],
    })
    const onState = vi.fn()
    render(<ConversationView item={item({})} onState={onState} />)
    expect((await screen.findByTestId('msg-user')).textContent).toBe('fix the flaky relay test')
    expect(calls[0].url).toBe('/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages')
    expect(screen.getByTestId('load-earlier')).toBeTruthy()
    const tool = screen.getByTestId('tool-call')
    expect(tool.tagName).toBe('DETAILS')
    expect((tool as HTMLDetailsElement).open).toBe(false)
    expect(tool.textContent).toContain('Bash')
    expect(tool.textContent).toContain('go test ./internal/relay')
    expect(tool.textContent).toContain('FAIL TestUDP')
    expect(tool.className).toContain('is-error')
    expect(onState).toHaveBeenCalledWith('working')

    const es = FakeEventSource.all[0]
    expect(es.url).toBe('/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages/watch?offset=300')
    act(() => {
      es.emit('messages', { messages: [msg(3, { text: 'Fixed: the test waited on a closed port.' })], next_offset: 400, size: 400 })
      es.emit('state', { state: 'waiting' })
    })
    expect(screen.getAllByTestId('msg-assistant').map((x) => x.textContent)).toEqual(['Fixed: the test waited on a closed port.'])
    expect(onState).toHaveBeenLastCalledWith('waiting')
  })

  it('loads earlier messages before the first one shown, until the beginning', async () => {
    const at = (offset: number, seq: number, text: string) => msg(seq, { offset, role: 'user', text })
    const calls = mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages?before=1000': () => [
        200,
        { messages: [at(0, 0, 'first prompt'), at(500, 1, 'second prompt')], start: 0, next_offset: 1000, size: 2000, truncated: false },
      ],
      '/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages': () => [
        200,
        { messages: [at(1000, 0, 'third prompt')], start: 1000, next_offset: 2000, size: 2000, truncated: true },
      ],
    })
    render(<ConversationView item={item({})} />)
    await screen.findByText('third prompt')
    fireEvent.click(screen.getByTestId('load-earlier'))
    await screen.findByText('first prompt')
    expect(calls.some((c) => c.url.endsWith('/messages?before=1000'))).toBe(true)
    // in order, the earlier part on top; the beginning reached: no more button
    expect(screen.getAllByTestId('msg-user').map((x) => x.textContent)).toEqual(['first prompt', 'second prompt', 'third prompt'])
    expect(screen.queryByTestId('load-earlier')).toBeNull()
  })

  it('renders what the agent writes as Markdown, and tool output as it is', async () => {
    mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/claude/abc/messages': () => [
        200,
        {
          messages: [
            msg(0, { text: '**Fixed** the `relay` test:\n\n```go\nfunc f() {}\n```\n\n- one\n- two\n\nsee [docs](https://example.com) <b>raw</b>' }),
          ],
          start: 0,
          next_offset: 100,
          size: 100,
        },
      ],
    })
    render(<ConversationView item={item({})} />)
    const bubble = await screen.findByTestId('msg-assistant')
    expect(bubble.querySelector('strong')?.textContent).toBe('Fixed')
    expect(bubble.querySelector('code')?.textContent).toBe('relay')
    expect(bubble.querySelector('pre')?.textContent).toBe('func f() {}')
    expect([...bubble.querySelectorAll('li')].map((l) => l.textContent)).toEqual(['one', 'two'])
    expect(bubble.querySelector('a')?.getAttribute('href')).toBe('https://example.com')
    expect(bubble.querySelector('b')).toBeNull() // markup in the text stays text
    expect(bubble.textContent).toContain('<b>raw</b>')
  })

  it('says when there is no transcript yet', () => {
    render(<ConversationView item={item({ id: '' })} />)
    expect(screen.getByTestId('conversation').textContent).toContain('No transcript yet')
  })
})

describe('agent counters on the worker card', () => {
  const GiB = 1024 ** 3
  const w: PoolWorker = {
    name: 'cpc-a',
    state: 'online',
    source: 'hub',
    metrics: { cpu_percent: 5, mem_total_bytes: String(32 * GiB), mem_used_bytes: String(8 * GiB), agents: { claude: { sessions: 2, processes: 2 } } },
    node: { name: 'cpc-a', role: 'worker', state: 'approved', online: true, owner: 'alice' },
  }

  it('are buttons with a sessions action, plain counters without', () => {
    const sessions = vi.fn()
    const { unmount } = render(<WorkerCard w={w} actions={{ sessions }} />)
    fireEvent.click(screen.getByTestId('agent-copilot'))
    expect(sessions).toHaveBeenCalledWith('cpc-a', 'copilot')
    expect(screen.getByTestId('agent-claude').tagName).toBe('BUTTON')
    unmount()
    render(<WorkerCard w={w} />)
    expect(screen.getByTestId('agent-claude').tagName).toBe('DIV')
  })

  it('open the Sessions window from the Pool tab', async () => {
    const calls = mockFetch({ '/trayapi/sessions/open': () => [200, {}] })
    render(
      <Toasts>
        <PoolTab pool={{ source: 'hub', transport: 'race', workers: [w] }} />
      </Toasts>,
    )
    fireEvent.click(screen.getByTestId('agent-claude'))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].init?.method).toBe('POST')
    expect(body(calls[0])).toEqual({ worker: 'cpc-a', agent: 'claude' })
  })
})

describe('agents in Longarm jobs', () => {
  const job = item({ key: 'cpc-a/copilot/j1', agent: 'copilot', id: 'j1', title: 'review W29', where: 'job', session: undefined, job: '01M66YJJ2AHVSSW2DBRQNSDWSF', job_name: 'review-W29', state: 'working' })

  it('are marked as jobs in the list and found by the job name', () => {
    expect(stateMark(job)).toMatchObject({ mark: '▸', cls: 'job' })
    expect(stateMark({ ...job, state: 'ended' }).cls).toBe('ended')
    expect(matches(job, 'W29')).toBe(true)
    render(<SessionList items={[job]} filter="" onFilter={() => {}} onOpen={() => {}} />)
    expect(screen.getByTestId('session-cpc-a/copilot/j1').title).toContain('Longarm job review-W29')
  })

  it('open read-only: the conversation, no steer, no Continue here while the job runs', async () => {
    location.hash = '#key=cpc-a/copilot/j1'
    mockFetch({
      '/local/longarm/v1/agent-sessions/cpc-a/copilot/j1/messages': () => [200, { messages: [], next_offset: 0, size: 0 }],
      '/local/longarm/v1/agent-sessions': () => [200, { items: [job] }],
    })
    render(<SessionsApp />)
    const panel = await screen.findByTestId('panel-cpc-a/copilot/j1')
    await within(panel).findByTestId('conversation')
    expect(within(panel).queryByTestId('steer')).toBeNull()
    expect(within(panel).queryByTestId('terminal')).toBeNull()
    expect(panel.textContent).toContain('Longarm job review-W29 (read-only)')
    expect((within(panel).getByTestId('continue-here') as HTMLButtonElement).disabled).toBe(true)
  })

  it('Continue here: why not, per case', () => {
    expect(continueBlocked(job)).toContain('once it has ended')
    expect(continueBlocked({ ...job, state: 'ended' })).toContain('private Copilot home')
    expect(continueBlocked({ ...job, agent: 'claude', state: 'ended' })).toBe('') // a Claude job's transcript resumes
    expect(continueBlocked({ ...job, where: 'desktop' })).toContain("can't copy")
    expect(continueBlocked({ ...job, where: 'none', state: 'ended' })).toBe('')
  })
})

describe('steerable jobs', () => {
  const job = item({
    key: 'cpc-a/job/01JOB',
    id: '',
    title: 'claude fix it',
    where: 'job',
    session: undefined,
    job: '01JOB',
    job_name: 'fix',
    steer: 'needs_you',
    state: 'waiting',
  })

  it('marks one that needs you; its terminal and the steer box reach the job', async () => {
    location.hash = `#key=${job.key}`
    const calls = mockFetch({
      '/local/longarm/v1/workers/cpc-a/jobs/01JOB/send': () => [200, { input_seq: 1 }],
      '/local/longarm/v1/agent-sessions': () => [200, { items: [job] }],
    })
    render(<SessionsApp />)
    expect(stateMark(job)).toMatchObject({ mark: '!', cls: 'needs' })
    const panel = await screen.findByTestId(`panel-${job.key}`)
    expect(within(panel).getByText(/steerable: needs you/)).toBeTruthy()
    await waitFor(() => expect(FakeWebSocket.all.some((w) => w.url.includes('/v1/workers/cpc-a/jobs/01JOB/attach?'))).toBe(true))
    const box = within(panel).getByLabelText('message to the session') as HTMLTextAreaElement
    fireEvent.change(box, { target: { value: 'y' } })
    fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(calls.some((c) => c.url.endsWith('/workers/cpc-a/jobs/01JOB/send'))).toBe(true))
    expect(within(panel).queryByTestId('continue-here')).toBeNull()
  })

  it('stays one tab when its transcript appears; read-only once it ended', () => {
    const later = { ...job, key: 'cpc-a/claude/abc', id: 'abc', steer: 'working' as const, state: 'working' as const }
    expect(sameSession(job, later)).toBe(true)
    expect(sameSession(job, { ...later, job: '01OTHER' })).toBe(false)
    expect(terminalOf(job)).toEqual({ name: '01JOB', job: true })
    expect(terminalOf({ ...later, steer: undefined, state: 'ended' })).toBeUndefined()
    expect(terminalOf(item({}))).toEqual({ name: 'agent1', job: false })
    expect(continueBlocked(later)).toContain('steer it here')
  })
})
