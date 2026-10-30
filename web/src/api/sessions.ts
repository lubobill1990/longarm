// The Sessions window's API: the agent sessions of the user's workers
// (longarm daemon's /v1/agent-sessions, through the tray's /local/longarm/
// proxy), the Longarm sessions they run in (start / send / stop) and their
// terminals (a WebSocket per attach). See docs/agent-sessions-ui.md, section 3.
import { get, post, sse, type Stream } from './http'

const enc = encodeURIComponent
const BASE = '/local/longarm/v1'

export type Agent = 'claude' | 'copilot'
/** working | waiting (its turn is over) | running (not a known agent) | ended */
export type AgentState = 'working' | 'waiting' | 'running' | 'ended'
/** longarm: in Longarm session `session`; desktop: a live process outside Longarm; none: a transcript only */
export type Where = 'longarm' | 'desktop' | 'none'

/** One row of GET /v1/agent-sessions. */
export interface AgentSession {
  /** <worker>/<agent>/<id>, or <worker>/session/<name> without a known agent */
  key: string
  worker: string
  agent: Agent | ''
  /** the transcript's id; "" for a Longarm session without one (yet) */
  id: string
  title: string
  project_dir?: string
  modified?: string
  size?: number
  where: Where
  /** the Longarm session's name (where: longarm) */
  session?: string
  state: AgentState
  last_activity?: string
  pid?: number
}

export interface AgentSessionList {
  items?: AgentSession[]
  /** workers that did not answer */
  unreachable?: string[]
}

export interface ToolRef {
  id: string
  name: string
}

/** One parsed transcript entry (internal/agentlog.Message). */
export interface AgentMessage {
  seq: number
  offset: number
  time?: string
  /** user | assistant | tool */
  role: string
  /** text | tool_call | tool_result | thinking | summary */
  kind: string
  text: string
  tool?: ToolRef
  is_error?: boolean
}

/** GET .../messages and the watch's "messages" events (new messages only). */
export interface AgentMessages {
  messages?: AgentMessage[]
  next_offset: number
  size: number
  /** older content exists before the first message */
  truncated?: boolean
  state?: AgentState
}

/** A Longarm session (pb.Session as protojson). */
export interface LongarmSession {
  name: string
  state?: string
  argv?: string[]
  cwd?: string
  dir?: string
  pid?: number
  created_at?: string
  started_at?: string
  ended_at?: string
  has_exit_code?: boolean
  exit_code?: number
  attached?: number
  input_seq?: string | number
  owner?: string
}

/** POST /v1/workers/{worker}/sessions */
export interface StartSession {
  /** "" = generated (<agent>-<4 hex>) */
  name?: string
  agent?: Agent
  cwd?: string
  /** an agent session id to resume */
  resume?: string
  /** resume as a copy (the source is still open) */
  fork?: boolean
  /** instead of agent: run anything */
  argv?: string[]
  cols?: number
  rows?: number
}

/** POST /v1/workers/{worker}/sessions/{name}/send (409 "not idle" when if_idle_ms refuses) */
export interface SendInput {
  text?: string
  /** enter, esc, ctrl-c ... (longarm session send --key) */
  keys?: string[]
  if_idle_ms?: number
  /** text as is (no newline → Enter) */
  raw?: boolean
}

export interface ListQuery {
  worker?: string
  agent?: Agent
  /** also ended Longarm sessions and handed-off transcripts */
  all?: boolean
}

const messagesPath = (s: Pick<AgentSession, 'worker' | 'agent' | 'id'>) =>
  `${BASE}/agent-sessions/${enc(s.worker)}/${enc(s.agent)}/${enc(s.id)}/messages`

const sessionPath = (worker: string, name: string) => `${BASE}/workers/${enc(worker)}/sessions/${enc(name)}`

export const sessionsApi = {
  list: (q: ListQuery = {}) => {
    const p = new URLSearchParams()
    if (q.worker) p.set('worker', q.worker)
    if (q.agent) p.set('agent', q.agent)
    if (q.all) p.set('all', '1')
    const qs = p.toString()
    return get<AgentSessionList>(`${BASE}/agent-sessions${qs ? `?${qs}` : ''}`)
  },
  /** the transcript's last part (offset undefined) or from offset */
  messages: (s: Pick<AgentSession, 'worker' | 'agent' | 'id'>, offset?: number) =>
    get<AgentMessages>(`${messagesPath(s)}${offset !== undefined ? `?offset=${offset}` : ''}`),
  /** new messages from offset as the transcript grows; state changes */
  watchMessages: (
    s: Pick<AgentSession, 'worker' | 'agent' | 'id'>,
    offset: number,
    onMessages: (m: AgentMessages) => void,
    onState: (state: AgentState) => void,
    onDone?: (err?: string) => void,
  ): Stream =>
    sse(
      `${messagesPath(s)}/watch?offset=${offset}`,
      { messages: (d) => onMessages(d as AgentMessages), state: (d) => onState((d as { state: AgentState }).state) },
      onDone,
    ),
  start: (worker: string, req: StartSession) => post<LongarmSession>(`${BASE}/workers/${enc(worker)}/sessions`, req),
  get: (worker: string, name: string) => get<LongarmSession>(sessionPath(worker, name)),
  send: (worker: string, name: string, input: SendInput) => post<{ input_seq: number }>(`${sessionPath(worker, name)}/send`, input),
  stop: (worker: string, name: string, graceMs = 10000) => post<LongarmSession>(`${sessionPath(worker, name)}/stop`, { grace_ms: graceMs }),
}

/** The terminal's WebSocket URL (same origin: the tray checks it). */
export function attachUrl(worker: string, name: string, cols: number, rows: number): string {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${scheme}://${location.host}${sessionPath(worker, name)}/attach?cols=${cols}&rows=${rows}`
}

/** The JSON control frames the server sends (text frames). */
export type AttachControl =
  | { type: 'started'; session: LongarmSession }
  | { type: 'exit'; code: number }
  | { type: 'error'; message: string }

export interface AttachHandlers {
  /** terminal output bytes */
  onData: (b: Uint8Array) => void
  onControl: (c: AttachControl) => void
  /** the socket closed (code 1000 after an exit, 1011 after an error, 1008: too slow) */
  onClose: (code: number) => void
}

export interface Attachment {
  /** keyboard input bytes */
  input(b: Uint8Array<ArrayBuffer>): void
  resize(cols: number, rows: number): void
  /** detaches (the session keeps running) */
  close(): void
}

/** Attaches to a Longarm session's terminal: binary frames out and in, JSON control. */
export function attachSession(worker: string, name: string, cols: number, rows: number, h: AttachHandlers): Attachment {
  const ws = new WebSocket(attachUrl(worker, name, cols, rows))
  ws.binaryType = 'arraybuffer'
  let closed = false
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      try {
        h.onControl(JSON.parse(ev.data) as AttachControl)
      } catch {
        /* ignore malformed control frames */
      }
      return
    }
    h.onData(new Uint8Array(ev.data as ArrayBuffer))
  }
  ws.onclose = (ev) => {
    if (closed) return
    closed = true
    h.onClose(ev.code)
  }
  const sendText = (v: unknown) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify(v))
  return {
    input: (b) => ws.readyState === WebSocket.OPEN && ws.send(b),
    resize: (cols, rows) => sendText({ type: 'resize', cols, rows }),
    close: () => {
      if (closed) return
      closed = true
      sendText({ type: 'detach' })
      ws.close(1000)
    },
  }
}
