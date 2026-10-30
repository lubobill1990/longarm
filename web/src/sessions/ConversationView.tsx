import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { sessionsApi, type AgentMessage, type AgentSession, type AgentState } from '../api/sessions'

/** Keep at most this many messages in the view (the oldest go). */
const MAX_MESSAGES = 5000

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
 * (the watch SSE). User and assistant bubbles; tool calls collapsed with
 * their result; scrolls along while at the bottom.
 */
export function ConversationView({ item, onState }: { item: AgentSession; onState?: (s: AgentState) => void }) {
  const [msgs, setMsgs] = useState<AgentMessage[]>([])
  const [truncated, setTruncated] = useState(false)
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(true)
  const box = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
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
    sessionsApi
      .messages(item)
      .then((r) => {
        if (gone) return
        add(r.messages)
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
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [msgs])

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
      }}
    >
      {truncated && <div className="conv-note muted small">earlier messages are not shown</div>}
      {error && <div className="error-box">{error}</div>}
      {loading && <div className="conv-note muted small">loading…</div>}
      {entries(msgs).map((e) => (e.kind === 'tool' ? <ToolCall key={e.call.seq} call={e.call} result={e.result} /> : <Bubble key={e.m.seq} m={e.m} />))}
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
  return (
    <div className={`bubble ${role} ${m.is_error ? 'is-error' : ''}`} data-testid={`msg-${role}`}>
      <div className="msg-text">{m.text}</div>
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
