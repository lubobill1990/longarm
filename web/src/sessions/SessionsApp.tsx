import { useCallback, useEffect, useRef, useState } from 'react'
import { longarmApi } from '../api/local'
import { sessionsApi, type Agent, type AgentSession, type AgentSessionList, type AgentState } from '../api/sessions'
import { ErrorBox, Toasts, useAction } from '../components/common'
import { installAppBehaviour } from '../tray/appShell'
import { useTheme } from '../tray/theme'
import { useNow } from '../tray/PoolTab'
import { ConversationView } from './ConversationView'
import { NewSessionDialog, startedItem } from './NewSessionDialog'
import { AGENT_LABEL, SessionList } from './SessionList'
import { SessionTabs, type Tab } from './SessionTabs'
import { SteerBox } from './SteerBox'
import { TerminalView } from './TerminalView'
import '../tray/tokens.css'
import '../tray/tray.css'
import './sessions.css'

/** How often the list is refreshed. */
const LIST_MS = 5000
/** At most this many tabs keep their terminal's WebSocket. */
export const MAX_CONNECTED = 6

export interface HashTarget {
  worker?: string
  agent?: Agent
  key?: string
}

/** #worker=..&agent=..&key=.. (the tray's /trayapi/sessions/open). */
export function parseHash(hash: string): HashTarget {
  const p = new URLSearchParams(hash.replace(/^#/, ''))
  const agent = p.get('agent')
  return {
    worker: p.get('worker') || undefined,
    agent: agent === 'claude' || agent === 'copilot' ? agent : undefined,
    key: p.get('key') || undefined,
  }
}

/** The same session: the same key, or the same Longarm session (its key changes when the transcript appears). */
export function sameSession(a: AgentSession, b: AgentSession): boolean {
  if (a.key === b.key) return true
  return a.worker === b.worker && !!a.session && a.session === b.session && a.where === 'longarm' && b.where === 'longarm'
}

/** The tabs whose terminal stays connected: the active one and the most recently shown others. */
export function connectedTabs(recent: string[], active?: string, max = MAX_CONNECTED): Set<string> {
  const ids = active ? [active, ...recent.filter((id) => id !== active)] : recent
  return new Set(ids.slice(0, max))
}

function useHash(): HashTarget & { seq: number } {
  const [h, setH] = useState(() => ({ ...parseHash(location.hash), seq: 0 }))
  useEffect(() => {
    const on = () => setH((old) => ({ ...parseHash(location.hash), seq: old.seq + 1 }))
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return h
}

/**
 * The Sessions window (/sessions): the Claude Code / Copilot CLI sessions
 * of the user's workers on the left, one tab per opened session on the
 * right - its terminal (live, in a Longarm session) or its conversation,
 * and a steer box.
 */
export function SessionsApp() {
  useTheme()
  useEffect(() => installAppBehaviour(), [])
  return (
    <Toasts>
      <Sessions />
    </Toasts>
  )
}

function Sessions() {
  const target = useHash()
  const now = useNow()
  const [scope, setScope] = useState<{ worker?: string; agent?: Agent }>({ worker: target.worker, agent: target.agent })
  const [all, setAll] = useState(false)
  const [list, setList] = useState<AgentSessionList>()
  const [error, setError] = useState<string>()
  const [filter, setFilter] = useState('')
  const [tabs, setTabs] = useState<Tab[]>([])
  const [active, setActive] = useState<string>()
  const [recent, setRecent] = useState<string[]>([])
  const [dialog, setDialog] = useState(false)
  const pendingKey = useRef(target.key)
  const activeRef = useRef(active)
  activeRef.current = active
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs

  // a new hash: its filter, and the session it names once the list has it
  useEffect(() => {
    setScope({ worker: target.worker, agent: target.agent })
    pendingKey.current = target.key
  }, [target.seq]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    let gone = false
    const load = () =>
      sessionsApi
        .list({ ...scope, all })
        .then((l) => {
          if (gone) return
          setList(l)
          setError(undefined)
        })
        .catch((e) => !gone && setError(e instanceof Error ? e.message : String(e)))
    load()
    const t = setInterval(load, LIST_MS)
    return () => {
      gone = true
      clearInterval(t)
    }
  }, [scope.worker, scope.agent, all]) // eslint-disable-line react-hooks/exhaustive-deps

  const select = useCallback((id: string) => {
    setActive(id)
    setRecent((r) => [id, ...r.filter((x) => x !== id)])
    setTabs((ts) => ts.map((t) => (t.id === id && t.badge ? { ...t, badge: false } : t)))
  }, [])

  const open = useCallback(
    (s: AgentSession) => {
      const have = tabsRef.current.find((t) => sameSession(t.item, s))
      if (!have) setTabs((ts) => [...ts, { id: s.key, item: s }])
      select(have?.id ?? s.key)
    },
    [select],
  )

  const close = (id: string) => {
    const ts = tabsRef.current
    const i = ts.findIndex((t) => t.id === id)
    const rest = ts.filter((t) => t.id !== id)
    setTabs(rest)
    setRecent((r) => r.filter((x) => x !== id))
    if (id === active) setActive(rest[Math.min(i, rest.length - 1)]?.id)
  }

  // the list's rows refresh the open tabs; a session turning "waiting" in the background gets a badge
  useEffect(() => {
    const items = list?.items ?? []
    setTabs((ts) =>
      ts.map((t) => {
        const s = items.find((x) => sameSession(t.item, x))
        if (!s) return t
        const badge = t.badge || (t.id !== activeRef.current && s.state === 'waiting' && t.item.state !== 'waiting')
        return { ...t, item: s, badge }
      }),
    )
    const want = pendingKey.current
    const hit = want && items.find((x) => x.key === want)
    if (hit) {
      pendingKey.current = undefined
      open(hit)
    }
  }, [list, open])

  // output in a background tab, or a state from its conversation
  const mark = useCallback((id: string, c: { badge?: boolean; state?: AgentState }) => {
    setTabs((ts) => {
      const t = ts.find((x) => x.id === id)
      if (!t) return ts
      const state = c.state ?? t.item.state
      const bg = id !== activeRef.current
      const badge = !!t.badge || (bg && (!!c.badge || (state === 'waiting' && t.item.state !== 'waiting')))
      if (badge === !!t.badge && state === t.item.state) return ts
      return ts.map((x) => (x === t ? { ...t, badge, item: { ...t.item, state } } : x))
    })
  }, [])

  const connected = connectedTabs(recent, active)
  const items = list?.items ?? []
  const workers = [...new Set([...(scope.worker ? [scope.worker] : []), ...items.map((s) => s.worker)])].sort()
  return (
    <div className="sessions" data-testid="sessions">
      <aside className="sessions-side">
        <div className="sessions-head">
          <span className="brand-mark" aria-hidden="true" />
          <span className="title">Sessions</span>
          <span className="spacer" />
          <button className="sm primary" onClick={() => setDialog(true)}>
            New session
          </button>
        </div>
        <div className="sessions-scope">
          {scope.worker && (
            <span className="pill chip" data-testid="scope-worker">
              {scope.worker}
              <button className="link" aria-label="all workers" onClick={() => setScope((s) => ({ ...s, worker: undefined }))}>
                ×
              </button>
            </span>
          )}
          {scope.agent && (
            <span className="pill chip" data-testid="scope-agent">
              {AGENT_LABEL[scope.agent]}
              <button className="link" aria-label="all agents" onClick={() => setScope((s) => ({ ...s, agent: undefined }))}>
                ×
              </button>
            </span>
          )}
          <label className="small muted sessions-all">
            <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> ended too
          </label>
        </div>
        <ErrorBox error={error} />
        {!list && !error ? (
          <div className="muted small slist-empty">loading sessions…</div>
        ) : (
          <SessionList items={items} unreachable={list?.unreachable} filter={filter} onFilter={setFilter} selected={tabs.find((t) => t.id === active)?.item.key} onOpen={open} now={now} />
        )}
      </aside>
      <main className="sessions-main">
        <SessionTabs tabs={tabs} active={active} onSelect={select} onClose={close} />
        {tabs.length === 0 && <div className="sessions-empty muted">Pick a session on the left, or start a new one.</div>}
        {tabs.map((t) => (
          <TabPanel key={t.id} tab={t} visible={t.id === active} connected={connected.has(t.id)} onOpen={open} onMark={(c) => mark(t.id, c)} />
        ))}
      </main>
      {dialog && (
        <NewSessionDialog
          workers={workers}
          initial={scope}
          onClose={() => setDialog(false)}
          onStarted={(s) => {
            setDialog(false)
            open(s)
          }}
        />
      )}
    </div>
  )
}

/**
 * One session's tab: Terminal | Conversation for a Longarm session (the
 * terminal first) with the steer box; the conversation only, with
 * "Continue here", for an agent outside Longarm or a transcript.
 */
function TabPanel({
  tab,
  visible,
  connected,
  onOpen,
  onMark,
}: {
  tab: Tab
  visible: boolean
  connected: boolean
  onOpen: (s: AgentSession) => void
  onMark: (c: { badge?: boolean; state?: AgentState }) => void
}) {
  const s = tab.item
  const inLongarm = s.where === 'longarm' && !!s.session
  const [view, setView] = useState<'terminal' | 'conversation'>(inLongarm ? 'terminal' : 'conversation')
  const [ended, setEnded] = useState(false)
  const { run, busy } = useAction()
  const cont = () =>
    run(async () => {
      const agent = s.agent as Agent
      const started = await sessionsApi.start(s.worker, { agent, resume: s.id, fork: s.where === 'desktop', cwd: s.project_dir })
      onOpen(startedItem(s.worker, agent, started, s.id))
    })
  const showTerm = inLongarm && view === 'terminal'
  return (
    <section className="tabpanel" role="tabpanel" hidden={!visible} data-testid={`panel-${tab.id}`}>
      <div className="tabpanel-bar">
        {inLongarm && (
          <div className="seg" role="group" aria-label="view">
            <button className={`sm ${view === 'terminal' ? 'on' : ''}`} aria-pressed={view === 'terminal'} onClick={() => setView('terminal')}>
              Terminal
            </button>
            <button className={`sm ${view === 'conversation' ? 'on' : ''}`} aria-pressed={view === 'conversation'} onClick={() => setView('conversation')}>
              Conversation
            </button>
          </div>
        )}
        <span className="small muted tabpanel-where">
          {s.worker}
          {s.session ? ` · session ${s.session}` : s.where === 'desktop' ? ' · outside Longarm (read-only)' : ' · transcript'}
          {s.project_dir ? ` · ${s.project_dir}` : ''}
        </span>
        <span className="spacer" />
        {!inLongarm && s.agent && s.id && (
          <button
            className="sm primary"
            disabled={busy}
            data-testid="continue-here"
            title={s.where === 'desktop' ? 'resume a copy (--fork-session) in a Longarm session: the original stays open' : 'resume it in a Longarm session'}
            onClick={cont}
          >
            Continue here
          </button>
        )}
      </div>
      <div className="tabpanel-body">
        {inLongarm && connected && (
          <div className="tabpanel-term" hidden={!showTerm}>
            <TerminalView
              worker={s.worker}
              session={s.session!}
              visible={visible && showTerm}
              onOutput={() => !visible && !tab.badge && onMark({ badge: true })}
              onExit={() => {
                setEnded(true)
                onMark({ state: 'ended' })
              }}
            />
          </div>
        )}
        {inLongarm && !connected && showTerm && <div className="sessions-empty muted">detached (re-attaches when shown)</div>}
        {visible && !showTerm && <ConversationView item={s} onState={(state) => onMark({ state })} />}
      </div>
      {inLongarm && <SteerBox worker={s.worker} session={s.session!} disabled={ended || s.state === 'ended'} />}
    </section>
  )
}
