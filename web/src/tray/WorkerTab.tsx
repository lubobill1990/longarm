import { useCallback, useEffect, useState } from 'react'
import { longarmdApi, trayApi, type TrayInfo } from '../api/local'
import type { Job } from '../api/types'
import { Bar } from '../components/charts'
import { ErrorBox, FirewallWarning, LogView, StatePill, useAction, usePoll } from '../components/common'
import { fmtAgo, fmtTime, gb, isActive, memPct, since } from '../lib/format'

/** longarm-tray's worker tab: this machine's longarmd through its local API. */
export function WorkerTab({ info }: { info: TrayInfo }) {
  if (!info.worker.enrolled) return <JoinForm />
  if (!info.worker.running)
    return (
      <div className="error-box">
        longarmd is not running{info.worker.error ? `: ${info.worker.error}` : ''}. Start the service (Windows: <span className="mono">longarmd service start</span>).
      </div>
    )
  return <WorkerStatusView />
}

function WorkerStatusView() {
  const st = usePoll(longarmdApi.status, 2000)
  const jobs = usePoll(() => longarmdApi.jobs(30), 3000)
  const { run, busy } = useAction()
  const [open, setOpen] = useState<Job>()
  const [maxJobs, setMaxJobs] = useState('')
  const [reserve, setReserve] = useState('')
  const s = st.data
  useEffect(() => {
    if (s?.limits && maxJobs === '' && reserve === '') {
      setMaxJobs(String(s.limits.max_jobs ?? 0))
      setReserve(String(s.limits.reserved_mem_gb ?? 0))
    }
  }, [s]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!s) return <ErrorBox error={st.error} />
  const draining = !!s.state?.draining
  const m = s.metrics
  return (
    <div>
      <ErrorBox error={st.error} />
      <FirewallWarning fw={s.firewall} />
      <section className="wcard">
        <div className="head">
          <span className="dot" style={{ background: s.hub?.connected || !s.hub?.address ? 'var(--green)' : 'var(--red)' }} />
          <span className="name">{s.name}</span>
          <span className="sub">
            longarmd {s.version} · {s.os}/{s.arch}
          </span>
          <span className="actions">
            <button
              data-testid="drain-toggle"
              disabled={busy}
              onClick={() => run(() => longarmdApi.drain(!draining, draining ? '' : 'paused in longarm-tray'), draining ? 'accepting work' : 'draining').then(st.reload)}
            >
              {draining ? 'resume' : 'pause (drain)'}
            </button>
          </span>
        </div>
        <div className="small" data-testid="worker-state">
          {draining ? <span className="amber">draining: {s.state?.reason}</span> : <span className="green">accepting work</span>} · {s.jobs_running ?? 0} jobs, {s.sessions_running ?? 0} sessions
        </div>
        <div className="small muted" style={{ marginTop: 4 }}>
          hub {s.hub?.address || 'not configured'}{' '}
          {s.hub?.address && (s.hub.connected ? `connected (${s.hub.node_state}) ${fmtAgo(since(s.hub.since))}` : `disconnected ${s.hub.error ?? ''}`)}
        </div>
        <div className="small muted">listening {(s.endpoints ?? []).join(' ')}</div>
        {s.cert_not_after && <div className="small muted">certificate valid until {fmtTime(s.cert_not_after)}</div>}
        {m && (
          <div style={{ marginTop: 6 }}>
            <Bar label="CPU" pct={m.cpu_percent ?? 0} value={`${(m.cpu_percent ?? 0).toFixed(0)}%`} />
            <Bar label="MEM" pct={memPct(m)} value={`${gb(m.mem_used_bytes).toFixed(1)} / ${gb(m.mem_total_bytes).toFixed(0)} GB`} />
          </div>
        )}
      </section>
      <section>
        <h3>Local limits</h3>
        <form
          className="row wrap"
          onSubmit={(e) => {
            e.preventDefault()
            run(() => longarmdApi.limits({ max_jobs: Number(maxJobs) || 0, reserved_mem_gb: Number(reserve) || 0 }), 'limits saved').then(st.reload)
          }}
        >
          <label className="row small">
            max jobs <input aria-label="max jobs" style={{ width: 60 }} type="number" min={0} value={maxJobs} onChange={(e) => setMaxJobs(e.target.value)} />
          </label>
          <label className="row small">
            keep free <input aria-label="reserved memory" style={{ width: 70 }} type="number" min={0} step="0.5" value={reserve} onChange={(e) => setReserve(e.target.value)} /> GB
          </label>
          <button disabled={busy}>Save</button>
        </form>
        <div className="muted small">0 = no limit. The hub's scheduler honours them (longarmd limits).</div>
      </section>
      <section>
        <h3>Toolchain</h3>
        <div className="row small">
          {s.toolchain && s.toolchain.state !== 'none' ? (
            <>
              <span>
                {s.toolchain.profile} r{s.toolchain.revision}
              </span>
              <StatePill state={s.toolchain.state} />
              <span className="red">{s.toolchain.error}</span>
            </>
          ) : (
            <span className="muted">no profile assigned</span>
          )}
          <span className="spacer" />
          <button disabled={busy} onClick={() => run(() => longarmdApi.toolchainApply(), 'toolchain update requested')}>
            Update now
          </button>
        </div>
        <div className="small mono muted">
          {Object.entries(s.toolchain?.versions ?? {})
            .map(([k, v]) => `${k} ${v || '✗'}`)
            .join(' · ')}
        </div>
      </section>
      <section>
        <h3>Jobs on this worker</h3>
        <ErrorBox error={jobs.error} />
        <table className="grid small" data-testid="worker-jobs">
          <tbody>
            {(jobs.data ?? []).map((j) => (
              <tr key={j.spec.id} onClick={() => setOpen(j)} style={{ cursor: 'pointer' }}>
                <td className="mono">{j.spec.name || j.spec.id.slice(-8)}</td>
                <td>
                  <StatePill state={j.state} />
                </td>
                <td className="muted">{j.spec.submitted_by}</td>
                <td className="muted nowrap">{fmtTime(j.created_at)}</td>
                <td>
                  {isActive(j.state) && (
                    <button
                      className="danger"
                      disabled={busy}
                      onClick={(e) => {
                        e.stopPropagation()
                        run(() => longarmdApi.kill(j.spec.id), 'killed').then(jobs.reload)
                      }}
                    >
                      kill
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {open && <WorkerJobLogs key={open.spec.id} id={open.spec.id} />}
      </section>
      <section>
        <h3>Fleet membership</h3>
        <button
          className="danger"
          disabled={busy}
          onClick={() =>
            confirm('Leave the fleet? The hub revokes this worker and longarmd stops. Running jobs keep running.') &&
            run(() => longarmdApi.leave('left in longarm-tray'), (r) => r.message ?? 'left')
          }
        >
          Leave the fleet
        </button>
      </section>
    </div>
  )
}

function WorkerJobLogs({ id }: { id: string }) {
  const open = useCallback((onChunk: Parameters<typeof longarmdApi.logs>[1], onDone: Parameters<typeof longarmdApi.logs>[2]) => longarmdApi.logs(id, onChunk, onDone), [id])
  return <LogView open={open} />
}

/** Not enrolled yet: join with a code from the hub panel (longarmd join). */
function JoinForm() {
  const { run, busy } = useAction()
  const [hub, setHub] = useState('')
  const [code, setCode] = useState('')
  const [fp, setFp] = useState('')
  const [name, setName] = useState('')
  const [out, setOut] = useState<string>()
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault()
        const r = await run(() => trayApi.join(hub, code, fp, name))
        if (r) setOut(r.output)
      }}
    >
      <h3>Join a fleet</h3>
      <div className="form-grid">
        <label>hub</label>
        <input aria-label="hub address" placeholder="hub.example.com:7443" value={hub} onChange={(e) => setHub(e.target.value)} />
        <label>join code</label>
        <input aria-label="join code" placeholder="K7P-Q2M" value={code} onChange={(e) => setCode(e.target.value)} />
        <label>CA fingerprint</label>
        <input aria-label="CA fingerprint" placeholder="shown in the hub panel (recommended)" value={fp} onChange={(e) => setFp(e.target.value)} />
        <label>name</label>
        <input aria-label="worker name" placeholder="default: hostname" value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <p>
        <button className="primary" disabled={busy || !hub || !code}>
          Join
        </button>
      </p>
      {out && <pre className="logview">{out}</pre>}
      <p className="muted small">Runs longarmd join; the worker then waits for an admin's approval in the hub panel.</p>
    </form>
  )
}
