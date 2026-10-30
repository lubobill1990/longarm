import { useState, type FormEvent } from 'react'
import { sessionsApi, type Agent, type AgentSession, type LongarmSession } from '../api/sessions'

/** The list row for a session just started (until the list has it). */
export function startedItem(worker: string, agent: Agent, s: LongarmSession, resume = ''): AgentSession {
  return {
    key: `${worker}/session/${s.name}`,
    worker,
    agent,
    id: resume,
    title: s.name,
    project_dir: s.dir || s.cwd,
    where: 'longarm',
    session: s.name,
    state: 'working',
    last_activity: s.started_at || s.created_at,
  }
}

/** "New session": claude / copilot (or --resume <id>) in a Longarm session on a worker. */
export function NewSessionDialog({
  workers,
  initial,
  onStarted,
  onClose,
}: {
  workers: string[]
  initial?: { worker?: string; agent?: Agent }
  onStarted: (item: AgentSession) => void
  onClose: () => void
}) {
  const [worker, setWorker] = useState(initial?.worker || workers[0] || '')
  const [agent, setAgent] = useState<Agent>(initial?.agent ?? 'claude')
  const [cwd, setCwd] = useState('')
  const [resume, setResume] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const s = await sessionsApi.start(worker.trim(), { agent, cwd: cwd.trim(), resume: resume.trim() })
      onStarted(startedItem(worker.trim(), agent, s, resume.trim()))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="sdialog-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <form className="sdialog" role="dialog" aria-label="New session" data-testid="new-session" onSubmit={submit} onKeyDown={(e) => e.key === 'Escape' && onClose()}>
        <h2>New session</h2>
        <label>
          <span>Worker</span>
          <input list="sessions-workers" value={worker} required autoFocus onChange={(e) => setWorker(e.target.value)} />
          <datalist id="sessions-workers">
            {workers.map((w) => (
              <option key={w} value={w} />
            ))}
          </datalist>
        </label>
        <label>
          <span>Agent</span>
          <select value={agent} onChange={(e) => setAgent(e.target.value as Agent)}>
            <option value="claude">Claude Code</option>
            <option value="copilot">Copilot CLI</option>
          </select>
        </label>
        <label>
          <span>Directory</span>
          <input value={cwd} placeholder="the home directory" onChange={(e) => setCwd(e.target.value)} />
        </label>
        <label>
          <span>Resume</span>
          <input value={resume} placeholder="a session id (optional)" onChange={(e) => setResume(e.target.value)} />
        </label>
        {error && <div className="error-box">{error}</div>}
        <div className="sdialog-actions">
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !worker.trim()}>
            {busy ? 'Starting…' : 'Start'}
          </button>
        </div>
      </form>
    </div>
  )
}
