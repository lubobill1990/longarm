import type { AgentSession } from '../api/sessions'
import { fmtAgo, since } from '../lib/format'

export const AGENT_LABEL: Record<string, string> = { claude: 'Claude Code', copilot: 'Copilot CLI' }

/** The state dot: ● working, ◐ waiting, ○ ended, ◇ a desktop process (outside Longarm). */
export function stateMark(s: AgentSession): { mark: string; cls: string; label: string } {
  if (s.where === 'desktop') return { mark: '◇', cls: 'desktop', label: `outside Longarm, ${s.state}` }
  if (s.state === 'waiting') return { mark: '◐', cls: 'waiting', label: 'waiting for input' }
  if (s.state === 'ended') return { mark: '○', cls: 'ended', label: 'ended' }
  return { mark: '●', cls: s.state, label: s.state }
}

/** Whether a session matches the filter box (title, worker, directory, session name, agent). */
export function matches(s: AgentSession, q: string): boolean {
  const t = q.trim().toLowerCase()
  if (!t) return true
  return [s.title, s.worker, s.project_dir, s.session, s.agent && AGENT_LABEL[s.agent], s.id].some((v) => v?.toLowerCase().includes(t))
}

/** Sessions by worker, workers in name order; the list's own order inside. */
export function groupByWorker(items: AgentSession[]): [string, AgentSession[]][] {
  const m = new Map<string, AgentSession[]>()
  for (const s of items) m.set(s.worker, [...(m.get(s.worker) ?? []), s])
  return [...m.entries()].sort(([a], [b]) => a.localeCompare(b))
}

export function AgentIcon({ agent }: { agent: string }) {
  if (!agent) return <span className="agent-icon none" aria-hidden="true" />
  return (
    <span className={`agent-icon ${agent}`} title={AGENT_LABEL[agent] ?? agent} aria-hidden="true">
      {agent === 'claude' ? 'C' : agent === 'copilot' ? 'P' : '?'}
    </span>
  )
}

/** The left column: the sessions grouped by worker, a filter box, the selected one highlighted. */
export function SessionList({
  items,
  unreachable,
  filter,
  onFilter,
  selected,
  onOpen,
  now = Date.now(),
}: {
  items: AgentSession[]
  unreachable?: string[]
  filter: string
  onFilter: (q: string) => void
  selected?: string
  onOpen: (s: AgentSession) => void
  now?: number
}) {
  const groups = groupByWorker(items.filter((s) => matches(s, filter)))
  return (
    <div className="slist" data-testid="session-list">
      <input className="slist-filter" type="search" placeholder="Filter sessions" aria-label="filter sessions" value={filter} onChange={(e) => onFilter(e.target.value)} />
      <div className="slist-body">
        {groups.length === 0 && <div className="muted small slist-empty">{items.length ? 'Nothing matches the filter.' : 'No sessions.'}</div>}
        {groups.map(([worker, list]) => (
          <div key={worker} className="slist-group" data-testid={`group-${worker}`}>
            <div className="slist-worker">{worker}</div>
            {list.map((s) => {
              const st = stateMark(s)
              const t = s.last_activity || s.modified
              return (
                <button
                  key={s.key}
                  className={`slist-item ${selected === s.key ? 'active' : ''}`}
                  data-testid={`session-${s.key}`}
                  data-state={st.cls}
                  aria-current={selected === s.key || undefined}
                  title={[s.title, s.project_dir, s.session && `Longarm session ${s.session}`].filter(Boolean).join('\n')}
                  onClick={() => onOpen(s)}
                >
                  <span className={`sdot ${st.cls}`} aria-label={st.label} role="img">
                    {st.mark}
                  </span>
                  <AgentIcon agent={s.agent} />
                  <span className="slist-title">{s.title || s.session || s.id || '(untitled)'}</span>
                  <span className="slist-time muted small">{t ? fmtAgo(since(t, now)) : ''}</span>
                </button>
              )
            })}
          </div>
        ))}
        {unreachable && unreachable.length > 0 && (
          <div className="small amber slist-unreachable" data-testid="unreachable">
            unreachable: {unreachable.join(', ')}
          </div>
        )}
      </div>
    </div>
  )
}
