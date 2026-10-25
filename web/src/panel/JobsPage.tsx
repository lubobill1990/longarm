import { useCallback, useEffect, useMemo, useState } from 'react'
import { hubApi } from '../api/hub'
import type { FleetJob } from '../api/types'
import { ErrorBox, LogView, StatePill, useAction, usePoll } from '../components/common'
import { fmtTime, isActive, stateName } from '../lib/format'
import { Link, navigate } from '../lib/router'

const STATES = ['', 'running', 'preparing', 'done', 'failed', 'killed', 'lost']

/** Live job index: /api/jobs plus SSE job events. */
function useLiveJobs(limit: number) {
  const [jobs, setJobs] = useState<Record<string, FleetJob>>({})
  const [error, setError] = useState<string>()
  useEffect(() => {
    let alive = true
    hubApi
      .jobs({ limit })
      .then((js) => alive && setJobs(Object.fromEntries(js.map((j) => [`${j.worker}/${j.job.spec.id}`, j]))))
      .catch((e) => setError(String(e.message ?? e)))
    const s = hubApi.events(['job'], (ev) => {
      const j = ev.job
      if (j) setJobs((old) => ({ ...old, [`${j.worker}/${j.job.spec.id}`]: j }))
    })
    return () => {
      alive = false
      s.close()
    }
  }, [limit])
  return { jobs, error }
}

export function JobsPage({ path }: { path: string }) {
  const id = path.split('/')[2]
  if (id) return <JobDetail id={decodeURIComponent(id)} />
  return <JobList />
}

function JobList() {
  const { jobs, error } = useLiveJobs(500)
  const nodes = usePoll(() => hubApi.nodes(false), 30000)
  const [worker, setWorker] = useState('')
  const [state, setState] = useState('')
  const [q, setQ] = useState('')
  const [showNew, setShowNew] = useState(false)
  const list = useMemo(() => {
    const ql = q.toLowerCase()
    return Object.values(jobs)
      .filter((j) => !worker || j.worker === worker)
      .filter((j) => !state || stateName(j.job.state) === state)
      .filter(
        (j) =>
          !ql ||
          j.job.spec.id.toLowerCase().includes(ql) ||
          (j.job.spec.name ?? '').toLowerCase().includes(ql) ||
          (j.job.spec.argv ?? []).join(' ').toLowerCase().includes(ql) ||
          (j.job.spec.submitted_by ?? '').toLowerCase().includes(ql),
      )
      .sort((a, b) => (b.job.created_at ?? '').localeCompare(a.job.created_at ?? ''))
      .slice(0, 300)
  }, [jobs, worker, state, q])
  return (
    <div>
      <div className="page-title">
        <h1>Jobs</h1>
        <select aria-label="worker filter" value={worker} onChange={(e) => setWorker(e.target.value)}>
          <option value="">all workers</option>
          {(nodes.data ?? []).map((n) => (
            <option key={n.name}>{n.name}</option>
          ))}
        </select>
        <select aria-label="state filter" value={state} onChange={(e) => setState(e.target.value)}>
          {STATES.map((s) => (
            <option key={s} value={s}>
              {s || 'any state'}
            </option>
          ))}
        </select>
        <input aria-label="search" placeholder="search id, command, user" value={q} onChange={(e) => setQ(e.target.value)} />
        <span className="spacer" />
        <button className="primary" onClick={() => setShowNew((v) => !v)}>
          New job
        </button>
      </div>
      <ErrorBox error={error} />
      {showNew && <NewJob workers={(nodes.data ?? []).filter((n) => n.online).map((n) => n.name)} onDone={() => setShowNew(false)} />}
      <table className="grid" data-testid="jobs">
        <thead>
          <tr>
            <th>job</th>
            <th>worker</th>
            <th>state</th>
            <th>exit</th>
            <th>by</th>
            <th>created</th>
            <th>command</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {list.map((j) => (
            <JobRow key={`${j.worker}/${j.job.spec.id}`} j={j} />
          ))}
          {list.length === 0 && (
            <tr>
              <td colSpan={8} className="notice">
                no jobs
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

function JobRow({ j }: { j: FleetJob }) {
  const { run, busy } = useAction()
  const s = j.job.spec
  return (
    <tr data-testid={`job-${s.id}`}>
      <td className="mono small">
        <Link to={`/jobs/${encodeURIComponent(s.id)}?worker=${encodeURIComponent(j.worker)}`}>{s.name || s.id.slice(-10)}</Link>
      </td>
      <td>{j.worker}</td>
      <td>
        <StatePill state={j.job.state} />
      </td>
      <td className="mono">{j.job.has_exit_code ? (j.job.exit_code ?? 0) : ''}</td>
      <td className="small muted">{s.submitted_by}</td>
      <td className="small muted nowrap">{fmtTime(j.job.created_at)}</td>
      <td className="mono small" style={{ maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={(s.argv ?? []).join(' ')}>
        {(s.argv ?? []).join(' ')}
      </td>
      <td>
        {isActive(j.job.state) && (
          <button className="danger" disabled={busy || !j.worker_online} onClick={() => run(() => hubApi.kill(s.id, j.worker), 'killed')}>
            Kill
          </button>
        )}
      </td>
    </tr>
  )
}

function NewJob({ workers, onDone }: { workers: string[]; onDone: () => void }) {
  const { run, busy } = useAction()
  const [worker, setWorker] = useState('auto')
  const [command, setCommand] = useState('')
  const [name, setName] = useState('')
  const [cwd, setCwd] = useState('')
  return (
    <form
      className="card"
      style={{ marginBottom: 12 }}
      onSubmit={async (e) => {
        e.preventDefault()
        const r = await run(() => hubApi.submit({ worker, command, name, cwd }), (j) => `submitted ${j.job.spec.id} on ${j.worker}`)
        if (r) {
          onDone()
          navigate(`/jobs/${encodeURIComponent(r.job.spec.id)}?worker=${encodeURIComponent(r.worker)}`)
        }
      }}
    >
      <div className="form-grid">
        <label>worker</label>
        <select aria-label="worker" value={worker} onChange={(e) => setWorker(e.target.value)}>
          <option value="auto">auto (scheduler)</option>
          {workers.map((w) => (
            <option key={w}>{w}</option>
          ))}
        </select>
        <label>command</label>
        <input aria-label="command" className="mono" placeholder='e.g. pnpm test   (quotes: "a b")' value={command} onChange={(e) => setCommand(e.target.value)} />
        <label>name</label>
        <input aria-label="name" placeholder="optional" value={name} onChange={(e) => setName(e.target.value)} />
        <label>directory</label>
        <input aria-label="directory" placeholder="optional, on the worker" value={cwd} onChange={(e) => setCwd(e.target.value)} />
      </div>
      <p className="muted small">
        Runs through the worker's hub session: no secrets, no repository upload. The worker must allow it (<span className="mono">[hub] remote_jobs = "all"</span>). CLI: <span className="mono">longarm admin run</span>.
      </p>
      <button className="primary" disabled={busy || !command.trim()}>
        Run
      </button>
    </form>
  )
}

function JobDetail({ id }: { id: string }) {
  const worker = new URLSearchParams(location.search).get('worker') ?? undefined
  const job = usePoll(() => hubApi.job(id, worker), 2000, [id, worker])
  const { run, busy } = useAction()
  const j = job.data
  const open = useCallback((onChunk: Parameters<typeof hubApi.logs>[2], onDone: Parameters<typeof hubApi.logs>[3]) => hubApi.logs(id, worker, onChunk, onDone), [id, worker])
  return (
    <div>
      <div className="page-title">
        <Link to="/jobs">← Jobs</Link>
        <h1 className="mono">{j?.job.spec.name || id}</h1>
        {j && <StatePill state={j.job.state} />}
        {j?.job.has_exit_code && <span className="mono">exit {j.job.exit_code ?? 0}</span>}
        <span className="spacer" />
        {j && isActive(j.job.state) && (
          <button className="danger" disabled={busy} onClick={() => run(() => hubApi.kill(id, j.worker), 'killed').then(() => job.reload())}>
            Kill
          </button>
        )}
      </div>
      <ErrorBox error={job.error} />
      {j && (
        <div className="card" style={{ marginBottom: 12 }}>
          <div className="form-grid small">
            <span className="muted">id</span>
            <span className="mono">{j.job.spec.id}</span>
            <span className="muted">worker</span>
            <span>{j.worker}</span>
            <span className="muted">command</span>
            <span className="mono">{(j.job.spec.argv ?? []).join(' ')}</span>
            <span className="muted">by</span>
            <span>{j.job.spec.submitted_by}</span>
            <span className="muted">created / ended</span>
            <span>
              {fmtTime(j.job.created_at)} / {fmtTime(j.job.ended_at)}
            </span>
            {j.job.reason && (
              <>
                <span className="muted">reason</span>
                <span>{j.job.reason}</span>
              </>
            )}
          </div>
        </div>
      )}
      <LogView open={open} />
    </div>
  )
}
