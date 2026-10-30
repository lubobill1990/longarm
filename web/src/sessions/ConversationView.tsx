import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { sessionsApi, type AgentMessage, type AgentSession, type AgentState } from '../api/sessions'
import { Markdown } from './Markdown'

/** Keep at most this many messages in the view (the oldest go). */
const MAX_MESSAGES = 5000

/** Scrolled this close to the top: load earlier messages. */
const EARLIER_AT_PX = 80

/** A message's key: its line's offset and its place in the line (seq restarts with every answer). */
export const msgKey = (m: AgentMessage) => `${m.offset}.${m.seq}`

/** A tool call with its result (matched by the tool's id), or any other message. */
export type Entry = { kind: 'message'; m: AgentMessage } | ToolEntry
type ToolEntry = { kind: 'tool'; call: AgentMessage; result?: AgentMessage }

/** Pairs every tool call with its result; results without a call stay single. */
export function entries(msgs: AgentMessage[]): Entry[] {
  const out: Entry[] = []
  const calls = new Map<string, ToolEntry>()
  for (const m of msgs) {
    if (m.kind === 'tool_call') {
      const e: ToolEntry = { kind: 'tool', call: m }
      if (m.tool?.id) calls.set(m.tool.id, e)
      out.push(e)
      continue
    }
    const call = m.kind === 'tool_result' && m.tool?.id ? calls.get(m.tool.id) : undefined
    if (call && !call.result) call.result = m
    else out.push({ kind: 'message', m })
  }
  return out
}

/**
 * The parsed transcript of an agent session: the last part, then live
 * (the watch SSE); scrolling up to the top loads earlier parts. User and
 * assistant bubbles (Markdown); tool calls collapsed with their result;
 * scrolls along while at the bottom.
 */
export function ConversationView({ item, onState }: { item: AgentSession; onState?: (s: AgentState) => void }) {
  const [msgs, setMsgs] = useState<AgentMessage[]>([])
  const [truncated, setTruncated] = useState(false)
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(true)
  const [earlier, setEarlier] = useState(false)
  const box = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  // the offset of the oldest line shown: load earlier asks before it
  const start = useRef(0)
  // the scroll position to keep while earlier messages are put on top
  const anchor = useRef<{ height: number; top: number }>(undefined)
  const stateCb = useRef(onState)
  stateCb.current = onState
  const hasTranscript = !!item.agent && !!item.id

  useEffect(() => {
    if (!hasTranscript) return
    let stream: { close(): void } | undefined
    let gone = false
    const add = (more?: AgentMessage[]) => more?.length && setMsgs((old) => [...old, ...more].slice(-MAX_MESSAGES))
    setMsgs([])
    setLoading(true)
    setError(undefined)
    setTruncated(false)
    stick.current = true
    sessionsApi
      .messages(item)
      .then((r) => {
        if (gone) return
        add(r.messages)
        start.current = r.start ?? 0
        setTruncated(!!r.truncated)
        setLoading(false)
        if (r.state) stateCb.current?.(r.state)
        stream = sessionsApi.watchMessages(
          item,
          r.next_offset,
          (m) => add(m.messages),
          (s) => stateCb.current?.(s),
          (e) => e && setError(e),
        )
      })
      .catch((e) => {
        if (gone) return
        setLoading(false)
        setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      gone = true
      stream?.close()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [item.worker, item.agent, item.id])

  useLayoutEffect(() => {
    const el = box.current
    if (!el) return
    const a = anchor.current
    anchor.current = undefined
    if (a) el.scrollTop = el.scrollHeight - a.height + a.top // earlier messages went on top: stay put
    else if (stick.current) el.scrollTop = el.scrollHeight
  }, [msgs])

  const loadEarlier = useCallback(() => {
    if (earlier || !truncated || msgs.length >= MAX_MESSAGES) return
    setEarlier(true)
    sessionsApi
      .messages(item, { before: start.current })
      .then((r) => {
        const el = box.current
        if (el) anchor.current = { height: el.scrollHeight, top: el.scrollTop }
        start.current = r.start ?? 0
        setTruncated(!!r.truncated)
        if (r.messages?.length) setMsgs((old) => [...r.messages!, ...old])
        else anchor.current = undefined
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setEarlier(false))
  }, [earlier, truncated, msgs.length, item])

  if (!hasTranscript)
    return (
      <div className="conv conv-empty muted" data-testid="conversation">
        {item.agent ? 'No transcript yet: it appears with the first message.' : 'Not an agent session: no conversation to show.'}
      </div>
    )
  return (
    <div
      className="conv"
      ref={box}
      data-testid="conversation"
      onScroll={(e) => {
        const el = e.currentTarget
        stick.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 16
        if (el.scrollTop < EARLIER_AT_PX) loadEarlier()
      }}
    >
      {truncated && (
        <button type="button" className="btn small conv-earlier" data-testid="load-earlier" disabled={earlier} onClick={loadEarlier}>
          {earlier ? 'loading earlier messages…' : 'Load earlier messages'}
        </button>
      )}
      {error && <div className="error-box">{error}</div>}
      {loading && <div className="conv-note muted small">loading…</div>}
      {entries(msgs).map((e) =>
        e.kind === 'tool' ? <ToolCall key={msgKey(e.call)} call={e.call} result={e.result} /> : <Bubble key={msgKey(e.m)} m={e.m} />,
      )}
    </div>
  )
}

function Bubble({ m }: { m: AgentMessage }) {
  if (m.kind === 'thinking')
    return (
      <details className="msg-thinking">
        <summary className="muted small">thinking</summary>
        <div className="msg-text">{m.text}</div>
      </details>
    )
  if (m.kind === 'summary')
    return (
      <div className="msg-summary small" data-testid="msg-summary">
        <span className="muted">conversation compacted:</span> {m.text}
      </div>
    )
  const role = m.role === 'user' ? 'user' : m.role === 'tool' ? 'tool' : 'assistant'
  // what people and agents write is Markdown; tool output and errors are shown as they are
  const md = (role === 'user' || role === 'assistant') && m.kind === 'text' && !m.is_error
  return (
    <div className={`bubble ${role} ${m.is_error ? 'is-error' : ''}`} data-testid={`msg-${role}`}>
      {md ? <Markdown text={m.text} /> : <div className="msg-text">{m.text}</div>}
    </div>
  )
}

function ToolCall({ call, result }: { call: AgentMessage; result?: AgentMessage }) {
  const err = !!result?.is_error
  return (
    <details className={`toolcall ${err ? 'is-error' : ''}`} data-testid="tool-call">
      <summary>
        <span className="tool-name">{call.tool?.name ?? 'tool'}</span> <span className="tool-arg mono">{call.text}</span>
        {!result && <span className="muted small"> · running…</span>}
        {err && <span className="red small"> · error</span>}
      </summary>
      {result && <pre className="tool-result mono">{result.text}</pre>}
    </details>
  )
}
