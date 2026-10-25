import { useCallback, useState } from 'react'
import { longarmApi, trayApi } from '../api/local'
import type { FleetJob, PoolView } from '../api/types'
import { ErrorBox, LogView, StatePill, useAction, usePoll } from '../components/common'
import { fmtTime, isActive } from '../lib/format'

/** My jobs (submitted by this client), with logs and kill. */
export function ClientJobs() {
  const [all, setAll] = useState(false)
  const jobs = usePoll(() => longarmApi.jobs({ mine: !all, limit: 50 }), 3000, [all])
  const [open, setOpen] = useState<FleetJob>()
  const { run, busy } = useAction()
  return (
    <div>
      <div className="row" style={{ marginBottom: 6 }}>
        <label className="row small">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> everyone's jobs
        </label>
        <span className="spacer" />
        <span className="muted small">longarm jobs · longarm logs · longarm kill</span>
      </div>
      <ErrorBox error={jobs.error} />
      <table className="grid small" data-testid="my-jobs">
        <tbody>
          {(jobs.data ?? []).map((j) => (
            <tr key={`${j.worker}/${j.job.spec.id}`} onClick={() => setOpen(j)} style={{ cursor: 'pointer' }}>
              <td className="mono">{j.job.spec.name || j.job.spec.id.slice(-8)}</td>
              <td>{j.worker}</td>
              <td>
                <StatePill state={j.job.state} />
                {j.job.has_exit_code ? ` ${j.job.exit_code ?? 0}` : ''}
              </td>
              <td className="muted nowrap">{fmtTime(j.job.created_at)}</td>
              <td>
                {isActive(j.job.state) && (
                  <button
                    className="danger"
                    disabled={busy}
                    onClick={(e) => {
                      e.stopPropagation()
                      run(() => longarmApi.kill(j.job.spec.id, j.worker), 'killed').then(jobs.reload)
                    }}
                  >
                    kill
                  </button>
                )}
              </td>
            </tr>
          ))}
          {(jobs.data ?? []).length === 0 && (
            <tr>
              <td className="notice">no jobs</td>
            </tr>
          )}
        </tbody>
      </table>
      {open && <JobLogs key={open.job.spec.id} j={open} />}
    </div>
  )
}

function JobLogs({ j }: { j: FleetJob }) {
  const openStream = useCallback(
    (onChunk: Parameters<typeof longarmApi.logs>[2], onDone: Parameters<typeof longarmApi.logs>[3]) => longarmApi.logs(j.job.spec.id, j.worker, onChunk, onDone),
    [j.job.spec.id, j.worker],
  )
  return (
    <section>
      <h3>
        {j.worker} · <span className="mono">{(j.job.spec.argv ?? []).join(' ').slice(0, 80)}</span>
      </h3>
      <LogView open={openStream} />
    </section>
  )
}

/** Port forwards hosted by longarm daemon (longarm forward --bg / --list / --stop). */
export function Forwards({ workers }: { workers: string[] }) {
  const fw = usePoll(longarmApi.forwards, 3000)
  const { run, busy } = useAction()
  const [worker, setWorker] = useState('')
  const [target, setTarget] = useState('')
  return (
    <div>
      <form
        className="row wrap"
        onSubmit={async (e) => {
          e.preventDefault()
          const r = await run(() => longarmApi.startForward(worker || workers[0], target), (f) => `forwarding ${f.local_addr}`)
          if (r) {
            setTarget('')
            fw.reload()
          }
        }}
      >
        <select aria-label="forward worker" value={worker || workers[0] || ''} onChange={(e) => setWorker(e.target.value)}>
          {workers.map((w) => (
            <option key={w}>{w}</option>
          ))}
        </select>
        <input aria-label="target port" placeholder="port or host:port on the worker" value={target} onChange={(e) => setTarget(e.target.value)} style={{ flex: 1 }} />
        <button className="primary" disabled={busy || !target || !workers.length}>
          Forward
        </button>
      </form>
      <ErrorBox error={fw.error} />
      <table className="grid small" style={{ marginTop: 8 }} data-testid="forwards">
        <tbody>
          {(fw.data ?? []).map((f) => (
            <tr key={f.local_addr}>
              <td>
                <button className="link" onClick={() => trayApi.open(f.url ?? '')}>
                  {f.local_addr}
                </button>
              </td>
              <td>
                → {f.worker}:{f.target}
              </td>
              <td className="muted">{f.connections ?? 0} conns</td>
              <td className="red small">{f.error}</td>
              <td>
                <button disabled={busy} onClick={() => run(() => longarmApi.stopForward(f.local_addr ?? ''), 'stopped').then(fw.reload)}>
                  stop
                </button>
              </td>
            </tr>
          ))}
          {(fw.data ?? []).length === 0 && (
            <tr>
              <td className="notice">no forwards</td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

const POLICIES: [string, string][] = [
  ['race', 'race: dial every leg, first mTLS handshake wins (sticky winner)'],
  ['tailscale', 'prefer tailscale'],
  ['longnet', 'prefer longnet (hub relay + hole punching)'],
  ['tcp', 'prefer plain tcp'],
]

/** Transport policy (longarm transport). */
export function Settings({ pool }: { pool?: PoolView }) {
  const s = usePoll(longarmApi.settings, 0)
  const { run, busy } = useAction()
  const [choice, setChoice] = useState<string>()
  const current = choice ?? s.data?.transport ?? 'race'
  return (
    <div>
      <h3>Transport policy</h3>
      {POLICIES.map(([b, label]) => (
        <label key={b} className="row" style={{ margin: '4px 0' }}>
          <input
            type="radio"
            name="transport"
            value={b}
            checked={current === b}
            disabled={busy}
            onChange={() => {
              setChoice(b) // optimistic; the daemon's answer follows
              run(() => longarmApi.setTransport(b), `transport ${b}`).then((r) => setChoice(r?.transport ?? s.data?.transport))
            }}
          />
          {label}
        </label>
      ))}
      <p className="muted small">CLI: longarm transport {current}</p>
      <h3>This client</h3>
      <div className="small">
        <div>identity {pool?.identity}</div>
        <div>
          hub {pool?.hub?.address || '—'} {pool?.hub?.address ? (pool.hub.connected ? '(connected)' : `(down: ${pool.hub.error ?? ''})`) : ''}
        </div>
      </div>
    </div>
  )
}
